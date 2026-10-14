// Package projectkeys verifies a User's token against the key set of the
// Project the call is for (miniship-cloud#1302, public ADR 0077).
//
// It is Supabase's shape. Supabase's auth server publishes the keys it signs a
// User's token with at /auth/v1/.well-known/jwks.json, and PostgREST checks a
// User's token against them. One rest serves every Project here, so it reads
// each Project's key set from the auth api's key-set route for that Project,
// keeps it, and reads it again on a key id it does not hold. The Project is
// named in the path of that read, never in a header.
//
// What is cached, and for how long, is the auth api's own answer: it says a
// verifier may hold its key set for five minutes (`cache-control: max-age=300`),
// which is the revocation window a retired key keeps. A key id that is not in
// the set is read again at most once a window, so a stream of tokens naming
// keys that do not exist is not a stream of reads.
package projectkeys

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Role is the one role a User's token may name. The pool signs nothing else,
// and a key of a Project's pool that named another would be a key that opened
// that Project's rows past its row rules.
const Role = "authenticated"

// Audience is the audience a token minted under /auth/v1 names. A token minted
// for one App at /pool/<app>/… names that App and is not taken here.
const Audience = "authenticated"

// Algorithm is the one algorithm a User's token is signed with.
const Algorithm = jose.EdDSA

// MaxAge is how long a key set is kept before it is read again: the five
// minutes the auth api's answer says a verifier may hold it.
const MaxAge = 5 * time.Minute

// maxKeySet is how much of an answer is read. A key set is a few keys.
const maxKeySet = 64 << 10

var (
	// ErrRefused is a token that does not verify for this Project.
	ErrRefused = errors.New("the User's token is refused")
	// ErrUnavailable is a key set rest could not read, so no token of that
	// Project can be verified now.
	ErrUnavailable = errors.New("the Project's key set could not be read")
)

// Source reads and keeps each Project's key set.
type Source struct {
	address func(project string) string
	client  *http.Client
	window  time.Duration
	max     int
	now     func() time.Time

	mu   sync.Mutex
	sets map[string]*keySet
}

type keySet struct {
	mu      sync.Mutex
	keys    jose.JSONWebKeySet
	fetched time.Time
}

// New reads a Project's key set at address(project), waiting at most timeout
// for one answer, reading a set again for an unknown key id at most once a
// window, and keeping at most max Projects' sets.
func New(address func(project string) string, timeout, window time.Duration, max int) *Source {
	return &Source{
		address: address,
		client: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		window: window,
		max:    max,
		now:    time.Now,
		sets:   map[string]*keySet{},
	}
}

// Verify checks token against project's key set and answers the role it names
// and its claims as they were signed.
func (s *Source) Verify(ctx context.Context, project, token string) (string, []byte, error) {
	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{Algorithm})
	if err != nil || len(parsed.Headers) != 1 {
		return "", nil, fmt.Errorf("%w: it is not a JWT signed with %s", ErrRefused, Algorithm)
	}
	kid := parsed.Headers[0].KeyID
	key, err := s.findKey(ctx, project, kid)
	if err != nil {
		return "", nil, err
	}
	var registered jwt.Claims
	var named map[string]any
	if err := parsed.Claims(key.Public(), &registered, &named); err != nil {
		return "", nil, fmt.Errorf("%w: its signature does not verify", ErrRefused)
	}
	if registered.Expiry == nil {
		return "", nil, fmt.Errorf("%w: it has no expiry", ErrRefused)
	}
	if err := registered.ValidateWithLeeway(jwt.Expected{Time: s.now()}, jwt.DefaultLeeway); err != nil {
		return "", nil, fmt.Errorf("%w: it is expired or not valid yet", ErrRefused)
	}
	if !slices.Contains(registered.Audience, Audience) {
		return "", nil, fmt.Errorf("%w: its audience is not %s", ErrRefused, Audience)
	}
	if role, _ := named["role"].(string); role != Role {
		return "", nil, fmt.Errorf("%w: it names a role that is not %s", ErrRefused, Role)
	}
	claims, err := json.Marshal(named)
	if err != nil {
		return "", nil, fmt.Errorf("%w: its claims could not be read", ErrRefused)
	}
	return Role, claims, nil
}

// findKey is the key kid names in project's set, reading the set when it is
// not held, when it is older than MaxAge, or when it lacks kid and was read
// more than a window ago.
func (s *Source) findKey(ctx context.Context, project, kid string) (jose.JSONWebKey, error) {
	set := s.setFor(project)
	set.mu.Lock()
	defer set.mu.Unlock()

	now := s.now()
	stale := set.fetched.IsZero() || now.Sub(set.fetched) >= MaxAge
	if !stale {
		if keys := set.keys.Key(kid); len(keys) == 1 {
			return keys[0], nil
		}
		if now.Sub(set.fetched) < s.window {
			return jose.JSONWebKey{}, fmt.Errorf("%w: no key of the Project's set has its key id", ErrRefused)
		}
	}
	keys, err := s.read(ctx, project)
	if err != nil {
		return jose.JSONWebKey{}, err
	}
	set.keys, set.fetched = keys, s.now()
	if found := set.keys.Key(kid); len(found) == 1 {
		return found[0], nil
	}
	return jose.JSONWebKey{}, fmt.Errorf("%w: no key of the Project's set has its key id", ErrRefused)
}

// setFor is project's entry, made on its first token. The map is bounded: what
// goes when it is full is the set read longest ago.
func (s *Source) setFor(project string) *keySet {
	s.mu.Lock()
	defer s.mu.Unlock()
	if set, ok := s.sets[project]; ok {
		return set
	}
	if s.max > 0 && len(s.sets) >= s.max {
		var oldestName string
		var oldest time.Time
		for name, set := range s.sets {
			if oldestName == "" || set.fetched.Before(oldest) {
				oldestName, oldest = name, set.fetched
			}
		}
		delete(s.sets, oldestName)
	}
	set := &keySet{}
	s.sets[project] = set
	return set
}

// read asks the auth api for project's key set.
func (s *Source) read(ctx context.Context, project string) (jose.JSONWebKeySet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.address(url.PathEscape(project)), nil)
	if err != nil {
		return jose.JSONWebKeySet{}, fmt.Errorf("%w: the key-set address is not an address", ErrUnavailable)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return jose.JSONWebKeySet{}, fmt.Errorf("%w: the auth api did not answer", ErrUnavailable)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxKeySet))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return jose.JSONWebKeySet{}, fmt.Errorf("%w: the auth api said %d", ErrUnavailable, resp.StatusCode)
	}
	var keys jose.JSONWebKeySet
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxKeySet)).Decode(&keys); err != nil {
		return jose.JSONWebKeySet{}, fmt.Errorf("%w: the key set could not be read", ErrUnavailable)
	}
	return keys, nil
}
