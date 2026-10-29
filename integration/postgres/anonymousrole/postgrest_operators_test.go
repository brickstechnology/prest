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
	"strings"
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
	anon := projectToken(t, install.private, reader, alias)

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
	anon := projectToken(t, install.private, reader, alias)

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
	anon := projectToken(t, install.private, reader, alias)

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
	anon := projectToken(t, install.private, reader, alias)

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

// ---------------------------------------------------------------------------
// miniship-cloud#1887: the whole of PostgREST 14's filter language.

const (
	fltCountries = "rest_flt_countries"
	fltCities    = "rest_flt_cities"
)

// stageFilters makes the table every filter operator is held against, and a
// table of cities pointing at it.
//
//	countries 1 Japan         JP 125000000 true
//	countries 2 Thailand      TH  70000000 true
//	countries 3 Jamaica       JM   3000000 false
//	countries 4 Laos          LA   7500000 NULL
//	countries 5 La Paz, Land  LP   8000000 true
//	countries 6 Cote (Ivory)  CI      1000 false
//	countries 7 every column NULL
func stageFilters(t *testing.T, db *sql.DB) {
	t.Helper()
	stagePostgREST(t, db)
	drop := fmt.Sprintf(`DROP TABLE IF EXISTS public.%s, public.%s CASCADE;`, fltCities, fltCountries)
	exec(t, db, drop)
	t.Cleanup(func() { exec(t, db, drop) })
	exec(t, db, fmt.Sprintf(`
		CREATE TABLE public.%[1]s (id int PRIMARY KEY, name text, code text, population int, active bool);
		INSERT INTO public.%[1]s VALUES
			(1, 'Japan', 'JP', 125000000, true),
			(2, 'Thailand', 'TH', 70000000, true),
			(3, 'Jamaica', 'JM', 3000000, false),
			(4, 'Laos', 'LA', 7500000, NULL),
			(5, 'La Paz, Land', 'LP', 8000000, true),
			(6, 'Cote (Ivory)', 'CI', 1000, false),
			(7, NULL, NULL, NULL, NULL);
		CREATE TABLE public.%[2]s (id int PRIMARY KEY, country_id int NOT NULL REFERENCES public.%[1]s (id), name text, population int);
		INSERT INTO public.%[2]s VALUES
			(1, 1, 'Tokyo', 14000000), (2, 1, 'Osaka', 2700000), (3, 2, 'Bangkok', 10000000),
			(4, 3, 'Kingston', 600000), (5, 4, 'Vientiane', 900000), (6, 5, 'La Paz', 800000);
		GRANT SELECT ON public.%[1]s, public.%[2]s TO %[3]s, %[4]s, %[5]s;
	`, fltCountries, fltCities, reader, signedIn, service))
}

// directFilterIDs is the ids a direct SQL query returns from the countries,
// asked as the reader.
func directFilterIDs(t *testing.T, db *sql.DB, where string) []int {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec("SET LOCAL ROLE " + reader)
	require.NoError(t, err)
	rows, err := tx.Query("SELECT id FROM public." + fltCountries + " WHERE " + where + " ORDER BY id")
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

func TestPostgREST_everyFilterOperatorReturnsTheRowsADirectQueryReturns(t *testing.T) {
	cfg, db := needsPostgres(t)
	stageFilters(t, db)
	install := newInstall(t)
	rest := restForTheSDK(t, cfg, install, newProjectPool(t, alias))
	anon := projectToken(t, install.private, reader, alias)

	for _, c := range []struct{ param, value, where string }{
		// the comparisons
		{"code", "eq.TH", "code = 'TH'"},
		{"code", "neq.TH", "code <> 'TH'"},
		{"population", "gt.7500000", "population > 7500000"},
		{"population", "gte.7500000", "population >= 7500000"},
		{"population", "lt.7500000", "population < 7500000"},
		{"population", "lte.7500000", "population <= 7500000"},
		{"name", "like.La*", "name LIKE 'La%'"},
		{"name", "ilike.*AN*", "name ILIKE '%AN%'"},
		{"name", "not.like.La*", "NOT (name LIKE 'La%')"},
		{"code", "not.eq.TH", "NOT (code = 'TH')"},
		// likeAllOf, likeAnyOf, ilikeAllOf and ilikeAnyOf, values double quoted
		{"name", `like(all).{"%a%","%n%"}`, "name LIKE ALL('{\"%a%\",\"%n%\"}')"},
		{"name", `like(any).{"Ja%","La%"}`, "name LIKE ANY('{\"Ja%\",\"La%\"}')"},
		{"name", `ilike(all).{"%A%","%N%"}`, "name ILIKE ALL('{\"%A%\",\"%N%\"}')"},
		{"name", `ilike(any).{"ja%","la%"}`, "name ILIKE ANY('{\"ja%\",\"la%\"}')"},
		{"name", "like(any).{Ja%,La%}", "name LIKE ANY('{Ja%,La%}')"},
		{"name", "like(any).{Ja*,La*}", "name LIKE ANY('{Ja%,La%}')"},
		{"name", "ilike(any).{ja%,la%}", "name ILIKE ANY('{ja%,la%}')"},
		{"name", "ilike(all).{%a%,%n%}", "name ILIKE ALL('{%a%,%n%}')"},
		{"name", `not.like(all).{"%a%","%n%"}`, "NOT (name LIKE ALL('{\"%a%\",\"%n%\"}'))"},
		// the other quantified operators
		{"code", "eq(any).{TH,JP}", "code = ANY('{TH,JP}')"},
		{"code", "eq(all).{TH,JP}", "code = ALL('{TH,JP}')"},
		{"code", "eq(all).{TH}", "code = ALL('{TH}')"},
		{"population", "gt(all).{1,8000000}", "population > ALL('{1,8000000}')"},
		{"population", "gt(any).{1,8000000}", "population > ANY('{1,8000000}')"},
		{"population", "gte(any).{8000000,70000000}", "population >= ANY('{8000000,70000000}')"},
		{"population", "gte(all).{8000000,70000000}", "population >= ALL('{8000000,70000000}')"},
		{"population", "lt(any).{1000,3000000}", "population < ANY('{1000,3000000}')"},
		{"population", "lt(all).{3000000,8000000}", "population < ALL('{3000000,8000000}')"},
		{"population", "lte(any).{1000,3000000}", "population <= ANY('{1000,3000000}')"},
		{"population", "lte(all).{3000000,8000000}", "population <= ALL('{3000000,8000000}')"},
		{"population", "not.gt(all).{1,8000000}", "NOT (population > ALL('{1,8000000}'))"},
		// regexMatch and regexIMatch
		{"name", "match.^J", "name ~ '^J'"},
		{"name", "match.n$", "name ~ 'n$'"},
		{"name", "match.^[JT]", "name ~ '^[JT]'"},
		{"name", "imatch.^j", "name ~* '^j'"},
		{"name", "imatch.LAND$", "name ~* 'LAND$'"},
		{"name", "not.match.^J", "NOT (name ~ '^J')"},
		{"name", "not.imatch.^j", "NOT (name ~* '^j')"},
		{"name", `match(any).{"^J","^T"}`, "name ~ ANY('{\"^J\",\"^T\"}')"},
		{"name", `match(all).{"a","n"}`, "name ~ ALL('{\"a\",\"n\"}')"},
		{"name", `imatch(any).{"^j","^t"}`, "name ~* ANY('{\"^j\",\"^t\"}')"},
		{"name", `imatch(all).{"A","N"}`, "name ~* ALL('{\"A\",\"N\"}')"},
		// isDistinct, which sees a NULL as a value
		{"population", "isdistinct.3000000", "population IS DISTINCT FROM 3000000"},
		{"code", "isdistinct.JP", "code IS DISTINCT FROM 'JP'"},
		{"code", "not.isdistinct.JP", "NOT (code IS DISTINCT FROM 'JP')"},
		// is: every value, in any case
		{"active", "is.null", "active IS NULL"},
		{"active", "is.NULL", "active IS NULL"},
		{"active", "is.not_null", "active IS NOT NULL"},
		{"active", "is.NOT_NULL", "active IS NOT NULL"},
		{"active", "is.true", "active IS TRUE"},
		{"active", "is.TRUE", "active IS TRUE"},
		{"active", "is.false", "active IS FALSE"},
		{"active", "is.unknown", "active IS UNKNOWN"},
		{"active", "not.is.null", "NOT (active IS NULL)"},
		{"active", "not.is.not_null", "NOT (active IS NOT NULL)"},
		{"active", "not.is.true", "NOT (active IS TRUE)"},
		{"active", "not.is.unknown", "NOT (active IS UNKNOWN)"},
		// in, notIn: quoted values with commas and parentheses
		{"id", "in.(1,3,5)", "id IN (1,3,5)"},
		{"id", "not.in.(1,3,5)", "id NOT IN (1,3,5)"},
		{"code", "in.(TH,JP)", "code IN ('TH','JP')"},
		{"code", `in.("TH","JP")`, "code IN ('TH','JP')"},
		{"name", `in.("La Paz, Land","Cote (Ivory)")`, "name IN ('La Paz, Land','Cote (Ivory)')"},
		// a NULL name is neither in the list nor not in it, as for a direct query
		{"name", `not.in.(Japan,"La Paz, Land")`, "name NOT IN ('Japan','La Paz, Land')"},
		{"name", `in.("Cote (Ivory)")`, "name = 'Cote (Ivory)'"},
		{"name", `in.("say \"hi\"",Japan)`, "name IN ('say \"hi\"','Japan')"},
		{"id", "in.()", "false"},
		{"id", "not.in.()", "true"},
		{"id", "in.(99)", "false"},
	} {
		query := "select=id&" + url.QueryEscape(c.param) + "=" + url.QueryEscape(c.value) + "&order=id"
		status, _, body := ask(t, rest, http.MethodGet, "/rest/v1/"+fltCountries+"?"+query, anon, nil)
		require.Equal(t, http.StatusOK, status, "%s=%s: %s", c.param, c.value, body)
		require.Equal(t, directFilterIDs(t, db, c.where), idsIn(t, body), "%s=%s", c.param, c.value)
	}
}

func TestPostgREST_everyFilterOperatorWorksInsideOrAndAnd(t *testing.T) {
	cfg, db := needsPostgres(t)
	stageFilters(t, db)
	install := newInstall(t)
	rest := restForTheSDK(t, cfg, install, newProjectPool(t, alias))
	anon := projectToken(t, install.private, reader, alias)

	for _, c := range []struct{ param, value, where string }{
		{"or", `(code.eq.TH,code.eq.JP)`, "code = 'TH' OR code = 'JP'"},
		{"or", `(name.like(all).{"%a%","%n%"},code.eq.LP)`, "name LIKE ALL('{\"%a%\",\"%n%\"}') OR code = 'LP'"},
		{"or", `(name.like(any).{"Ja%","La%"},population.gt.100000000)`, "name LIKE ANY('{\"Ja%\",\"La%\"}') OR population > 100000000"},
		{"or", `(name.ilike(any).{ja%,th%},code.eq.CI)`, "name ILIKE ANY('{ja%,th%}') OR code = 'CI'"},
		{"or", `(name.ilike(all).{"%A%","%N%"},code.eq.CI)`, "name ILIKE ALL('{\"%A%\",\"%N%\"}') OR code = 'CI'"},
		{"or", `(code.eq(any).{TH,JP},id.eq.6)`, "code = ANY('{TH,JP}') OR id = 6"},
		{"or", `(population.gt(all).{1,8000000},id.eq.6)`, "population > ALL('{1,8000000}') OR id = 6"},
		{"or", `(population.lte(any).{1000,3000000},id.eq.1)`, "population <= ANY('{1000,3000000}') OR id = 1"},
		{"or", `(name.match.^J,name.imatch.^t)`, "name ~ '^J' OR name ~* '^t'"},
		{"or", `(name.match(any).{"^J","^T"},id.eq.6)`, "name ~ ANY('{\"^J\",\"^T\"}') OR id = 6"},
		{"or", `(name.imatch(all).{"A","N"},id.eq.6)`, "name ~* ALL('{\"A\",\"N\"}') OR id = 6"},
		{"or", `(code.isdistinct.JP,id.eq.7)`, "code IS DISTINCT FROM 'JP' OR id = 7"},
		{"or", `(name.not.in.(Japan,"La Paz, Land"),id.eq.1)`, "name NOT IN ('Japan','La Paz, Land') OR id = 1"},
		{"or", `(name.in.("La Paz, Land","Cote (Ivory)"),id.eq.1)`, "name IN ('La Paz, Land','Cote (Ivory)') OR id = 1"},
		{"or", `(active.is.not_null,id.eq.7)`, "active IS NOT NULL OR id = 7"},
		{"or", `(active.is.unknown,active.is.false)`, "active IS UNKNOWN OR active IS FALSE"},
		{"or", `(id.in.(),id.eq.2)`, "false OR id = 2"},
		{"and", `(name.like(all).{"%a%","%n%"},population.gt.5000000)`, "name LIKE ALL('{\"%a%\",\"%n%\"}') AND population > 5000000"},
		{"and", `(code.eq(any).{TH,JP,LA},active.is.true)`, "code = ANY('{TH,JP,LA}') AND active IS TRUE"},
		{"not.or", `(code.eq.TH,code.eq.JP)`, "NOT (code = 'TH' OR code = 'JP')"},
		{"not.and", `(code.eq.TH,population.gt.1)`, "NOT (code = 'TH' AND population > 1)"},
		{"or", `(and(code.eq(any).{TH,JP},population.gt.100000000),not.or(name.match.^L,active.is.null))`,
			"(code = ANY('{TH,JP}') AND population > 100000000) OR NOT (name ~ '^L' OR active IS NULL)"},
		{"or", `(not.and(name.like(all).{"%a%","%n%"},code.neq.JM),code.eq.CI)`,
			"NOT (name LIKE ALL('{\"%a%\",\"%n%\"}') AND code <> 'JM') OR code = 'CI'"},
		{"or", `(name.eq."La Paz, Land",name.eq."Cote (Ivory)")`, "name = 'La Paz, Land' OR name = 'Cote (Ivory)'"},
		{"or", `(code.not.eq.TH,code.not.is.null)`, "NOT (code = 'TH') OR NOT (code IS NULL)"},
	} {
		query := "select=id&" + c.param + "=" + url.QueryEscape(c.value) + "&order=id"
		status, _, body := ask(t, rest, http.MethodGet, "/rest/v1/"+fltCountries+"?"+query, anon, nil)
		require.Equal(t, http.StatusOK, status, "%s=%s: %s", c.param, c.value, body)
		require.Equal(t, directFilterIDs(t, db, c.where), idsIn(t, body), "%s=%s", c.param, c.value)
	}
}

func TestPostgREST_everyFilterOperatorWorksOnAnEmbeddedTable(t *testing.T) {
	cfg, db := needsPostgres(t)
	stageFilters(t, db)
	install := newInstall(t)
	rest := restForTheSDK(t, cfg, install, newProjectPool(t, alias))
	anon := projectToken(t, install.private, reader, alias)

	nested := func(cities string) string {
		return `SELECT coalesce(json_agg(json_build_object('id', d.id, '` + fltCities + `',
			(SELECT coalesce(json_agg(json_build_object('id', n.id) ORDER BY n.id), '[]'::json)
			   FROM public.` + fltCities + ` n WHERE n.country_id = d.id AND (` + cities + `)))
			ORDER BY d.id), '[]'::json)::text FROM public.` + fltCountries + ` d`
	}
	c := fltCities
	base := "select=id," + c + "(id)&" + c + ".order=id&"
	for _, tc := range []struct{ query, where string }{
		{c + ".name=like(any).{To*,Os*}", "n.name LIKE ANY('{To%,Os%}')"},
		{c + `.name=like(all).{"%o%","%k%"}`, `n.name LIKE ALL('{"%o%","%k%"}')`},
		{c + ".name=ilike(any).{to%,kin%}", "n.name ILIKE ANY('{to%,kin%}')"},
		{c + ".name=ilike(all).{%O%,%A%}", "n.name ILIKE ALL('{%O%,%A%}')"},
		{c + ".population=gt(all).{1000000,5000000}", "n.population > ALL('{1000000,5000000}')"},
		{c + ".population=lt(any).{700000,900000}", "n.population < ANY('{700000,900000}')"},
		{c + ".id=eq(any).{1,3,5}", "n.id = ANY('{1,3,5}')"},
		{c + ".name=match.^T", "n.name ~ '^T'"},
		{c + ".name=imatch.^k", "n.name ~* '^k'"},
		{c + ".name=match(any).{^T,^B}", "n.name ~ ANY('{^T,^B}')"},
		{c + ".id=isdistinct.1", "n.id IS DISTINCT FROM 1"},
		{c + ".name=not.in.(Tokyo,\"La Paz\")", "n.name NOT IN ('Tokyo','La Paz')"},
		{c + ".name=in.(Tokyo,\"La Paz\")", "n.name IN ('Tokyo','La Paz')"},
		{c + ".name=is.not_null", "n.name IS NOT NULL"},
		{c + ".name=not.like(all).{*o*,*k*}", "NOT (n.name LIKE ALL('{%o%,%k%}'))"},
		{c + `.or=(name.like(all).{"%o%","%k%"},name.eq.Osaka)`, `n.name LIKE ALL('{"%o%","%k%"}') OR n.name = 'Osaka'`},
		{c + ".or=(name.match.^T,population.gt(any).{5000000,9000000})", "n.name ~ '^T' OR n.population > ANY('{5000000,9000000}')"},
		{c + ".not.or=(name.imatch.^t,id.in.(2,3))", "NOT (n.name ~* '^t' OR n.id = ANY('{2,3}'))"},
		{c + ".and=(name.isdistinct.Tokyo,population.gt.1000000)", "n.name IS DISTINCT FROM 'Tokyo' AND n.population > 1000000"},
	} {
		key, value, _ := strings.Cut(tc.query, "=")
		path := "/rest/v1/" + fltCountries + "?" + base + key + "=" + url.QueryEscape(value)
		status, _, body := ask(t, rest, http.MethodGet, path, anon, nil)
		require.Equal(t, http.StatusOK, status, "%s: %s", tc.query, body)
		require.JSONEq(t, directOpsJSON(t, db, nested(tc.where)), body, tc.query)
	}
}

// A value is a parameter and nothing else: an injection in a list, in a
// quantified list or in a group's value is data to look for, and the table
// stays.
func TestPostgREST_anInjectionInAFilterValueIsDataToLookFor(t *testing.T) {
	cfg, db := needsPostgres(t)
	stageFilters(t, db)
	install := newInstall(t)
	rest := restForTheSDK(t, cfg, install, newProjectPool(t, alias))
	anon := projectToken(t, install.private, reader, alias)

	attack := `x'); DROP TABLE public.` + fltCountries + `; --`
	for _, c := range []struct{ param, value string }{
		{"name", `in.("` + attack + `",Japan)`},
		{"name", `not.in.("` + attack + `")`},
		{"name", `in.(Japan,` + attack + `)`},
		{"name", `like(any).{"` + attack + `"}`},
		{"name", `match(all).{"` + attack + `"}`},
		{"name", `isdistinct.` + attack},
		{"name", `eq(any).{"` + attack + `"}`},
		{"or", `(name.in.("` + attack + `",Japan),code.eq.TH)`},
		{"or", `(name.like(all).{"` + attack + `"},code.eq.TH)`},
	} {
		query := "select=id&" + c.param + "=" + url.QueryEscape(c.value) + "&order=id"
		status, _, body := ask(t, rest, http.MethodGet, "/rest/v1/"+fltCountries+"?"+query, anon, nil)
		if strings.HasPrefix(c.value, "match(") {
			// The text is a regular expression, and the Database refuses it as one.
			require.Equal(t, http.StatusBadRequest, status, "%s=%s: %s", c.param, c.value, body)
			require.Contains(t, body, "2201B")
		} else {
			require.Equal(t, http.StatusOK, status, "%s=%s: %s", c.param, c.value, body)
			_ = idsIn(t, body)
		}
		var count int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM public.`+fltCountries).Scan(&count), "the table is gone after %s=%s", c.param, c.value)
		require.Equal(t, 7, count)
	}
}

func TestPostgREST_aFormPostgRESTRefusesIsRefusedAsPostgRESTRefusesIt(t *testing.T) {
	cfg, db := needsPostgres(t)
	stageFilters(t, db)
	install := newInstall(t)
	rest := restForTheSDK(t, cfg, install, newProjectPool(t, alias))
	anon := projectToken(t, install.private, reader, alias)

	for _, c := range []struct{ param, value, message, details string }{
		{"name", "foo.x", `"failed to parse filter (foo.x)" (line 1, column 1)`, `unexpected "f" expecting "not" or operator (eq, gt, ...)`},
		{"name", "neq(any).{a}", `"failed to parse filter (neq(any).{a})" (line 1, column 4)`, `unexpected "(" expecting operator (eq, gt, ...)`},
		{"name", "cs(all).{a}", `"failed to parse filter (cs(all).{a})" (line 1, column 3)`, `unexpected "(" expecting operator (eq, gt, ...)`},
		{"id", "in(any).(1)", `"failed to parse filter (in(any).(1))" (line 1, column 3)`, `unexpected "(" expecting operator (eq, gt, ...)`},
		{"id", "is(all).null", `"failed to parse filter (is(all).null)" (line 1, column 3)`, `unexpected "(" expecting operator (eq, gt, ...)`},
		{"name", "eq(foo).x", `"failed to parse filter (eq(foo).x)" (line 1, column 4)`, `unexpected "f" expecting operator (eq, gt, ...)`},
		{"active", "is.maybe", `"failed to parse filter (is.maybe)" (line 1, column 4)`, `unexpected "m" expecting isVal: (null, not_null, true, false, unknown)`},
		{"id", "in.1", `"failed to parse filter (in.1)" (line 1, column 4)`, `unexpected "1" expecting "("`},
		{"id", "not.foo.1", `"failed to parse filter (not.foo.1)" (line 1, column 5)`, `unexpected "f" expecting operator (eq, gt, ...)`},
	} {
		status, _, body := ask(t, rest, http.MethodGet,
			"/rest/v1/"+fltCountries+"?select=id&"+c.param+"="+url.QueryEscape(c.value), anon, nil)
		var answer map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &answer), body)
		require.Equal(t, http.StatusBadRequest, status, "%s=%s: %s", c.param, c.value, body)
		require.Equal(t, "PGRST100", answer["code"], body)
		require.Equal(t, c.message, answer["message"], body)
		require.Equal(t, c.details, answer["details"], body)
	}

	// Inside a group the message names the logic tree.
	status, _, body := ask(t, rest, http.MethodGet,
		"/rest/v1/"+fltCountries+"?select=id&or="+url.QueryEscape(`(id.eq.1,id.foo.2)`), anon, nil)
	var answer map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &answer), body)
	require.Equal(t, http.StatusBadRequest, status, body)
	require.Equal(t, "PGRST100", answer["code"], body)
	require.Equal(t, `"failed to parse logic tree ((id.eq.1,id.foo.2))" (line 1, column 15)`, answer["message"], body)
}
