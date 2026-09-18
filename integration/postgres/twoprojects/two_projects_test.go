package twoprojects_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/adapters/postgres"
	"github.com/prest/prest/v2/app"
	"github.com/prest/prest/v2/config"
	"github.com/prest/prest/v2/integration/helpers"
)

// The login is cluster-wide, as miniship's authenticator is: NOINHERIT, owning
// nothing, granted each Project's anonymous role so that a read can become it.
const (
	login    = "rest_twoprojects_login"
	password = "rest-twoprojects-login"

	// How long a Database is watched for a statement it was never asked for.
	idleWindow = 3 * time.Second
)

// A Project, as rest is given one: the name in the first path segment, the
// Database behind it, the role its reads become, and the one row that says
// which Database answered.
type project struct {
	alias    string
	database string
	anonRole string
	title    string
}

var (
	alpha = project{"alpha", "rest_twoprojects_alpha", "rest_twoprojects_anon_alpha", "the row alpha holds"}
	beta  = project{"beta", "rest_twoprojects_beta", "rest_twoprojects_anon_beta", "the row beta holds"}
	// Registered, and never called by any subject in this package.
	gamma = project{"gamma", "rest_twoprojects_gamma", "rest_twoprojects_anon_gamma", "the row nobody asks for"}

	projects = []project{alpha, beta, gamma}
)

var (
	stageOnce sync.Once
	staged    *sql.DB
)

// needsPostgres counts a subject that needs the live Postgres, and returns a
// superuser connection to the test database — from which pg_stat_activity and
// pg_stat_database answer for the whole cluster. A subject that cannot reach
// it fails; it never skips.
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
	require.NotNil(t, staged, "the three Databases were not staged")

	// Whatever a previous subject left connected has gone, so what this one
	// reads out of pg_stat_activity is its own.
	for _, p := range projects {
		require.Eventually(t, func() bool { return restBackends(t, admin, p.database) == 0 },
			20*time.Second, 100*time.Millisecond,
			"a rest connection to %s outlived the subject that opened it", p.database)
	}

	// Cleared last, so what countsRan reports is this subject's own window.
	// testify's poll goroutine outlives the assertion that started it by up to
	// one tick, and the subject before this one had its admin connection closed
	// by t.Cleanup in between — so a straggler of its own can record
	// "sql: database is closed" after it has finished. The drain above has
	// taken seconds by the time this line runs, which is far longer than that
	// straggler lives.
	whyTheCountFailed.Store(nil)
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

// stage creates the login, and one Database for each Project holding one row
// that names it. Three Databases rather than three schemas: a schema cannot
// show that a connection went to the wrong place, and this asks the cluster
// which Databases were connected to.
func stage(t *testing.T, cfg *config.Prest) {
	t.Helper()
	// Its own connection, outlasting every subject: dropProjects runs after
	// the last of them, when each subject's own has been closed.
	admin, err := sql.Open("postgres", dsn(cfg, cfg.PGUser, cfg.PGPass, cfg.PGDatabase))
	require.NoError(t, err)
	staged = admin
	dropProjects()

	exec(t, admin, fmt.Sprintf(
		`CREATE ROLE %s LOGIN NOINHERIT NOBYPASSRLS PASSWORD '%s'`, login, password))
	for _, p := range projects {
		exec(t, admin, fmt.Sprintf(`CREATE ROLE %s NOLOGIN NOBYPASSRLS`, p.anonRole))
		exec(t, admin, fmt.Sprintf(`GRANT %s TO %s`, p.anonRole, login))
		exec(t, admin, fmt.Sprintf(`CREATE DATABASE %s`, p.database))

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
}

// dropProjects removes everything stage made. It runs before staging and after
// the last subject, and says nothing when there was nothing to remove.
func dropProjects() {
	if staged == nil {
		return
	}
	for _, p := range projects {
		_, _ = staged.Exec(fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, p.database))
		_, _ = staged.Exec(fmt.Sprintf(`DROP ROLE IF EXISTS %s`, p.anonRole))
	}
	_, _ = staged.Exec(fmt.Sprintf(`DROP ROLE IF EXISTS %s`, login))
}

func exec(t *testing.T, db *sql.DB, statements string) {
	t.Helper()
	_, err := db.Exec(statements)
	require.NoError(t, err)
}

// restOverProjects is rest as prestd composes it, given all three Projects,
// connecting as the login and becoming each Project's own role. It keeps one
// idle connection to each Database it opens, so a Database can be watched for
// what rest does on a connection nobody is using.
func restOverProjects(t *testing.T, cfg *config.Prest) *httptest.Server {
	t.Helper()
	conf := *cfg
	conf.Adapter = nil
	conf.SingleDB = false
	conf.Debug = false
	conf.EnableDefaultJWT = false
	conf.AuthEnabled = false
	conf.Cache.Enabled = false
	conf.AccessConf = config.AccessConf{Restrict: false}
	conf.PGUser, conf.PGPass, conf.PGAnonRole = login, password, ""
	conf.Databases = nil
	for _, p := range projects {
		conf.Databases = append(conf.Databases, config.DatabaseConf{
			Alias:       p.alias,
			Host:        cfg.PGHost,
			Port:        cfg.PGPort,
			User:        login,
			Pass:        password,
			Database:    p.database,
			SSL:         config.DatabaseSSLConf{Mode: cfg.PGSSLMode},
			MaxOpenConn: 2,
			MaxIdleConn: 1,
			AnonRole:    p.anonRole,
		})
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

func get(t *testing.T, server *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := server.Client().Get(server.URL + path)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

func read(t *testing.T, server *httptest.Server, p project) (int, string) {
	t.Helper()
	return get(t, server, "/"+p.alias+"/public/posts")
}

// restBackends is how many of a Database's sessions are rest's, which is the
// question a Project's operator asks of pg_stat_activity.
//
// miniship (#548): it answers **1** when the query could not run, and never 0.
// Two reasons, and both were paid for.
//
// The first is that this is asked inside require.Never and require.Eventually,
// which poll in a goroutine of their own. A require in one of those fails the
// test from outside it, and once the subject has finished, testify turns that
// into `panic: Fail in goroutine after ... has completed` — a red run whose
// subject says PASS. It happened here, on a runner busy enough that the last
// poll landed after t.Cleanup had closed this connection.
//
// The second is the one that matters more. Every assertion this feeds is an
// absence — *no connection reached that Project* — and a query that did not run
// answers 0 exactly like a Database nothing connected to. So a failure answers
// the number that fails both shapes: `> 0` is true, so Never fails, and `== 0`
// is false, so Eventually times out. Either way the run is red, and the error
// is on the line below rather than swallowed.
func restBackends(t *testing.T, admin *sql.DB, database string) int {
	t.Helper()
	var n int
	if err := admin.QueryRow(
		`SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND application_name = $2`,
		database, postgres.ApplicationName).Scan(&n); err != nil {
		// Not through t: a t.Log from a polling goroutine that outlived the
		// subject is the same panic as a t.Error from one. whyTheCountFailed
		// carries it to a subject that is still running.
		whyTheCountFailed.Store(&err)
		return 1
	}
	return n
}

// whyTheCountFailed is the last failure of the two pg_stat_activity queries
// above, kept off t for the reason restBackends gives. countsRan is asked after
// a poll, by a subject that is still running, so a red says what went wrong
// rather than only that a number was not the one expected.
var whyTheCountFailed atomic.Pointer[error]

func countsRan(t *testing.T) {
	t.Helper()
	if err := whyTheCountFailed.Swap(nil); err != nil {
		t.Fatalf("this subject asked pg_stat_activity and it did not answer: %v", *err)
	}
}

// A session of rest's, as the Database sees it. Every field moves when a
// statement runs on that connection and none of them moves when nothing does:
// the last statement's text and the moment it started, the moment the session
// last changed state, and the pid and start of the backend, which say whether
// this is even the same connection.
type session struct {
	pid          int
	backendStart time.Time
	state        string
	query        string
	queryStart   sql.NullTime
	stateChange  sql.NullTime
}

// restSessions is what a Database's rest connections are doing.
//
// The obvious counter is xact_commit in pg_stat_database, which the ticket
// suggests — and it is wrong here, because a database is never a database
// nothing else uses: **autovacuum's own workers connect to each one and run
// transactions in it**, so that counter moves on its own, roughly once a naptime.
// It failed a run of this subject by exactly that. pg_stat_activity is scoped
// to rest's own sessions, which is the claim being made.
//
// miniship (#548): it answers nil when the query could not run, for the reason
// restBackends answers 1 — this is asked inside a poll, and the poll's
// goroutine outlives the subject. nil is what fails the caller: idleSessions
// keeps waiting on an empty answer and times out, and the direct comparison
// below is against a list that has rows in it.
func restSessions(t *testing.T, admin *sql.DB, database string) []session {
	t.Helper()
	rows, err := admin.Query(`
		SELECT pid, backend_start, state, query, query_start, state_change
		FROM pg_stat_activity
		WHERE datname = $1 AND application_name = $2
		ORDER BY pid`, database, postgres.ApplicationName)
	if err != nil {
		whyTheCountFailed.Store(&err)
		return nil
	}
	defer rows.Close()

	var out []session
	for rows.Next() {
		var s session
		if err := rows.Scan(
			&s.pid, &s.backendStart, &s.state, &s.query, &s.queryStart, &s.stateChange); err != nil {
			whyTheCountFailed.Store(&err)
			return nil
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		whyTheCountFailed.Store(&err)
		return nil
	}
	return out
}

// idleSessions is restSessions once every one of them has gone idle, so the
// window measured after it is rest sending nothing rather than rest still
// finishing the call before it.
func idleSessions(t *testing.T, admin *sql.DB, database string) []session {
	t.Helper()
	var settled []session
	require.Eventually(t, func() bool {
		settled = restSessions(t, admin, database)
		if len(settled) == 0 {
			return false
		}
		for _, s := range settled {
			if s.state != "idle" {
				return false
			}
		}
		return true
	}, 20*time.Second, 100*time.Millisecond,
		"rest's connections to %s never went idle after the call", database)
	return settled
}

// One process, one run, two Projects: each path returns its own Database's row
// and never the other's.
func TestRest_eachProjectsTableReturnsItsOwnRows(t *testing.T) {
	cfg, _ := needsPostgres(t)
	rest := restOverProjects(t, cfg)

	for _, p := range []project{alpha, beta, alpha, beta} {
		status, body := read(t, rest, p)
		require.Equal(t, http.StatusOK, status, "%s: %s", p.alias, body)
		require.JSONEq(t, `[{"id": 1, "title": "`+p.title+`"}]`, body)
		for _, other := range projects {
			if other.alias != p.alias {
				require.NotContains(t, body, other.title,
					"%s answered with %s's row", p.alias, other.alias)
			}
		}
	}
}

// A Project nobody calls costs nothing: no connection at start-up, none from a
// health check, and none from another Project's read.
func TestRest_aProjectNobodyCallsRecordsNoConnection(t *testing.T) {
	cfg, admin := needsPostgres(t)
	rest := restOverProjects(t, cfg)

	// Composed, and nothing asked for yet.
	for _, p := range projects {
		require.Zero(t, restBackends(t, admin, p.database),
			"composing rest connected to %s", p.alias)
	}

	// Health checks, which are what an orchestrator asks over and over.
	for range 10 {
		status, body := get(t, rest, "/_health")
		require.Equal(t, http.StatusOK, status, body)
	}
	for _, p := range projects {
		require.Zero(t, restBackends(t, admin, p.database),
			"a health check connected to %s", p.alias)
	}

	// One Project is read. The other two are not connected to: not the one
	// registered before it, and not the one after.
	status, body := read(t, rest, beta)
	require.Equal(t, http.StatusOK, status, body)
	require.Eventually(t, func() bool { return restBackends(t, admin, beta.database) > 0 },
		10*time.Second, 100*time.Millisecond, "beta's read opened no connection")
	require.Never(t, func() bool {
		return restBackends(t, admin, alpha.database)+restBackends(t, admin, gamma.database) > 0
	}, idleWindow, 250*time.Millisecond,
		"reading beta connected to a Project nobody called")
	// The window above is an absence, so it is only worth something if the
	// question was actually asked throughout it.
	countsRan(t)
}

// After a Project's last call, nothing runs on its connections until the next
// one — no keep-alive ping, no validation query, no SET on a timer.
func TestRest_afterTheLastCallNothingRunsOnThatDatabase(t *testing.T) {
	cfg, admin := needsPostgres(t)
	rest := restOverProjects(t, cfg)

	status, body := read(t, rest, beta)
	require.Equal(t, http.StatusOK, status, body)

	// The connection is there to be quiet on: what the window below measures is
	// rest holding an idle connection and sending nothing, not rest having hung
	// up.
	before := idleSessions(t, admin, beta.database)
	time.Sleep(idleWindow)
	require.Equal(t, before, restSessions(t, admin, beta.database),
		"rest ran a statement on %s that no caller asked for", beta.alias)

	// The control: the same fields move when a caller asks, so the stillness
	// above is rest's silence and not fields that never move.
	status, body = read(t, rest, beta)
	require.Equal(t, http.StatusOK, status, body)
	after := idleSessions(t, admin, beta.database)
	require.NotEqual(t, before, after,
		"the fields this subject watched never move, so it proved nothing")
	countsRan(t)
}

// A Database can tell which of its sessions are rest's.
func TestRest_connectionsSayWhoTheyAre(t *testing.T) {
	cfg, admin := needsPostgres(t)
	rest := restOverProjects(t, cfg)

	status, body := read(t, rest, alpha)
	require.Equal(t, http.StatusOK, status, body)

	var name, user string
	require.NoError(t, admin.QueryRow(`
		SELECT application_name, usename FROM pg_stat_activity
		WHERE datname = $1 AND usename = $2 LIMIT 1`,
		alpha.database, login).Scan(&name, &user))
	require.Equal(t, postgres.ApplicationName, name)
	require.Equal(t, login, user)

	// And this subject's own connection is not rest's, so the name tells the
	// two apart rather than naming everything.
	var mine int
	require.NoError(t, admin.QueryRow(`
		SELECT count(*) FROM pg_stat_activity
		WHERE pid = pg_backend_pid() AND application_name = $1`,
		postgres.ApplicationName).Scan(&mine))
	require.Zero(t, mine)
}
