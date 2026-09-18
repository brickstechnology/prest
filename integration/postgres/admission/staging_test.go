package admission_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/adapters/postgres"
	"github.com/prest/prest/v2/admission"
	"github.com/prest/prest/v2/app"
	"github.com/prest/prest/v2/config"
	"github.com/prest/prest/v2/integration/helpers"
)

// A Project, as a lookup answers for one: its own Database, its own login, and
// the role its reads become. One login each, and CONNECT on each Database
// granted to that login alone, so "the credential opens no other Project's
// Database" is a question Postgres can answer rather than a claim.
type project struct {
	alias    string
	database string
	login    string
	password string
	anonRole string
	title    string
}

var (
	alpha = project{"alpha", "rest_admission_alpha", "rest_admission_login_alpha",
		"alpha-login-password", "rest_admission_anon_alpha", "the row alpha holds"}
	beta = project{"beta", "rest_admission_beta", "rest_admission_login_beta",
		"beta-login-password", "rest_admission_anon_beta", "the row beta holds"}
	// gamma's Database does not exist when rest starts. It is created while
	// rest runs, which is the whole subject of this package.
	gamma = project{"gamma", "rest_admission_gamma", "rest_admission_login_gamma",
		"gamma-login-password", "rest_admission_anon_gamma", "the row gamma holds"}

	projects = []project{alpha, beta, gamma}
	// atStart are the Projects rest is given before it runs; gamma is not one.
	atStart = []project{alpha, beta}
)

// theLookupKey is the derived key for the lookup route, base64url, as the api
// hands it to rest. What travels is a service token minted with it.
var theLookupKey = base64.RawURLEncoding.EncodeToString(
	[]byte("a thirty-two byte key for a route"[:32]))

var (
	stageOnce sync.Once
	staged    *sql.DB
)

// needsPostgres counts a subject that needs the live Postgres, and returns a
// superuser connection to the test database. A subject that cannot reach it
// fails; it never skips.
func needsPostgres(t *testing.T) (*config.Prest, *sql.DB) {
	t.Helper()
	needed.Add(1)
	cfg := helpers.LoadTestConfig(t)
	admin, err := sql.Open("postgres", dsn(cfg, cfg.PGUser, cfg.PGPass, cfg.PGDatabase))
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, admin.PingContext(ctx),
		"this subject needs a live Postgres, and did not get one")
	got.Add(1)

	stageOnce.Do(func() { stage(t, cfg) })
	require.NotNil(t, staged, "the Databases were not staged")
	// Every subject starts from the same two Projects, whatever the one
	// before it created or rotated.
	dropProject(gamma)
	resetPassword(t, gamma)
	return cfg, admin
}

func dsn(cfg *config.Prest, user, pass, database string) string {
	return (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, pass),
		Host:     fmt.Sprintf("%s:%d", cfg.PGHost, cfg.PGPort),
		Path:     "/" + database,
		RawQuery: "sslmode=" + cfg.PGSSLMode,
	}).String()
}

// stage makes the three logins and the two Databases rest is given. gamma's
// login exists from the start — a Project's credential is minted when its
// Database is rented, and what this package is about is rest not knowing.
func stage(t *testing.T, cfg *config.Prest) {
	t.Helper()
	admin, err := sql.Open("postgres", dsn(cfg, cfg.PGUser, cfg.PGPass, cfg.PGDatabase))
	require.NoError(t, err)
	staged = admin
	dropProjects()

	for _, p := range projects {
		exec(t, admin, fmt.Sprintf(
			`CREATE ROLE %s LOGIN NOINHERIT NOBYPASSRLS PASSWORD '%s'`, p.login, p.password))
	}
	for _, p := range atStart {
		createProject(t, cfg, p)
	}
}

// createProject rents a Project its Database: the anonymous role its reads
// become, the login that may connect to it and to nothing else, and one row
// that names which Database answered.
func createProject(t *testing.T, cfg *config.Prest, p project) {
	t.Helper()
	exec(t, staged, fmt.Sprintf(`CREATE ROLE %s NOLOGIN NOBYPASSRLS`, p.anonRole))
	exec(t, staged, fmt.Sprintf(`GRANT %s TO %s`, p.anonRole, p.login))
	exec(t, staged, fmt.Sprintf(`CREATE DATABASE %s`, p.database))
	// The tenant boundary Postgres itself keeps: nobody may connect to this
	// Database but the login that was minted for this Project.
	exec(t, staged, fmt.Sprintf(`REVOKE CONNECT ON DATABASE %s FROM PUBLIC`, p.database))
	exec(t, staged, fmt.Sprintf(`GRANT CONNECT ON DATABASE %s TO %s`, p.database, p.login))

	own, err := sql.Open("postgres", dsn(cfg, cfg.PGUser, cfg.PGPass, p.database))
	require.NoError(t, err)
	exec(t, own, fmt.Sprintf(`
		CREATE TABLE public.posts (id int PRIMARY KEY, title text);
		INSERT INTO public.posts VALUES (1, '%s');
		GRANT USAGE ON SCHEMA public TO %s;
		GRANT SELECT ON public.posts TO %s;
	`, p.title, p.anonRole, p.anonRole))
	require.NoError(t, own.Close())
}

func dropProject(p project) {
	if staged == nil {
		return
	}
	_, _ = staged.Exec(fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, p.database))
	_, _ = staged.Exec(fmt.Sprintf(`DROP ROLE IF EXISTS %s`, p.anonRole))
}

func dropProjects() {
	if staged == nil {
		return
	}
	for _, p := range projects {
		dropProject(p)
	}
	for _, p := range projects {
		_, _ = staged.Exec(fmt.Sprintf(`DROP ROLE IF EXISTS %s`, p.login))
	}
}

func resetPassword(t *testing.T, p project) {
	t.Helper()
	if staged == nil {
		return
	}
	_, _ = staged.Exec(fmt.Sprintf(`ALTER ROLE %s PASSWORD '%s'`, p.login, p.password))
}

func exec(t *testing.T, db *sql.DB, statements string) {
	t.Helper()
	_, err := db.Exec(statements)
	require.NoError(t, err)
}

// answerer is the process holding the Database plugin, as rest reaches it: an
// HTTP server that answers for one Project at a time and counts what it was
// asked. It answers only a caller that proved itself, so a subject can ask
// whether the lookup address is internal.
type answerer struct {
	server *httptest.Server

	mu      sync.Mutex
	asked   map[string]int
	answers map[string]admission.Answer
	hold    chan struct{}
	refuse  bool
}

func newAnswerer(t *testing.T, cfg *config.Prest) *answerer {
	t.Helper()
	a := &answerer{asked: map[string]int{}, answers: map[string]admission.Answer{}}
	a.server = httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.server.Close)
	for _, p := range atStart {
		a.teach(cfg, p, p.password)
	}
	return a
}

func (a *answerer) serve(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	a.mu.Lock()
	a.asked[name]++
	answer, known := a.answers[name]
	hold, refuse := a.hold, a.refuse
	a.mu.Unlock()

	// The internal credential, checked before anything is answered. A caller
	// with none learns nothing about any Project.
	if refuse || !heldUp(r.Header.Get(admission.InternalClientHeader)) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
	}
	if !known {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}

// teach is a Project's Database being rented, as far as rest can tell: from
// now on the answerer has an address and a credential for it.
func (a *answerer) teach(cfg *config.Prest, p project, password string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.answers[p.alias] = admission.Answer{
		Project:  p.alias,
		URL:      dsn(cfg, p.login, password, p.database),
		AnonRole: p.anonRole,
	}
}

func (a *answerer) holds() func() {
	hold := make(chan struct{})
	a.mu.Lock()
	a.hold = hold
	a.mu.Unlock()
	return func() { close(hold) }
}

func (a *answerer) refuses() {
	a.mu.Lock()
	a.refuse = true
	a.mu.Unlock()
}

func (a *answerer) asksFor(project string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.asked[project]
}

func (a *answerer) answerFor(project string) admission.Answer {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.answers[project]
}

// restOverProjects is rest as prestd composes it: given the Projects that
// existed when it started, and an address to ask about every other name.
func restOverProjects(t *testing.T, cfg *config.Prest, answers *answerer, tune ...func(*config.AdmissionConf)) *httptest.Server {
	t.Helper()
	return restGiven(t, cfg, answers, atStart, tune...)
}

// restGivenNothing is the shape the self-host install runs: rest with no
// registry at all, learning about every Project by asking.
func restGivenNothing(t *testing.T, cfg *config.Prest, answers *answerer, tune ...func(*config.AdmissionConf)) *httptest.Server {
	t.Helper()
	return restGiven(t, cfg, answers, nil, tune...)
}

func restGiven(t *testing.T, cfg *config.Prest, answers *answerer, given []project, tune ...func(*config.AdmissionConf)) *httptest.Server {
	t.Helper()
	conf := *cfg
	conf.Adapter = nil
	conf.SingleDB = false
	conf.Debug = false
	conf.EnableDefaultJWT = false
	conf.AuthEnabled = false
	conf.Cache.Enabled = false
	conf.AccessConf = config.AccessConf{Restrict: false}
	conf.PGAnonRole = ""
	conf.Databases = nil
	for _, p := range given {
		conf.Databases = append(conf.Databases, config.DatabaseConf{
			Alias:       p.alias,
			Host:        cfg.PGHost,
			Port:        cfg.PGPort,
			User:        p.login,
			Pass:        p.password,
			Database:    p.database,
			SSL:         config.DatabaseSSLConf{Mode: cfg.PGSSLMode},
			MaxOpenConn: 2,
			MaxIdleConn: 1,
			AnonRole:    p.anonRole,
		})
	}
	conf.Admission = config.AdmissionConf{
		URL:         answers.server.URL,
		Key:         theLookupKey,
		Timeout:     2 * time.Second,
		Window:      2 * time.Second,
		MaxProjects: 4000,
	}
	for _, tune := range tune {
		tune(&conf.Admission)
	}

	a, err := app.New(&conf)
	require.NoError(t, err)
	server := httptest.NewServer(a.Handler)
	t.Cleanup(func() {
		server.Close()
		for _, name := range a.Adapters.GetAll() {
			if adapter, err := a.Adapters.Get(name); err == nil {
				postgres.Close(adapter)
			}
		}
	})
	return server
}

func read(t *testing.T, server *httptest.Server, p project) (int, string) {
	t.Helper()
	resp, err := server.Client().Get(server.URL + "/" + p.alias + "/public/posts")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

// readsItsOwnRow is the whole of what an App's browser wants: this Project's
// row, and no other Project's.
func readsItsOwnRow(t *testing.T, server *httptest.Server, p project) {
	t.Helper()
	status, body := read(t, server, p)
	require.Equal(t, http.StatusOK, status, "%s: %s", p.alias, body)
	require.JSONEq(t, `[{"id": 1, "title": "`+p.title+`"}]`, body)
	for _, other := range projects {
		if other.alias != p.alias {
			require.NotContains(t, body, other.title,
				"%s answered with %s's row", p.alias, other.alias)
		}
	}
}

// hangUpOn ends every connection rest holds to a Database, so its next read
// must open a new one — which is what makes a credential that was rotated
// visible at all. Postgres leaves an established session alone when a role's
// password changes.
func hangUpOn(t *testing.T, admin *sql.DB, p project) {
	t.Helper()
	_, err := admin.Exec(
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		  WHERE datname = $1 AND application_name = $2`,
		p.database, postgres.ApplicationName)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return restBackends(t, admin, p) == 0 },
		20*time.Second, 100*time.Millisecond, "rest's connections to %s did not end", p.alias)
}

func restBackends(t *testing.T, admin *sql.DB, p project) int {
	t.Helper()
	var n int
	require.NoError(t, admin.QueryRow(
		`SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND application_name = $2`,
		p.database, postgres.ApplicationName).Scan(&n))
	return n
}
