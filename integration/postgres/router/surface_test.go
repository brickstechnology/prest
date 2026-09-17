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

// rest_surface is a schema this test owns in both test databases, so no
// other package's writes to public.test change what it reads. Its table is
// named test, which testdata/prest.toml's access list lets a caller read in
// any schema.
const schema = "rest_surface"

// miniship: rest reads the database it was given, and refuses one it was not
// given though that database sits on the same Postgres, holds the same tables,
// and opens for the same login. Upstream, with pg.single off, served it.
// This runs rest in-process, because the deployed prestd services in
// integration/postgres/docker-compose.yml are not started by miniship.yml.
func TestRest_readsTheDatabaseItWasGivenAndNoOther(t *testing.T) {
	cfg := helpers.LoadTestConfig(t)
	cfg.SingleDB = false
	a, err := app.New(cfg)
	require.NoError(t, err)
	server := httptest.NewServer(a.Handler)
	t.Cleanup(server.Close)

	given, notGiven := helpers.Databases()[0], helpers.Databases()[1]
	ownTable(t, openDatabase(t, cfg, given), "a row only "+given+" holds")
	ownTable(t, openDatabase(t, cfg, notGiven), "a row only "+notGiven+" holds")

	// Read the table in the database rest was given.
	// Expected: 200 with that database's row.
	status, body := call(t, server.URL, http.MethodGet, "/"+given+"/"+schema+"/test", "")
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, "a row only "+given+" holds")

	// Read the same table in secondary-db, which rest was not given.
	// Expected: 404, and no row of either database in the body.
	status, body = call(t, server.URL, http.MethodGet, "/"+notGiven+"/"+schema+"/test", "")
	require.Equal(t, http.StatusNotFound, status, body)
	require.NotContains(t, body, "holds")

	// Read the same table name in the maintenance database every Postgres
	// has. The table is in the access list, so the database decides.
	// Expected: 404.
	status, body = call(t, server.URL, http.MethodGet, "/postgres/"+schema+"/test", "")
	require.Equal(t, http.StatusNotFound, status, body)

	// Read that database's catalog. pg_database is not in the access list,
	// and pREST's access check runs before the table read checks the
	// database, so the access check answers.
	// Expected: 401, and no database name from the catalog.
	status, body = call(t, server.URL, http.MethodGet, "/postgres/pg_catalog/pg_database", "")
	require.Equal(t, http.StatusUnauthorized, status, body)
	require.NotContains(t, body, "template1")

	// List the databases on the server, which upstream answered.
	// Expected: 404, and neither test database named.
	status, body = call(t, server.URL, http.MethodGet, "/databases", "")
	require.Equal(t, http.StatusNotFound, status, body)
	require.NotContains(t, body, notGiven)

	// Write a row into the given database's table.
	// Expected: 404, and the row is not there afterwards.
	status, body = call(t, server.URL, http.MethodPost, "/"+given+"/"+schema+"/test", `{"name": "written through rest"}`)
	require.Equal(t, http.StatusNotFound, status, body)
	status, body = call(t, server.URL, http.MethodGet, "/"+given+"/"+schema+"/test", "")
	require.Equal(t, http.StatusOK, status, body)
	require.NotContains(t, body, "written through rest")

	// Ask for the health address.
	// Expected: 200.
	status, body = call(t, server.URL, http.MethodGet, "/_health", "")
	require.Equal(t, http.StatusOK, status, body)
}

// ownTable creates rest_surface.test in db holding one row, and drops the
// schema when the test ends.
func ownTable(t *testing.T, db *sql.DB, row string) {
	t.Helper()
	_, err := db.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE;
		CREATE SCHEMA ` + schema + `;
		CREATE TABLE ` + schema + `.test (id serial, name text)`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`) })
	_, err = db.Exec(`INSERT INTO `+schema+`.test (name) VALUES ($1)`, row)
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
