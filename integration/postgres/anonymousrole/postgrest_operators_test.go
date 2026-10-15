package anonymousrole_test

// miniship (miniship-cloud#1427): the Supabase SDK's array, range, jsonb and
// full-text filters (`contains`, `containedBy`, `overlaps`, `rangeGt` and the
// rest, `textSearch`), cs, cd, ov, sl, sr, nxl, nxr, adj, fts, plfts, phfts and
// wfts, against a live Postgres. Each is held to the rows a direct SQL query
// returns as the same role, in a filter, under not., inside or=( … ) and on an
// embedded table; a value that does not parse is the Database's own SQLSTATE,
// as PostgREST gives it.
//
// The public repository's docker/tests/database-sdk.test.ts asks the same
// questions with the real SDK through the gateway.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	opsDocs  = "rest_ops_docs"
	opsNotes = "rest_ops_notes"
)

// stageOperators makes a table with every column type the operators read, and
// a table of notes pointing at it, both readable by the reader.
//
//	docs  1: tags {a,b,c}  meta {"k":"v","n":1}      span [1,10)   "The quick brown fox ..."
//	docs  2: tags {b,c}    meta {"k":"w"}            span [10,20)  "A quick red cat"
//	docs  3: tags {c,d}    meta {"n":1,"x":[1,2]}    span [15,30)  "Brown bears eat honey"
//	docs  4: tags {}       meta {}                   span empty    "Foxes and dogs"
//	docs  5: every column NULL
func stageOperators(t *testing.T, db *sql.DB) {
	t.Helper()
	stagePostgREST(t, db)
	drop := fmt.Sprintf(`DROP TABLE IF EXISTS public.%s, public.%s CASCADE;`, opsNotes, opsDocs)
	exec(t, db, drop)
	t.Cleanup(func() { exec(t, db, drop) })
	exec(t, db, fmt.Sprintf(`
		CREATE TABLE public.%[1]s (
			id int PRIMARY KEY, tags text[], meta jsonb, span int4range, body text,
			tsv tsvector GENERATED ALWAYS AS (to_tsvector('english', coalesce(body, ''))) STORED);
		INSERT INTO public.%[1]s (id, tags, meta, span, body) VALUES
			(1, '{a,b,c}', '{"k": "v", "n": 1}', '[1,10)', 'The quick brown fox jumps over the lazy dog'),
			(2, '{b,c}', '{"k": "w"}', '[10,20)', 'A quick red cat'),
			(3, '{c,d}', '{"n": 1, "x": [1, 2]}', '[15,30)', 'Brown bears eat honey'),
			(4, '{}', '{}', 'empty', 'Foxes and dogs'),
			(5, NULL, NULL, NULL, NULL);
		CREATE TABLE public.%[2]s (id int PRIMARY KEY, doc_id int NOT NULL REFERENCES public.%[1]s (id), tags text[]);
		INSERT INTO public.%[2]s VALUES (1, 1, '{x,y}'), (2, 1, '{y}'), (3, 2, '{x}'), (4, 3, '{z}');
		GRANT SELECT ON public.%[1]s, public.%[2]s TO %[3]s, %[4]s, %[5]s;
	`, opsDocs, opsNotes, reader, signedIn, service))
}

// directOpsIDs is the ids a direct SQL query returns from the docs, asked as
// the reader.
func directOpsIDs(t *testing.T, db *sql.DB, where string) []int {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec("SET LOCAL ROLE " + reader)
	require.NoError(t, err)
	rows, err := tx.Query("SELECT id FROM public." + opsDocs + " WHERE " + where + " ORDER BY id")
	require.NoError(t, err)
	defer rows.Close()
	ids := []int{}
	for rows.Next() {
		var id int
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

// directOpsJSON is one value a direct SQL query returns as JSON, asked as the
// reader.
func directOpsJSON(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec("SET LOCAL ROLE " + reader)
	require.NoError(t, err)
	var out string
	require.NoError(t, tx.QueryRow(query).Scan(&out))
	return out
}

func TestPostgREST_theArrayRangeAndFullTextFiltersReturnTheRowsADirectQueryReturns(t *testing.T) {
	cfg, db := needsPostgres(t)
	stageOperators(t, db)
	install := newInstall(t)
	rest := restForTheSDK(t, cfg, install, newProjectPool(t, alias))
	anon := token(t, install.private, reader)

	for _, c := range []struct {
		// param is the query parameter the SDK writes, value its text.
		param, value, where string
	}{
		// contains, containedBy and overlaps on an array, a jsonb value and a range
		{"tags", "cs.{a,b}", "tags @> '{a,b}'"},
		{"tags", "cs.{}", "tags @> '{}'"},
		{"tags", "cd.{a,b,c,d}", "tags <@ '{a,b,c,d}'"},
		{"tags", "ov.{a,d}", "tags && '{a,d}'"},
		{"tags", `cs.{"a b",c}`, `tags @> '{"a b",c}'`},
		{"meta", `cs.{"k":"v"}`, `meta @> '{"k":"v"}'`},
		{"meta", `cs.{"x":[1]}`, `meta @> '{"x":[1]}'`},
		{"meta", `cd.{"k":"v","n":1,"z":0}`, `meta <@ '{"k":"v","n":1,"z":0}'`},
		{"span", "cs.[2,5)", "span @> '[2,5)'"},
		{"span", "cd.[0,25)", "span <@ '[0,25)'"},
		{"span", "ov.[5,12)", "span && '[5,12)'"},
		// the five range comparisons
		{"span", "sl.[20,30)", "span << '[20,30)'"},
		{"span", "sr.[1,5)", "span >> '[1,5)'"},
		{"span", "nxl.[12,14)", "span &> '[12,14)'"},
		{"span", "nxr.[12,14)", "span &< '[12,14)'"},
		{"span", "adj.[10,12)", "span -|- '[10,12)'"},
		// a json path is a jsonb value
		{"meta->x", "cs.[1]", "meta->'x' @> '[1]'"},
		// negation
		{"tags", "not.cs.{a}", "NOT (tags @> '{a}')"},
		{"span", "not.adj.[10,12)", "NOT (span -|- '[10,12)')"},
		{"tsv", "not.fts(english).fox", "NOT (tsv @@ to_tsquery('english', 'fox'))"},
		// full text, with and without the configuration
		{"tsv", "fts.fox", "tsv @@ to_tsquery('fox')"},
		{"tsv", "fts(english).quick & fox", "tsv @@ to_tsquery('english', 'quick & fox')"},
		{"tsv", "fts(english).quick | bears", "tsv @@ to_tsquery('english', 'quick | bears')"},
		{"tsv", "plfts(english).quick fox", "tsv @@ plainto_tsquery('english', 'quick fox')"},
		{"tsv", "plfts.fox", "tsv @@ plainto_tsquery('fox')"},
		{"tsv", "phfts(english).brown fox", "tsv @@ phraseto_tsquery('english', 'brown fox')"},
		{"tsv", "phfts(english).fox brown", "tsv @@ phraseto_tsquery('english', 'fox brown')"},
		{"tsv", `wfts(english).quick -cat`, `tsv @@ websearch_to_tsquery('english', 'quick -cat')`},
		{"tsv", `wfts(english)."brown fox" or honey`, `tsv @@ websearch_to_tsquery('english', '"brown fox" or honey')`},
		{"body", "fts(english).fox", "body @@ to_tsquery('english', 'fox')"},
		{"body", "wfts(simple).Brown", "body @@ websearch_to_tsquery('simple', 'Brown')"},
	} {
		// The SDK's request for one question. Expected: 200 and the ids a
		// direct query returns.
		query := "select=id&" + url.QueryEscape(c.param) + "=" + url.QueryEscape(c.value) + "&order=id"
		status, _, body := ask(t, rest, http.MethodGet, "/rest/v1/"+opsDocs+"?"+query, anon, nil)
		require.Equal(t, http.StatusOK, status, "%s=%s: %s", c.param, c.value, body)
		require.Equal(t, directOpsIDs(t, db, c.where), idsIn(t, body), "%s=%s", c.param, c.value)
	}
}

func TestPostgREST_theArrayRangeAndFullTextFiltersWorkInsideOrAndAnd(t *testing.T) {
	cfg, db := needsPostgres(t)
	stageOperators(t, db)
	install := newInstall(t)
	rest := restForTheSDK(t, cfg, install, newProjectPool(t, alias))
	anon := token(t, install.private, reader)

	for _, c := range []struct{ param, value, where string }{
		// A {…} run keeps its commas and its quotes: an array, and a json object.
		{"or", `(tags.cs.{a,b},tags.cs.{d})`, "tags @> '{a,b}' OR tags @> '{d}'"},
		{"or", `(tags.cs.{a},meta.cs.{"k":"w"})`, `tags @> '{a}' OR meta @> '{"k":"w"}'`},
		{"or", `(meta.cs.{"k":"v","n":1},meta.cd.{})`, `meta @> '{"k":"v","n":1}' OR meta <@ '{}'`},
		// A quoted range keeps its comma, and loses its quotes.
		{"or", `(span.sl."[15,30)",span.adj."[10,12)")`, "span << '[15,30)' OR span -|- '[10,12)'"},
		{"or", `(span.cs."[2,5)",tags.ov.{d})`, "span @> '[2,5)' OR tags && '{d}'"},
		// Full text, with a configuration, next to a filter that has none.
		{"or", `(tsv.fts(english).honey,tsv.plfts(english).red cat,tags.cs.{a})`,
			"tsv @@ to_tsquery('english', 'honey') OR tsv @@ plainto_tsquery('english', 'red cat') OR tags @> '{a}'"},
		{"and", `(tsv.wfts(english).quick,tags.cs.{b},span.not.adj."[0,1)")`,
			"tsv @@ websearch_to_tsquery('english', 'quick') AND tags @> '{b}' AND NOT (span -|- '[0,1)')"},
		// Nested, and negated as a group.
		{"or", `(and(tags.cs.{b},tags.cd.{a,b,c}),not.or(tags.ov.{a,b,c},tags.is.null))`,
			"(tags @> '{b}' AND tags <@ '{a,b,c}') OR NOT (tags && '{a,b,c}' OR tags IS NULL)"},
		{"or", `(and(tsv.fts(english).fox,meta.cs.{"n":1}),span.sr."[25,26)")`,
			"(tsv @@ to_tsquery('english', 'fox') AND meta @> '{\"n\":1}') OR span >> '[25,26)'"},
		{"not.or", `(tags.cs.{a},tags.cs.{d})`, "NOT (tags @> '{a}' OR tags @> '{d}')"},
	} {
		query := "select=id&" + c.param + "=" + url.QueryEscape(c.value) + "&order=id"
		status, _, body := ask(t, rest, http.MethodGet, "/rest/v1/"+opsDocs+"?"+query, anon, nil)
		require.Equal(t, http.StatusOK, status, "%s=%s: %s", c.param, c.value, body)
		require.Equal(t, directOpsIDs(t, db, c.where), idsIn(t, body), "%s=%s", c.param, c.value)
	}
}

func TestPostgREST_theArrayFiltersWorkOnAnEmbeddedTable(t *testing.T) {
	cfg, db := needsPostgres(t)
	stageOperators(t, db)
	install := newInstall(t)
	rest := restForTheSDK(t, cfg, install, newProjectPool(t, alias))
	anon := token(t, install.private, reader)

	nested := func(notes string) string {
		return `SELECT coalesce(json_agg(json_build_object('id', d.id, '` + opsNotes + `',
			(SELECT coalesce(json_agg(json_build_object('id', n.id) ORDER BY n.id), '[]'::json)
			   FROM public.` + opsNotes + ` n WHERE n.doc_id = d.id AND (` + notes + `)))
			ORDER BY d.id), '[]'::json)::text FROM public.` + opsDocs + ` d`
	}
	inner := func(notes string) string {
		return `SELECT coalesce(json_agg(json_build_object('id', d.id, '` + opsNotes + `',
			(SELECT coalesce(json_agg(json_build_object('id', n.id) ORDER BY n.id), '[]'::json)
			   FROM public.` + opsNotes + ` n WHERE n.doc_id = d.id AND (` + notes + `)))
			ORDER BY d.id), '[]'::json)::text FROM public.` + opsDocs + ` d
			WHERE EXISTS (SELECT FROM public.` + opsNotes + ` n WHERE n.doc_id = d.id AND (` + notes + `))`
	}

	// The embed is named by its table, rest_ops_notes.
	n := opsNotes
	for _, c := range []struct{ query, expected string }{
		// A to-many embed filtered by an array operator keeps every parent.
		{"select=id," + n + "(id)&" + n + ".order=id&" + n + ".tags=cs.{x}", nested("n.tags @> '{x}'")},
		{"select=id," + n + "(id)&" + n + ".order=id&" + n + ".tags=ov.{y,z}", nested("n.tags && '{y,z}'")},
		{"select=id," + n + "(id)&" + n + ".order=id&" + n + ".tags=not.cd.{x,y}", nested("NOT (n.tags <@ '{x,y}')")},
		// The same under !inner drops the parents that have none.
		{"select=id," + n + "!inner(id)&" + n + ".order=id&" + n + ".tags=cs.{x}", inner("n.tags @> '{x}'")},
		// An or on the embedded table.
		{"select=id," + n + "(id)&" + n + ".order=id&" + n + ".or=(tags.cs.{x},tags.cs.{z})", nested("n.tags @> '{x}' OR n.tags @> '{z}'")},
	} {
		// The filter's value is written as the SDK writes it: {…} in the URL.
		path := "/rest/v1/" + opsDocs + "?" + c.query
		status, _, body := ask(t, rest, http.MethodGet, path, anon, nil)
		require.Equal(t, http.StatusOK, status, "%s: %s", c.query, body)
		require.JSONEq(t, directOpsJSON(t, db, c.expected), body, c.query)
	}
}

func TestPostgREST_aValueThatDoesNotParseIsTheDatabasesErrorCode(t *testing.T) {
	cfg, db := needsPostgres(t)
	stageOperators(t, db)
	install := newInstall(t)
	rest := restForTheSDK(t, cfg, install, newProjectPool(t, alias))
	anon := token(t, install.private, reader)

	for _, c := range []struct {
		param, value string
		status       int
		code         string
	}{
		// A malformed array, json value and range literal: invalid text representation.
		{"tags", "cs.abc", http.StatusBadRequest, "22P02"},
		{"tags", "ov.{a,", http.StatusBadRequest, "22P02"},
		{"meta", "cs.{not json}", http.StatusBadRequest, "22P02"},
		{"span", "adj.foo", http.StatusBadRequest, "22P02"},
		{"span", "sl.[5,1)", http.StatusBadRequest, "22000"},
		// An operator the column's type does not have: undefined function, 404.
		{"id", "cs.1", http.StatusNotFound, "42883"},
		{"body", "adj.x", http.StatusNotFound, "42883"},
		// A query the Database cannot read, or a configuration it does not have.
		{"tsv", "fts.a b", http.StatusBadRequest, "42601"},
		{"tsv", "fts(nosuchconfig).fox", http.StatusBadRequest, "42704"},
		// What PostgREST's own parser refuses.
		{"tsv", "fts().fox", http.StatusBadRequest, "PGRST100"},
		{"tsv", "fts(english", http.StatusBadRequest, "PGRST100"},
		{"tags", "overlaps.{a}", http.StatusBadRequest, "PGRST100"},
	} {
		status, _, body := ask(t, rest, http.MethodGet,
			"/rest/v1/"+opsDocs+"?select=id&"+c.param+"="+url.QueryEscape(c.value), anon, nil)
		var answer map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &answer), body)
		require.Equal(t, c.status, status, "%s=%s: %s", c.param, c.value, body)
		require.Equal(t, c.code, answer["code"], "%s=%s: %s", c.param, c.value, body)
		for _, field := range []string{"code", "message", "details", "hint"} {
			require.Contains(t, answer, field, body)
		}
	}
}
