package router_test

import (
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/app"
	"github.com/prest/prest/v2/config"
	"github.com/prest/prest/v2/integration/helpers"
)

// rest_surface_test is a public table this test owns in both test databases,
// so no other package's writes change what it reads. rest serves public alone.
const table = "rest_surface_test"

// reader is the role rest's reads become here. The test connects as the
// superuser, which may become any role; the role holds SELECT on the table.
const reader = "rest_surface_reader"

// miniship: rest reads the database it was given, and refuses one it was not
// given though that database sits on the same Postgres, holds the same tables,
// and opens for the same login. Upstream, with pg.single off, served it.
// This runs rest in-process, because the deployed prestd services in
// integration/postgres/docker-compose.yml are not started by miniship.yml.
func TestRest_readsTheDatabaseItWasGivenAndNoOther(t *testing.T) {
	cfg := helpers.LoadTestConfig(t)
	cfg.SingleDB = false
	// As the stack runs rest: its own access list off, so the database's
	// grants decide, and one role to become.
	cfg.AccessConf = config.AccessConf{Restrict: false}
	cfg.Cache.Enabled = false
	cfg.PGAnonRole = reader
	// app.New builds its own adapter over this config. The shared test adapter
	// reads the config it was loaded with, whose access list is on.
	cfg.Adapter = nil
	a, err := app.New(cfg)
	require.NoError(t, err)
	server := httptest.NewServer(a.Handler)
	t.Cleanup(server.Close)

	given, notGiven := helpers.Databases()[0], helpers.Databases()[1]
	givenDB, notGivenDB := openDatabase(t, cfg, given), openDatabase(t, cfg, notGiven)
	ownRole(t, givenDB, givenDB, notGivenDB)
	ownTable(t, givenDB, "a row only "+given+" holds")
	ownTable(t, notGivenDB, "a row only "+notGiven+" holds")

	// Read the table in the database rest was given.
	// Expected: 200 with that database's row.
	status, body := call(t, server.URL, http.MethodGet, "/"+given+"/public/"+table, "")
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, "a row only "+given+" holds")

	// Read the same table in secondary-db, which rest was not given.
	// Expected: 404, and no row of either database in the body.
	status, body = call(t, server.URL, http.MethodGet, "/"+notGiven+"/public/"+table, "")
	require.Equal(t, http.StatusNotFound, status, body)
	require.NotContains(t, body, "holds")

	// Read the same table name in the maintenance database every Postgres
	// has.
	// Expected: 404.
	status, body = call(t, server.URL, http.MethodGet, "/postgres/public/"+table, "")
	require.Equal(t, http.StatusNotFound, status, body)

	// Read a catalog, in that database and in the given one. rest serves
	// public alone, and says so before any SQL.
	// Expected: 404, and no database name from the catalog.
	for _, path := range []string{"/postgres/pg_catalog/pg_database", "/" + given + "/pg_catalog/pg_database"} {
		status, body = call(t, server.URL, http.MethodGet, path, "")
		require.Equal(t, http.StatusNotFound, status, "%s: %s", path, body)
		require.NotContains(t, body, "template1", path)
	}

	// List the databases on the server, which upstream answered.
	// Expected: 404, and neither test database named.
	status, body = call(t, server.URL, http.MethodGet, "/databases", "")
	require.Equal(t, http.StatusNotFound, status, body)
	require.NotContains(t, body, notGiven)

	// Write a row into the given database's table.
	// Expected: 404, and the row is not there afterwards.
	status, body = call(t, server.URL, http.MethodPost, "/"+given+"/public/"+table, `{"name": "written through rest"}`)
	require.Equal(t, http.StatusNotFound, status, body)
	status, body = call(t, server.URL, http.MethodGet, "/"+given+"/public/"+table, "")
	require.Equal(t, http.StatusOK, status, body)
	require.NotContains(t, body, "written through rest")

	// Ask for the health address.
	// Expected: 200.
	status, body = call(t, server.URL, http.MethodGet, "/_health", "")
	require.Equal(t, http.StatusOK, status, body)
}

// ownRole creates the reader role once, through db, and removes it when the
// test ends, after dropping what it holds in each of every.
func ownRole(t *testing.T, db *sql.DB, every ...*sql.DB) {
	t.Helper()
	drop := func() {
		for _, each := range every {
			_, _ = each.Exec(`DO $$ BEGIN
				IF EXISTS (SELECT FROM pg_roles WHERE rolname = '` + reader + `') THEN
					DROP OWNED BY ` + reader + `;
				END IF;
			END $$`)
		}
		_, _ = db.Exec(`DROP ROLE IF EXISTS ` + reader)
	}
	drop()
	_, err := db.Exec(`CREATE ROLE ` + reader + ` NOLOGIN NOBYPASSRLS`)
	require.NoError(t, err)
	t.Cleanup(drop)
}

// ownTable creates public.rest_surface_test in db holding one row, readable
// by the reader, and drops it when the test ends.
func ownTable(t *testing.T, db *sql.DB, row string) {
	t.Helper()
	_, err := db.Exec(`DROP TABLE IF EXISTS public.` + table + `;
		CREATE TABLE public.` + table + ` (id serial, name text);
		GRANT SELECT ON public.` + table + ` TO ` + reader)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DROP TABLE IF EXISTS public.` + table) })
	_, err = db.Exec(`INSERT INTO public.`+table+` (name) VALUES ($1)`, row)
	require.NoError(t, err)
}

func call(t *testing.T, base, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, base+path, strings.NewReader(body))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(b)
}

// openDatabase connects to name on cfg's server with cfg's login, outside
// rest, to set up what rest must not reach.
func openDatabase(t *testing.T, cfg *config.Prest, name string) *sql.DB {
	t.Helper()
	dsn := (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(cfg.PGUser, cfg.PGPass),
		Host:     fmt.Sprintf("%s:%d", cfg.PGHost, cfg.PGPort),
		Path:     "/" + name,
		RawQuery: "sslmode=" + cfg.PGSSLMode,
	}).String()
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())
	return db
}
