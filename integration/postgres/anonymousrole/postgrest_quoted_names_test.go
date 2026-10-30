package anonymousrole_test

// miniship (miniship-cloud#1890): a double-quoted name in a read or a write,
// and the Database's own words in a failed read, against a live Postgres. The
// expectations are what PostgREST v14.18 answered for the same requests on the
// same table, except that a missing column is named under rest's table alias
// ("t"), where PostgREST names the table.

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

const quoted = "rest_pgr_quoted"

// A column with a space, one with a `;` and both kinds of quote, and a jsonb
// column whose keys read as a number and as two words.
const (
	spaced   = "my col"
	nasty    = `a;b"c`
	nastyDDL = `"a;b""c"`
)

func stageQuoted(t *testing.T, f notesFixture) {
	t.Helper()
	drop := fmt.Sprintf(`DROP TABLE IF EXISTS public.%s;`, quoted)
	exec(t, f.db, drop)
	t.Cleanup(func() { exec(t, f.db, drop) })
	exec(t, f.db, fmt.Sprintf(`
		CREATE TABLE public.%[1]s (
			id int PRIMARY KEY, name text, "my col" text UNIQUE, d int, score numeric, %[4]s text, j jsonb);
		INSERT INTO public.%[1]s VALUES
			(1, 'Japan',  'mc1', 1, 91,   'q1', '{"0": "zero", "a b": "two words"}'),
			(2, 'Laos',   'mc2', 2, NULL, 'q2', '{}'),
			(3, 'France', 'mc3', 3, 85.25, 'q3', '{}');
		GRANT SELECT ON public.%[1]s TO %[2]s;
		GRANT SELECT, INSERT, UPDATE ON public.%[1]s TO %[3]s;
	`, quoted, reader, service, nastyDDL))
}

func quotedFixture(t *testing.T) notesFixture {
	t.Helper()
	f := newNotesFixture(t)
	stageQuoted(t, f)
	return f
}

func TestPostgREST_aQuotedNameIsReadWhereverAColumnMayBeNamed(t *testing.T) {
	f := quotedFixture(t)
	path := func(query url.Values) string { return "/rest/v1/" + quoted + "?" + query.Encode() }

	for _, c := range []struct {
		name  string
		query url.Values
		want  string
	}{
		// Q-02, as supabase-js sends it
		{"select with a quoted column", url.Values{"select": {`name, "my col" ,d`}, "order": {"id"}},
			`[{"name":"Japan","my col":"mc1","d":1},{"name":"Laos","my col":"mc2","d":2},{"name":"France","my col":"mc3","d":3}]`},
		{"an alias and a quoted alias", url.Values{"select": {`other:"my col","an alias":name`}, "id": {"eq.1"}},
			`[{"other":"mc1","an alias":"Japan"}]`},
		{"a cast", url.Values{"select": {`"my col"::text`}, "id": {"eq.2"}}, `[{"my col":"mc2"}]`},
		{"a name with a ; and a \"", url.Values{"select": {`"a;b\"c"`}, "id": {"eq.1"}}, `[{"a;b\"c":"q1"}]`},
		{"a quoted json key stays a key", url.Values{"select": {`j->"0",j->>"a b"`}, "id": {"eq.1"}},
			`[{"0":"zero","a b":"two words"}]`},
		{"a filter on a quoted column", url.Values{"select": {"id"}, `"my col"`: {"eq.mc2"}}, `[{"id":2}]`},
		{"a filter on the nasty column", url.Values{"select": {"id"}, `"a;b\"c"`: {"eq.q3"}}, `[{"id":3}]`},
		{"an or group of quoted columns", url.Values{"select": {"id"}, "or": {`("my col".eq.mc1,"a;b\"c".eq.q3)`}, "order": {"id"}},
			`[{"id":1},{"id":3}]`},
		{"an order by a quoted column", url.Values{"select": {"id"}, "order": {`"my col".desc`}},
			`[{"id":3},{"id":2},{"id":1}]`},
	} {
		status, _, body := ask(t, f.rest, http.MethodGet, path(c.query), f.svc, nil)
		require.Equal(t, http.StatusOK, status, "%s: %s", c.name, body)
		require.JSONEq(t, c.want, body, c.name)
	}
}

func TestPostgREST_aQuotedNameCannotCarrySQL(t *testing.T) {
	f := quotedFixture(t)

	// A quote that closes the name early leaves text behind it: refused before any SQL.
	for _, query := range []url.Values{
		{"select": {`"name"; DROP TABLE ` + quoted + `; --`}},
		{"select": {`"my col"x`}},
		{"select": {`"my col`}},
		{"select": {"id"}, "order": {`"id"; DROP TABLE ` + quoted}},
	} {
		status, _, body := ask(t, f.rest, http.MethodGet, "/rest/v1/"+quoted+"?"+query.Encode(), f.svc, nil)
		require.Equal(t, http.StatusBadRequest, status, "%v: %s", query, body)
		require.Equal(t, "PGRST100", refusalOf(t, body)["code"], body)
	}

	// An escaped quote and a ; inside the name are the name: the Database finds
	// no such column and says so, and the table is still there.
	status, _, body := ask(t, f.rest, http.MethodGet, "/rest/v1/"+quoted+"?"+url.Values{"select": {`"x\"; DROP TABLE ` + quoted + `; --"`}}.Encode(), f.svc, nil)
	require.Equal(t, http.StatusBadRequest, status, body)
	got := refusalOf(t, body)
	require.Equal(t, "42703", got["code"], body)
	require.Contains(t, got["message"], "does not exist")
	var count int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM public.`+quoted).Scan(&count))
	require.Equal(t, 3, count)
}

func TestPostgREST_aQuotedNameInAWritesColumnsAndOnConflict(t *testing.T) {
	f := quotedFixture(t)
	status, _, body := send(t, f.rest, http.MethodPost,
		"/rest/v1/"+quoted+"?"+url.Values{"columns": {`"id","my col"`}, "on_conflict": {`"my col"`}}.Encode(),
		f.svc, map[string]string{"Prefer": "resolution=merge-duplicates,return=representation"},
		`[{"id": 9, "my col": "mc9"}, {"id": 10, "my col": "mc1"}]`)
	require.Equal(t, http.StatusCreated, status, body)
	var name string
	require.NoError(t, f.db.QueryRow(`SELECT name FROM public.`+quoted+` WHERE "my col" = 'mc1'`).Scan(&name))
	require.Equal(t, "Japan", name, "the conflict on the quoted column updated the row it names")
	var id int
	require.NoError(t, f.db.QueryRow(`SELECT id FROM public.`+quoted+` WHERE "my col" = 'mc1'`).Scan(&id))
	require.Equal(t, 10, id)
}

func TestPostgREST_theDatabasesOwnWordsAreWhatAFailedReadSays(t *testing.T) {
	f := quotedFixture(t)

	for _, c := range []struct {
		name, query string
		status      int
		code        string
		message     string
	}{
		// F-16: `isdistinct.null` is the text 'null' to PostgREST 14, which a numeric column refuses
		{"isdistinct.null", "select=name&score=isdistinct.null", 400, "22P02", `invalid input syntax for type numeric: "null"`},
		{"a mistyped filter", "select=name&id=eq.abc", 400, "22P02", `invalid input syntax for type integer: "abc"`},
		{"a mistyped in list", "select=name&id=in.(1,abc)", 400, "22P02", `invalid input syntax for type integer: "abc"`},
		{"a column that is not there", "select=nope", 400, "42703", `column t.nope does not exist`},
		{"a filter on a column that is not there", "select=id&nope=eq.1", 400, "42703", `column t.nope does not exist`},
		{"an operator the type lacks", "select=id&id=like.1", 404, "42883", `operator does not exist: integer ~~ unknown`},
	} {
		status, _, body := ask(t, f.rest, http.MethodGet, "/rest/v1/"+quoted+"?"+c.query, f.svc, nil)
		require.Equal(t, c.status, status, "%s: %s", c.name, body)
		got := refusalOf(t, body)
		require.Equal(t, c.code, got["code"], c.name)
		require.Equal(t, c.message, got["message"], c.name)
	}

	// isdistinct with a value still keeps the NULL row
	status, _, body := ask(t, f.rest, http.MethodGet, "/rest/v1/"+quoted+"?select=name&score=isdistinct.91&order=id", f.svc, nil)
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `[{"name":"Laos"},{"name":"France"}]`, body)
}
