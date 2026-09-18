package app_test

// A Project is admitted while rest runs, by a lookup on a miss
// (miniship-cloud#548).
//
// ADR 0017's amendment settled the shape: on a call for a Project rest has not
// seen, rest asks the process holding the Database plugin — once, for that
// Project alone — for the Database's address and a credential, opens the pool
// and keeps it. Later calls ask nothing. Not a polled table, and not a push.
//
// Everything here is counted at two sockets and nowhere else: the answerer,
// which is a real HTTP server recording every lookup and which Project it
// named, and the countingDatabase of surface_test.go, which counts the
// connections that reached it. No log line and no internal counter is read.
//
// integration/postgres/admission asks a live Postgres the same questions, with
// Databases created while the process runs, real credentials and a real
// rotation.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/admission"
	"github.com/prest/prest/v2/app"
	"github.com/prest/prest/v2/config"
)

// theLookupKey is the derived key for the lookup route, as the api hands it to
// rest: base64url, and never the install's root. rest mints a service token
// with it on every call, so what travels is the token and not this.
var theLookupKey = base64.RawURLEncoding.EncodeToString([]byte(
	"a thirty-two byte key for a route"[:32]))

// heldUp is checkAssertion's question, asked here in Go: does the thing rest
// put in the header verify with the route's key, is it for this route, and
// does it say rest sent it. The MAC is checked before anything is parsed, as
// the api's own verifier does it (RFC 8725 §3.1).
func heldUp(t *testing.T, offered string) {
	t.Helper()
	parts := strings.Split(offered, ".")
	require.Len(t, parts, 3, "what rest sent is not a service token")

	key, err := base64.RawURLEncoding.DecodeString(theLookupKey)
	require.NoError(t, err)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	require.Equal(t, base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), parts[2],
		"the service token does not verify with the route's key")

	var head struct{ Alg, Typ string }
	var body struct {
		Sub, Aud string
		Exp, Iat int64
		Jti      string
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &head))
	raw, err = base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &body))

	require.Equal(t, "HS256", head.Alg)
	require.Equal(t, "miniship-link+jwt", head.Typ)
	require.Equal(t, "database", body.Aud, "the token is aimed at another route")
	require.Equal(t, "rest", body.Sub)
	require.NotEmpty(t, body.Jti)
	require.Equal(t, int64(300), body.Exp-body.Iat, "five minutes, and no longer")
}

// answerer stands where the process holding the Database plugin is: an HTTP
// server that records every lookup it was asked for, the credential it was
// asked with, and answers with whatever the test put there.
type answerer struct {
	server *httptest.Server

	mu          sync.Mutex
	asked       []string
	credentials []string
	answers     map[string]admission.Answer
	hold        chan struct{}
}

func newAnswerer(t *testing.T) *answerer {
	t.Helper()
	a := &answerer{answers: map[string]admission.Answer{}}
	a.server = httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.server.Close)
	return a
}

func (a *answerer) serve(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimPrefix(r.URL.Path, "/")
	a.mu.Lock()
	a.asked = append(a.asked, project)
	a.credentials = append(a.credentials, r.Header.Get(admission.InternalClientHeader))
	answer, known := a.answers[project]
	hold := a.hold
	a.mu.Unlock()

	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
	}
	if !known {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no such Project"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}

// put teaches the answerer about a Project, as renting a Database for it
// would. read is the login a read of it connects with.
func (a *answerer) put(project string, db *countingDatabase, password string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.answers[project] = admission.Answer{
		Project:  project,
		URL:      fmt.Sprintf("postgres://authenticator:%s@127.0.0.1:%d/%s?sslmode=disable", password, db.port, project),
		AnonRole: "app_anon",
	}
}

// holds makes every lookup wait until the returned function is called, which
// is an answerer that accepts and never answers.
func (a *answerer) holds() func() {
	hold := make(chan struct{})
	a.mu.Lock()
	a.hold = hold
	a.mu.Unlock()
	return func() { close(hold) }
}

func (a *answerer) asks() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.asked...)
}

func (a *answerer) asksFor(project string) int {
	n := 0
	for _, asked := range a.asks() {
		if asked == project {
			n++
		}
	}
	return n
}

// A Project rest was never given is admitted by one lookup, and the read that
// asked for it reaches that Project's Database and no other.
func TestAdmission_aProjectRestWasNotGivenIsLookedUpOnceAndAdmitted(t *testing.T) {
	answers := newAnswerer(t)
	given := newCountingDatabase(t)
	unseen := newCountingDatabase(t)
	answers.put(projectC, unseen, "not-a-real-password")

	rest := restWithAnswerer(t, answers, map[string]*countingDatabase{projectA: given})

	serve(rest.Handler, "GET /"+projectC+"/public/posts")

	require.Equal(t, []string{projectC}, answers.asks(),
		"one lookup, and it named that Project alone")
	unseen.requireAConnection(t)
	given.requireNoConnection(t)
	require.True(t, rest.Adapters.IsRegistered(projectC),
		"the admitted Project was not kept")
}

// A Project's second and later calls cause no lookup: the pool is kept, and
// the registry answers before the gate is reached.
func TestAdmission_anAdmittedProjectIsNotLookedUpAgain(t *testing.T) {
	answers := newAnswerer(t)
	unseen := newCountingDatabase(t)
	answers.put(projectC, unseen, "not-a-real-password")

	rest := restWithAnswerer(t, answers, nil)

	for range 20 {
		serve(rest.Handler, "GET /"+projectC+"/public/posts")
	}
	require.Equal(t, 1, answers.asksFor(projectC),
		"a Project already admitted was looked up again")
}

// Concurrent first calls for one Project cause one lookup between them, not
// one each: the Project's own permit holds them, and the call that wins
// admits it for all of them.
func TestAdmission_concurrentFirstCallsCauseOneLookup(t *testing.T) {
	answers := newAnswerer(t)
	unseen := newCountingDatabase(t)
	answers.put(projectC, unseen, "not-a-real-password")

	rest := restWithAnswerer(t, answers, nil)

	// The answerer is held until every call is inside the gate, so the window
	// this subject is about is open for all of them at once. Without that,
	// the first lookup returns before the rest arrive and they find the
	// Project admitted — which would pass whether or not there is a permit.
	release := answers.holds()

	var wg sync.WaitGroup
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			serve(rest.Handler, "GET /"+projectC+"/public/posts")
		}()
	}
	require.Eventually(t, func() bool { return answers.asksFor(projectC) > 0 },
		5*time.Second, 5*time.Millisecond, "no call reached the answerer")
	time.Sleep(200 * time.Millisecond)
	release()
	wg.Wait()

	require.Equal(t, 1, answers.asksFor(projectC),
		"twenty-four first calls for one Project were twenty-four lookups")
}

// A burst of calls for a name that has no Project is not a burst of lookups:
// the answer is kept for the window, and asked again only after it.
func TestAdmission_aBurstForANameWithNoProjectIsNotABurstOfLookups(t *testing.T) {
	answers := newAnswerer(t)
	rest := restWithAnswerer(t, answers, nil)

	for range 50 {
		rec := serve(rest.Handler, "GET /nosuchproject/public/posts")
		require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		require.NotContains(t, rec.Body.String(), "nosuchproject",
			"the answer quoted the caller's own name back")
	}
	require.Equal(t, 1, answers.asksFor("nosuchproject"),
		"fifty calls for a name with no Project were more than one lookup")

	// The window passes, and the answer is asked for again: it is kept for a
	// short time, not kept for the life of the process. A Project created a
	// moment after somebody first asked for it must become reachable.
	time.Sleep(200 * time.Millisecond)
	serve(rest.Handler, "GET /nosuchproject/public/posts")
	require.Equal(t, 2, answers.asksFor("nosuchproject"),
		"the refusal was kept past its window, so a new Project would never be admitted")
}

// With the answerer accepting and never answering, a Project that has a pool
// keeps answering, and one that has none is refused inside the timeout rather
// than left waiting.
func TestAdmission_withTheAnswererDown_warmProjectsAnswerAndAColdOneIsRefused(t *testing.T) {
	answers := newAnswerer(t)
	warm := newCountingDatabase(t)
	answers.put(projectB, warm, "not-a-real-password")

	rest := restWithAnswerer(t, answers, nil, func(c *config.AdmissionConf) {
		c.Timeout = 300 * time.Millisecond
	})

	// Warm: admitted while the answerer still answered.
	serve(rest.Handler, "GET /"+projectB+"/public/posts")
	warm.requireAConnection(t)

	release := answers.holds()
	defer release()

	// The warm Project is unaffected, and asks the answerer nothing.
	before := answers.asksFor(projectB)
	for range 5 {
		rec := serve(rest.Handler, "GET /"+projectB+"/public/posts")
		require.Equal(t, http.StatusBadGateway, rec.Code,
			"the warm Project stopped reaching its own Database: %s", rec.Body)
	}
	require.Equal(t, before, answers.asksFor(projectB),
		"a warm Project waited on the answerer")

	// The cold one is refused, and inside a bound rest set rather than
	// whenever the answerer gives up.
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- serve(rest.Handler, "GET /"+projectC+"/public/posts") }()
	select {
	case rec := <-done:
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	case <-time.After(10 * time.Second):
		t.Fatal("a cold Project hung on an answerer that never answered")
	}
}

// The lookup carries the internal credential, and an answerer that refuses the
// caller it came from admits nothing: rest reports a Project it could not find
// out about, never a Project that is not there.
func TestAdmission_theLookupCarriesTheInternalCredential(t *testing.T) {
	answers := newAnswerer(t)
	unseen := newCountingDatabase(t)
	answers.put(projectC, unseen, "not-a-real-password")

	rest := restWithAnswerer(t, answers, nil)
	serve(rest.Handler, "GET /"+projectC+"/public/posts")

	answers.mu.Lock()
	sent := append([]string(nil), answers.credentials...)
	answers.mu.Unlock()
	require.Len(t, sent, 1, "the lookup went without a credential")
	heldUp(t, sent[0])
	require.NotContains(t, sent[0], theLookupKey,
		"the route's key itself travelled, which is the thing a service token replaces")

	// And an answerer that refuses it is not a Project that does not exist.
	refusing := newAnswerer(t)
	refusing.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	refused := restWithAnswerer(t, refusing, nil)
	rec := serve(refused.Handler, "GET /"+projectC+"/public/posts")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
}

// A lookup answers for exactly one Project. An answerer that answers the
// question with another Project's Database is refused, rather than that
// Database being served under the name that was asked for.
func TestAdmission_anAnswerNamingAnotherProjectIsRefused(t *testing.T) {
	answers := newAnswerer(t)
	elsewhere := newCountingDatabase(t)
	answers.put(projectA, elsewhere, "not-a-real-password")

	// Asked about gamma, the answerer hands back alpha's entry.
	answers.mu.Lock()
	answers.answers[projectC] = answers.answers[projectA]
	answers.mu.Unlock()

	rest := restWithAnswerer(t, answers, nil)
	rec := serve(rest.Handler, "GET /"+projectC+"/public/posts")

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.False(t, rest.Adapters.IsRegistered(projectC),
		"another Project's Database was served under the name that was asked for")
	elsewhere.requireNoConnection(t)
}

// restWithAnswerer composes rest over the Projects it was given, with answers
// standing where the process holding the Database plugin is. The two windows
// are short so a subject can pass through them without sleeping for seconds.
func restWithAnswerer(t *testing.T, answers *answerer, given map[string]*countingDatabase, tune ...func(*config.AdmissionConf)) *app.App {
	t.Helper()
	cfg := &config.Prest{
		HTTPTimeout:   5,
		PGUser:        "authenticator",
		PGPass:        "not-a-real-password",
		PGSSLMode:     "disable",
		PGConnTimeout: 2,
		PGMaxOpenConn: 1,
		JSONAggType:   "jsonb_agg",
		SingleDB:      false,
		AccessConf:    config.AccessConf{Restrict: false},
		Admission: config.AdmissionConf{
			URL:         answers.server.URL,
			Key:         theLookupKey,
			Timeout:     2 * time.Second,
			Window:      150 * time.Millisecond,
			MaxProjects: 4000,
		},
	}
	for alias, db := range given {
		cfg.Databases = append(cfg.Databases, config.DatabaseConf{
			Alias:       alias,
			Host:        "127.0.0.1",
			Port:        db.port,
			User:        "authenticator",
			Pass:        "not-a-real-password",
			Database:    alias,
			SSL:         config.DatabaseSSLConf{Mode: "disable"},
			MaxOpenConn: 1,
			AnonRole:    "app_anon",
		})
	}
	for _, tune := range tune {
		tune(&cfg.Admission)
	}
	a, err := app.New(cfg)
	require.NoError(t, err)
	return a
}
