package anonymousrole_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/adapters"
	"github.com/prest/prest/v2/adapters/postgres"
	"github.com/prest/prest/v2/app"
	"github.com/prest/prest/v2/config"
	pctx "github.com/prest/prest/v2/context"
	"github.com/prest/prest/v2/integration/helpers"
)

// The roles are cluster-wide, so their names are this package's own. They
// mirror miniship's pair: the login is NOINHERIT and is granted the reader,
// which cannot log in. The stranger is a role the login was never granted.
const (
	login    = "rest_anonrole_login"
	password = "rest-anonrole-login"
	reader   = "rest_anonrole_reader"
	stranger = "rest_anonrole_stranger"
	nobody   = "rest_anonrole_nobody"

	// The alias rest is given the test database under.
	alias = "project"
	// A schema outside public, which the reader may read in the database.
	private = "rest_anonrole_private"
)

// needsPostgres counts a subject that needs the live Postgres, and returns a
// superuser connection to the test database. A subject that cannot reach it
// fails; it never skips.
func needsPostgres(t *testing.T) (*config.Prest, *sql.DB) {
	t.Helper()
	needed.Add(1)
	cfg := helpers.LoadTestConfig(t)
	db, err := sql.Open("postgres", dsn(cfg, cfg.PGUser, cfg.PGPass))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, db.PingContext(ctx), "this subject needs a live Postgres, and did not get one")
	got.Add(1)
	stage(t, db)
	return cfg, db
}

func dsn(cfg *config.Prest, user, pass string) string {
	return (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, pass),
		Host:     fmt.Sprintf("%s:%d", cfg.PGHost, cfg.PGPort),
		Path:     "/" + cfg.PGDatabase,
		RawQuery: "sslmode=" + cfg.PGSSLMode,
	}).String()
}

// stage creates the roles and the tables every subject reads, as the
// superuser, and removes them when the subject ends.
//
//   - public.rest_anonrole_posts has row security, and its policy shows a row
//     only to the role named in visible_to. Both the reader and the login hold
//     SELECT on it, so a read that ran as the login would see the login's row.
//   - public.rest_anonrole_ungranted is readable by the login and not by the
//     reader.
//   - rest_anonrole_private.posts is readable by the reader, outside public.
func stage(t *testing.T, db *sql.DB) {
	t.Helper()
	unstage(t, db)
	t.Cleanup(func() { unstage(t, db) })
	exec(t, db, fmt.Sprintf(`
		CREATE ROLE %[1]s LOGIN NOINHERIT NOBYPASSRLS PASSWORD '%[2]s';
		CREATE ROLE %[3]s NOLOGIN NOBYPASSRLS;
		CREATE ROLE %[4]s NOLOGIN NOBYPASSRLS;
		GRANT %[3]s TO %[1]s;

		CREATE TABLE public.rest_anonrole_posts (id int PRIMARY KEY, title text, visible_to text);
		INSERT INTO public.rest_anonrole_posts VALUES
			(1, 'a post the reader may read', '%[3]s'),
			(2, 'a second post the reader may read', '%[3]s'),
			(3, 'a post only the login may read', '%[1]s'),
			(4, 'a post nobody may read', '%[5]s');
		ALTER TABLE public.rest_anonrole_posts ENABLE ROW LEVEL SECURITY;
		CREATE POLICY visible_to_its_role ON public.rest_anonrole_posts
			FOR SELECT USING (visible_to = current_user);
		GRANT SELECT ON public.rest_anonrole_posts TO %[3]s, %[1]s;

		CREATE TABLE public.rest_anonrole_ungranted (id int PRIMARY KEY, secret text);
		INSERT INTO public.rest_anonrole_ungranted VALUES (1, 'a secret the reader holds no grant on');
		GRANT SELECT ON public.rest_anonrole_ungranted TO %[1]s;

		CREATE SCHEMA %[6]s;
		CREATE TABLE %[6]s.posts (id int PRIMARY KEY, title text);
		INSERT INTO %[6]s.posts VALUES (1, 'a row outside public');
		GRANT USAGE ON SCHEMA %[6]s TO %[3]s, %[1]s;
		GRANT SELECT ON %[6]s.posts TO %[3]s, %[1]s;
	`, login, password, reader, stranger, nobody, private))
}

func unstage(t *testing.T, db *sql.DB) {
	t.Helper()
	exec(t, db, fmt.Sprintf(`
		DROP TABLE IF EXISTS public.rest_anonrole_posts, public.rest_anonrole_ungranted;
		DROP SCHEMA IF EXISTS %[1]s CASCADE;
		DO $$
		DECLARE r text;
		BEGIN
			FOREACH r IN ARRAY ARRAY['%[2]s', '%[3]s', '%[4]s'] LOOP
				IF EXISTS (SELECT FROM pg_roles WHERE rolname = r) THEN
					EXECUTE format('DROP OWNED BY %%I', r);
					EXECUTE format('DROP ROLE %%I', r);
				END IF;
			END LOOP;
		END $$;
	`, private, login, reader, stranger))
}

func exec(t *testing.T, db *sql.DB, statements string) {
	t.Helper()
	_, err := db.Exec(statements)
	require.NoError(t, err)
}

// restAs is rest as prestd composes it, given the test database once, under
// alias, connecting as the login and reading as role. It runs with pREST's own
// access list off, so what a caller receives is what the database allows.
func restAs(t *testing.T, cfg *config.Prest, role string) *httptest.Server {
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
	conf.PGMaxIdleConn = 0
	conf.Databases = []config.DatabaseConf{{
		Alias:       alias,
		Host:        cfg.PGHost,
		Port:        cfg.PGPort,
		User:        login,
		Pass:        password,
		Database:    cfg.PGDatabase,
		SSL:         config.DatabaseSSLConf{Mode: cfg.PGSSLMode},
		MaxOpenConn: 2,
		AnonRole:    role,
	}}
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

func call(t *testing.T, server *httptest.Server, method, path string, header http.Header) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(method, server.URL+path, strings.NewReader(`{"title":"written through rest"}`))
	require.NoError(t, err)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, string(body)
}

func get(t *testing.T, server *httptest.Server, path string) (int, string) {
	t.Helper()
	status, _, body := call(t, server, http.MethodGet, path, nil)
	return status, body
}

func TestRest_rowSecurityDecidesWhichRowsArrive(t *testing.T) {
	cfg, _ := needsPostgres(t)
	rest := restAs(t, cfg, reader)

	// Read the table whose policy shows two rows to the reader.
	// Expected: 200 with those two, and neither the login's row nor the row
	// nobody may read.
	status, body := get(t, rest, "/"+alias+"/public/rest_anonrole_posts?_order=id")
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `[
		{"id": 1, "title": "a post the reader may read", "visible_to": "`+reader+`"},
		{"id": 2, "title": "a second post the reader may read", "visible_to": "`+reader+`"}
	]`, body)

	// Ask for each hidden row by its key.
	// Expected: 200 and an empty list, each time.
	for _, id := range []string{"3", "4"} {
		status, body = get(t, rest, "/"+alias+"/public/rest_anonrole_posts?id="+id)
		require.Equal(t, http.StatusOK, status, body)
		require.JSONEq(t, `[]`, body, "row %s", id)
	}

	// Ask for the hidden rows by the column the policy reads.
	// Expected: an empty list; the filter cannot widen the policy.
	status, body = get(t, rest, "/"+alias+"/public/rest_anonrole_posts?visible_to=$ne."+reader)
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `[]`, body)

	// Count the table, count first.
	// Expected: the two rows the reader may read, not four.
	status, body = get(t, rest, "/"+alias+"/public/rest_anonrole_posts?_count=*&_count_first=true")
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `{"count": 2}`, body)
}

func TestRest_aTableTheRoleHoldsNoGrantOnIsRefusedByTheDatabase(t *testing.T) {
	cfg, _ := needsPostgres(t)
	rest := restAs(t, cfg, reader)

	// Read a public table the login may read and the reader may not.
	// Expected: 403 in Postgres's words, not pREST's access list's
	// ("authorization required"), and no row.
	status, body := get(t, rest, "/"+alias+"/public/rest_anonrole_ungranted")
	require.Equal(t, http.StatusForbidden, status, body)
	require.Contains(t, body, "permission denied for table rest_anonrole_ungranted")
	require.NotContains(t, body, "authorization required")
	require.NotContains(t, body, "a secret")
}

func TestRest_aSchemaOtherThanPublicIsRefused(t *testing.T) {
	cfg, _ := needsPostgres(t)
	rest := restAs(t, cfg, reader)

	// Read a table outside public that the reader may read in the database,
	// and two catalogs every role may read.
	// Expected: 404 for each, with no row.
	for _, path := range []string{
		"/" + alias + "/" + private + "/posts",
		"/" + alias + "/pg_catalog/pg_roles",
		"/" + alias + "/information_schema/tables",
	} {
		status, body := get(t, rest, path)
		require.Equal(t, http.StatusNotFound, status, "%s: %s", path, body)
		require.NotContains(t, body, "outside public", path)
		require.NotContains(t, body, login, path)
	}
}

func TestRest_everyWriteIsRefused(t *testing.T) {
	cfg, db := needsPostgres(t)
	rest := restAs(t, cfg, reader)

	// Write to a public table with every verb.
	// Expected: 404 for each, and the table unchanged.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, path := range []string{
			"/" + alias + "/public/rest_anonrole_posts",
			"/" + alias + "/public/rest_anonrole_posts?id=1",
			"/batch/" + alias + "/public/rest_anonrole_posts",
		} {
			status, _, body := call(t, rest, method, path, nil)
			require.Equal(t, http.StatusNotFound, status, "%s %s: %s", method, path, body)
		}
	}
	var rows int
	var titles string
	require.NoError(t, db.QueryRow(`SELECT count(*), string_agg(title, '|' ORDER BY id) FROM public.rest_anonrole_posts`).Scan(&rows, &titles))
	require.Equal(t, 4, rows)
	require.NotContains(t, titles, "written through rest")
}

func TestRest_aRoleThatCannotBeEnteredServesNothing(t *testing.T) {
	cfg, _ := needsPostgres(t)

	// A role the login was never granted, and a role that does not exist.
	for _, role := range []string{stranger, "rest_anonrole_does_not_exist"} {
		rest := restAs(t, cfg, role)

		// Read a table the login itself may read, and the table with row
		// security.
		// Expected: 500 for each, with no row: the read did not fall back to
		// the login.
		for _, table := range []string{"rest_anonrole_ungranted", "rest_anonrole_posts"} {
			status, body := get(t, rest, "/"+alias+"/public/"+table)
			require.Equal(t, http.StatusInternalServerError, status, "%s, %s: %s", role, table, body)
			require.Contains(t, body, "could not become the anonymous role", role)
			require.NotContains(t, body, "a secret", role)
			require.NotContains(t, body, "may read", role)
		}
	}
}

func TestRest_aDatabaseGivenNoRoleIsNotServed(t *testing.T) {
	cfg, _ := needsPostgres(t)
	rest := restAs(t, cfg, "")

	// Read a table the login itself may read, from a database given no role.
	// Expected: 500 and no row.
	status, body := get(t, rest, "/"+alias+"/public/rest_anonrole_ungranted")
	require.Equal(t, http.StatusInternalServerError, status, body)
	require.Contains(t, body, "no anonymous role")
	require.NotContains(t, body, "a secret")
}

func TestRest_theReadIsTheRoleAndTheConnectionReturnsAsTheLogin(t *testing.T) {
	cfg, _ := needsPostgres(t)
	conf := *cfg
	conf.Adapter = nil
	conf.PGUser, conf.PGPass = login, password
	conf.PGMaxOpenConn, conf.PGMaxIdleConn = 1, 1
	conf.Databases = []config.DatabaseConf{{
		Alias: alias, Host: cfg.PGHost, Port: cfg.PGPort, User: login, Pass: password,
		Database: cfg.PGDatabase, SSL: config.DatabaseSSLConf{Mode: cfg.PGSSLMode},
		MaxOpenConn: 1, MaxIdleConn: 1, AnonRole: reader,
	}}
	adapter := postgres.New(&conf)
	t.Cleanup(func() { postgres.Close(adapter) })
	roles, ok := adapter.(adapters.RoleReader)
	require.True(t, ok)
	ctx := context.WithValue(context.Background(), pctx.DBNameKey, alias)
	const who = `SELECT current_user AS acting, session_user AS connected, current_setting('transaction_read_only') AS read_only`

	// Read who is acting, as the reader, on a pool of one connection.
	// Expected: acting as the reader, connected as the login, read-only.
	sc := roles.QueryAsRoleCtx(ctx, reader, who)
	require.NoError(t, sc.Err())
	require.JSONEq(t, `[{"acting": "`+reader+`", "connected": "`+login+`", "read_only": "on"}]`, string(sc.Bytes()))

	// Ask the same one connection, outside any role, who is acting now.
	// Expected: the login. The role ended with the transaction.
	sc = adapter.QueryCtx(ctx, who)
	require.NoError(t, sc.Err())
	require.JSONEq(t, `[{"acting": "`+login+`", "connected": "`+login+`", "read_only": "off"}]`, string(sc.Bytes()))

	// And a role the login was not granted fails, rather than reading.
	sc = roles.QueryAsRoleCtx(ctx, stranger, who)
	require.ErrorIs(t, sc.Err(), adapters.ErrRoleNotEntered)
	require.NotContains(t, string(sc.Bytes()), login)
}

func TestRest_aBrowserMayReadFromAnyOriginButNotWriteOrCarryCredentials(t *testing.T) {
	cfg, _ := needsPostgres(t)
	t.Setenv("PREST_CONF", "/nonexistent/rest-anonrole.toml")
	defaults, err := config.Load()
	require.NoError(t, err)
	conf := *cfg
	conf.CORSAllowOrigin = defaults.CORSAllowOrigin
	conf.CORSAllowMethods = defaults.CORSAllowMethods
	conf.CORSAllowHeaders = defaults.CORSAllowHeaders
	conf.CORSAllowCredentials = defaults.CORSAllowCredentials
	rest := restAs(t, &conf, reader)
	origin := "https://an-app.example"

	// A preflight for a read, from an App's address.
	// Expected: granted, to that origin, without credentials.
	status, header, _ := call(t, rest, http.MethodOptions, "/"+alias+"/public/rest_anonrole_posts", http.Header{
		"Origin": {origin}, "Access-Control-Request-Method": {http.MethodGet},
	})
	require.Less(t, status, 300)
	require.Contains(t, []string{"*", origin}, header.Get("Access-Control-Allow-Origin"))
	require.Empty(t, header.Get("Access-Control-Allow-Credentials"))

	// A preflight for each write verb.
	// Expected: not granted.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		_, header, _ = call(t, rest, http.MethodOptions, "/"+alias+"/public/rest_anonrole_posts", http.Header{
			"Origin": {origin}, "Access-Control-Request-Method": {method},
		})
		require.Empty(t, header.Get("Access-Control-Allow-Origin"), method)
		require.NotContains(t, header.Get("Access-Control-Allow-Methods"), method, method)
	}

	// A read carrying the origin.
	// Expected: the rows, shared with that origin, without credentials.
	status, header, body := call(t, rest, http.MethodGet, "/"+alias+"/public/rest_anonrole_posts", http.Header{"Origin": {origin}})
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, []string{"*", origin}, header.Get("Access-Control-Allow-Origin"))
	require.Empty(t, header.Get("Access-Control-Allow-Credentials"))

	// The health address, with no token.
	// Expected: 200.
	status, body = get(t, rest, "/_health")
	require.Equal(t, http.StatusOK, status, body)
}
