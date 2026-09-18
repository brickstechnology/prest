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

// theInternalCredential is what rest presents to the answerer. It is not a
// secret in this package; what the subjects below check is that it is sent,
// and that the answerer's own refusal of a caller without it is what rest
// reports.
const theInternalCredential = "an-internal-credential"

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

// restWithAnswerer composes rest over the Projects it was given, with answers
// standing where the process holding the Database plugin is. The two windows
// are short so a subject can pass through them without sleeping for seconds.
func restWithAnswerer(t *testing.T, answers *answerer, given map[string]*countingDatabase) *app.App {
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
			Token:       theInternalCredential,
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
	a, err := app.New(cfg)
	require.NoError(t, err)
	return a
}
