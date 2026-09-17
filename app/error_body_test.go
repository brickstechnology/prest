package app_test

// miniship (#549): what an error answer may say. #545 found that pREST returns
// the driver's error verbatim, the Database's host and port included. An error
// answer from rest carries no SQL text, no connection string, no host name and
// no port; the detail is logged where only an operator reads it.

import (
	"net"
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

// A Database that is not listening at all: the connection is refused by the
// operating system, which is the shape the ticket names. Upstream answered
// this one with `dial tcp 127.0.0.1:<port>: connect: connection refused`.
func TestTableRead_aDatabaseThatIsNotThere_saysNothingAboutWhereItIsNot(t *testing.T) {
	// A port nothing is listening on: taken and given back.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	rest := newRest(t, &countingDatabase{port: port})

	rec := serve(rest, "GET /given/public/posts")
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	requireNoSecrets(t, port, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "refused")
	require.NotContains(t, rec.Body.String(), "connect")
}

// A Database that answered nothing is not a role failure, so an operator
// reading the answer is not sent to look at the grants.
func TestTableRead_aDeadDatabaseIsNotReportedAsARoleFailure(t *testing.T) {
	db := newCountingDatabase(t)
	rest := newRest(t, db)

	rec := serve(rest, "GET /given/public/posts")
	require.False(t, strings.Contains(rec.Body.String(), "role"),
		"a Database that answered nothing is not a role failure: %s", rec.Body)
}

// The query string longer than rest reads, through the composed stack.
func TestTableRead_aQueryStringTooLongIsRefusedWithoutADatabase(t *testing.T) {
	db := newCountingDatabase(t)
	rest := newRest(t, db)

	filters := make([]string, 0, 4000)
	for i := range 4000 {
		filters = append(filters, "column_"+strconv.Itoa(i)+"=$eq."+strconv.Itoa(i))
	}
	rec := serve(rest, "GET /given/public/posts?"+strings.Join(filters, "&"))
	require.Equal(t, http.StatusRequestURITooLong, rec.Code, rec.Body.String())
	requireNoSecrets(t, db.port, rec.Body.String())
	db.requireNoConnection(t)
}
