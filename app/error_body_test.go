package app_test

// miniship (#549): what an error answer may say. #545 found that pREST returns
// the driver's error verbatim, the Database's host and port included. An error
// answer from rest carries no SQL text, no connection string, no host name and
// no port; the detail is logged where only an operator reads it.

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// secrets are the things an error answer must never carry, with the name of
// what each one would give away.
func requireNoSecrets(t *testing.T, port int, body string) {
	t.Helper()
	for what, secret := range map[string]string{
		"the Database's host":     "127.0.0.1",
		"the Database's port":     strconv.Itoa(port),
		"the login rest holds":    "authenticator",
		"the connection string":   "sslmode",
		"a password field":        "password",
		"the driver":              "pq:",
		"the driver's dial error": "dial tcp",
	} {
		require.NotContains(t, body, secret, "the answer carries %s", what)
	}
	for _, sql := range []string{"SELECT", "FROM \"", "jsonb_agg", "LIMIT"} {
		require.NotContains(t, body, sql, "the answer carries SQL text")
	}
}

// A Database that answers nothing — it accepts the connection and closes it —
// is the reachable shape of "the Database refused the connection".
func TestTableRead_aDatabaseThatRefusesTheConnection_saysNothingAboutIt(t *testing.T) {
	db := newCountingDatabase(t)
	rest := newRest(t, db)

	rec := serve(rest, "GET /given/public/posts")
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	requireNoSecrets(t, db.port, rec.Body.String())
	db.requireAConnection(t)
}

// Every error answer rest can give without a Database, in one place.
func TestTableRead_noErrorAnswerCarriesASecret(t *testing.T) {
	db := newCountingDatabase(t)
	rest := newRest(t, db)

	for _, request := range []string{
		"GET /given/public/posts?_join=inner:pg_catalog.pg_class:public.posts.id:$eq:pg_class.oid",
		"GET /given/public/posts?_select=pg_read_file('/etc/passwd')",
		"GET /given/public/posts?_page=nope",
		"GET /given/pg_catalog/pg_authid",
		"GET /postgres/public/posts",
		"GET /given/public/posts?_renderer=xml",
	} {
		rec := serve(rest, request)
		require.GreaterOrEqual(t, rec.Code, 400, request)
		requireNoSecrets(t, db.port, rec.Body.String())
		// The answer names the parameter and the rule, never the value the
		// caller sent, so an error cannot be used to echo bytes back.
		require.NotContains(t, rec.Body.String(), "pg_read_file", request)
		require.NotContains(t, rec.Body.String(), "pg_catalog.pg_class", request)
	}
}

// The one error answer that quotes the Database is #546's: a privilege the
// anonymous role lacks answers 403 in Postgres's own words, which name the
// table and nothing else.
func TestTableRead_theRoleThatCouldNotBeEntered_saysOnlyThat(t *testing.T) {
	db := newCountingDatabase(t)
	rest := newRest(t, db)

	rec := serve(rest, "GET /given/public/posts")
	require.False(t, strings.Contains(rec.Body.String(), "role"),
		"a Database that answered nothing is not a role failure: %s", rec.Body)
}
