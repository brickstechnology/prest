// Package admission learns about a Project rest was not given, while rest
// runs (miniship-cloud#548).
//
// ADR 0017's amendment decided the shape and this package is it: on a call for
// a Project rest has not seen, rest asks the process holding the Database
// plugin — once, for that Project alone — for the Database's address and a
// credential, opens the pool and keeps it. Later calls ask nothing. It is not
// a polled table and it is not a push, and rest stores nothing: a restart
// forgets every Project and learns each one again on its first call.
//
// The four things in front of the lookup are the four Neon's own proxy has,
// which is the shared proxy ADR 0017 took this shape from
// (miniship-cloud's NEON-AS-AN-ENGINE.md §7.2, reading
// proxy/src/control_plane/client/cplane_proxy_v1.rs L409-474):
//
//   - a cache, so a Project already admitted costs no lookup. rest's is the
//     adapter registry itself, and unlike Neon's it has no time limit: Neon
//     caches for four minutes because the address it holds expires when the
//     compute suspends, and a Database's address does not expire under rest.
//   - a permit per Project, rechecked after it is acquired, so a burst of
//     first calls for one Project produces one lookup.
//   - a rate limit per Project, so a burst for a name that has no Project is
//     not a burst of lookups.
//   - an invalidation when a connection fails, so a credential that was
//     rotated is fetched again — once.
package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// InternalClientHeader is the header rest presents the internal credential in.
// The spelling is the one public #472 settled for a caller inside the install,
// and the lookup address answers no caller without it.
const InternalClientHeader = "x-miniship-api-client"

// Answer is what a lookup answers with, and it is the contract: the cloud half
// (miniship-cloud#552) implements the process that produces one, and rest
// cares about the shape and not about who rented the Database.
//
// URL carries the address and the credential together, in the spelling
// DATABASE_URL_<n> already uses, so a Project admitted while rest runs becomes
// exactly the registry entry a Project given at start-up is. AnonRole is the
// role a read of it becomes; there is no default, and an answer without one is
// an answer rest cannot serve.
type Answer struct {
	Project  string `json:"project"`
	URL      string `json:"url"`
	AnonRole string `json:"anonRole"`
}

var (
	// ErrNoSuchProject is the answerer saying there is no such Project. It is
	// an answer, so it is kept for the window rather than asked again.
	ErrNoSuchProject = errors.New("no such Project")
	// ErrUnavailable is rest not having got an answer: the answerer refused
	// rest's credential, timed out, could not be reached, or said something
	// rest cannot open a pool with. A Project already admitted is unaffected,
	// which is the property ADR 0017 accepted when it chose a lookup.
	ErrUnavailable = errors.New("the Project could not be admitted")
	// ErrTooSoon is a second forced lookup for one Project inside the window.
	// A credential that was rotated gets one fresh lookup; this is what stops
	// it being a loop.
	ErrTooSoon = errors.New("the Project was looked up too recently to look up again")
)

// Answerer is the process holding the Database plugin, as rest asks it.
type Answerer interface {
	// Lookup asks for one Project and no others. It answers
	// ErrNoSuchProject for a name that has no Project, and ErrUnavailable
	// for every way of not having asked or not having understood.
	Lookup(ctx context.Context, project string) (Answer, error)
}

// maxAnswer is how much of an answer rest reads. An answer is three short
// strings; anything larger is not one, and reading it would be rest holding a
// hostile answerer's stream open.
const maxAnswer = 64 << 10

// HTTPAnswerer asks the answerer over HTTP, at an address inside the install.
type HTTPAnswerer struct {
	base   string
	key    string
	client *http.Client
	now    func() time.Time
}

// NewHTTPAnswerer asks base for one Project at a time, proving itself with a
// service token minted from key — the derived key for the lookup route, never
// the install's root. assertion.go says what one of those is.
//
// The client follows no redirect: a redirect is another address, and rest's
// credential goes to the one address an operator configured and to no address
// an answer named. timeout bounds the whole exchange, so a Project with no
// pool is refused rather than left waiting on a socket that never answers.
func NewHTTPAnswerer(base, key string, timeout time.Duration) *HTTPAnswerer {
	return &HTTPAnswerer{
		base: strings.TrimRight(base, "/"),
		key:  key,
		now:  time.Now,
		client: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Lookup asks for project alone.
func (a *HTTPAnswerer) Lookup(ctx context.Context, project string) (Answer, error) {
	assertion, err := mintAssertion(a.key, a.now())
	if err != nil {
		return Answer{}, fmt.Errorf("%w: rest cannot prove itself at the lookup address", ErrUnavailable)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		a.base+"/"+url.PathEscape(project), nil)
	if err != nil {
		return Answer{}, fmt.Errorf("%w: %s", ErrUnavailable, "the lookup address is not an address")
	}
	req.Header.Set(InternalClientHeader, assertion)
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return Answer{}, fmt.Errorf("%w: the answerer did not answer", ErrUnavailable)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAnswer))
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Answer{}, ErrNoSuchProject
	case resp.StatusCode != http.StatusOK:
		return Answer{}, fmt.Errorf("%w: the answerer said %d", ErrUnavailable, resp.StatusCode)
	}

	var answer Answer
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAnswer)).Decode(&answer); err != nil {
		return Answer{}, fmt.Errorf("%w: the answer could not be read", ErrUnavailable)
	}
	if err := answer.validFor(project); err != nil {
		return Answer{}, err
	}
	return answer, nil
}

// validFor holds an answer to the one Project it was asked for. An answerer
// that names another Project is refused rather than admitted under the name
// that was asked for: a lookup answers for exactly one Project, and the
// credential in it opens that Project's Database alone.
func (a Answer) validFor(project string) error {
	switch {
	case a.Project != project:
		return fmt.Errorf("%w: the answer named another Project", ErrUnavailable)
	case a.URL == "":
		return fmt.Errorf("%w: the answer carried no address", ErrUnavailable)
	case a.AnonRole == "":
		return fmt.Errorf("%w: the answer named no anonymous role", ErrUnavailable)
	}
	return nil
}
