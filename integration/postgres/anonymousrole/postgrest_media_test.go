package anonymousrole_test

// miniship (miniship-cloud#1886, ADR 0074): the response media type of
// /rest/v1, against a live Postgres. Each subject sends the Accept that
// supabase-js 2.118 sends for .csv(), .geojson(), .stripNulls(), .single() and
// .explain(), and holds the bytes to what PostgREST v14 answers: the expected
// strings below were checked against a PostgREST v14.18 running on the same
// rows (the record text of asCsvF, the 406 PGRST107 of negotiateContent).

import (
	"database/sql"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

const countries = "rest_pgm_countries"

// stageCountries makes the table the media types read: a plain row, a row of
// nulls, and rows that need quoting (a quote and a comma, a newline, a
// backslash, the empty string).
//
//	id  name           code  note        tags   meta            flag
//	1   Thailand       TH    plain       {a,b}  {"k": 1}        true
//	2   Japan          JP    NULL        NULL   NULL            false
//	3   Say "hi", ok   XX    two\nlines  {x}    {"a": [1, 2]}   true
//	4   (empty)        YY    back\slash  NULL   NULL            NULL
func stageCountries(t *testing.T, db *sql.DB) {
	t.Helper()
	// The roles are the caller's: newCallFixture and newNotesFixture stage them.
	drop := fmt.Sprintf(`DROP TABLE IF EXISTS public.%s;`, countries)
	exec(t, db, drop)
	t.Cleanup(func() { exec(t, db, drop) })
	exec(t, db, fmt.Sprintf(`
		CREATE TABLE public.%[1]s (
			id integer PRIMARY KEY, name text, code text, note text, tags text[], meta jsonb, flag boolean);
		INSERT INTO public.%[1]s VALUES
			(1, 'Thailand', 'TH', 'plain', '{a,b}', '{"k": 1}', true),
			(2, 'Japan', 'JP', NULL, NULL, NULL, false),
			(3, 'Say "hi", ok', 'XX', E'two\nlines', '{x}', '{"a": [1, 2]}', true),
			(4, '', 'YY', E'back\\slash', NULL, NULL, NULL);
		GRANT SELECT ON public.%[1]s TO %[2]s, %[3]s, %[4]s;
	`, countries, signedIn, service, reader))
}

func accepting(media string) map[string]string { return map[string]string{"Accept": media} }

func TestPostgRESTMedia_csvIsPostgRESTsCsv(t *testing.T) {
	f := newCallFixture(t)
	stageCountries(t, f.db)

	// .csv() on the benchmark's read. Expected: a header row, then the rows,
	// joined by a newline with none at the end, and the charset.
	status, headers, body := send(t, f.rest, http.MethodGet, "/rest/v1/"+countries+"?select=name,code&order=id&limit=2", f.anon, accepting("text/csv"), "")
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "name,code\nThailand,TH\nJapan,JP", body)
	require.Equal(t, "text/csv; charset=utf-8", headers.Get("Content-Type"))
	require.Equal(t, "0-1/*", headers.Get("Content-Range"))

	// NULL is nothing, a boolean is t or f, an array and jsonb are the
	// Database's text for them, and a field with a quote, a comma, a newline,
	// a backslash or nothing at all is quoted as a record is: quotes and
	// backslashes doubled.
	status, _, body = send(t, f.rest, http.MethodGet, "/rest/v1/"+countries+"?select=id,name,note,tags,meta,flag&order=id", f.anon, accepting("text/csv"), "")
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "id,name,note,tags,meta,flag\n"+
		"1,Thailand,plain,\"{a,b}\",\"{\"\"k\"\": 1}\",t\n"+
		"2,Japan,,,,f\n"+
		"3,\"Say \"\"hi\"\", ok\",\"two\nlines\",{x},\"{\"\"a\"\": [1, 2]}\",t\n"+
		"4,\"\",\"back\\\\slash\",,,", body)

	// The count is answered as it is for JSON.
	_, headers, _ = send(t, f.rest, http.MethodGet, "/rest/v1/"+countries+"?select=id&order=id&limit=2&offset=1", f.anon,
		map[string]string{"Accept": "text/csv", "Prefer": "count=exact"}, "")
	require.Equal(t, "1-2/4", headers.Get("Content-Range"))

	// No row: an empty header and the newline, as asCsvF writes it.
	status, headers, body = send(t, f.rest, http.MethodGet, "/rest/v1/"+countries+"?id=eq.999", f.anon, accepting("text/csv"), "")
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "\n", body)
	require.Equal(t, "*/*", headers.Get("Content-Range"))

	// text/csv among others: the first the server can produce, in quality order.
	_, headers, _ = send(t, f.rest, http.MethodGet, "/rest/v1/"+countries+"?select=id&limit=1", f.anon, accepting("text/plain, text/csv"), "")
	require.Equal(t, "text/csv; charset=utf-8", headers.Get("Content-Type"))

	// HEAD: the headers, no body.
	status, headers, body = send(t, f.rest, http.MethodHead, "/rest/v1/"+countries+"?select=id", f.anon, accepting("text/csv"), "")
	require.Equal(t, http.StatusOK, status)
	require.Empty(t, body)
	require.Equal(t, "text/csv; charset=utf-8", headers.Get("Content-Type"))
}

func TestPostgRESTMedia_csvOfAWriteThatReturnsRows(t *testing.T) {
	f := newNotesFixture(t)
	path := "/rest/v1/" + notes
	csv := map[string]string{"Accept": "text/csv", "Prefer": "return=representation"}

	// insert().select().csv(). Expected: 201 and the rows as CSV.
	status, headers, body := send(t, f.rest, http.MethodPost, path+"?select=id,title,tags,meta", f.svc, csv,
		`[{"title":"c1","tags":["p","q"],"meta":{"k":null}},{"title":"c, 2"}]`)
	require.Equal(t, http.StatusCreated, status, body)
	require.Equal(t, "id,title,tags,meta\n5,c1,\"{p,q}\",\"{\"\"k\"\": null}\"\n6,\"c, 2\",,", body)
	require.Equal(t, "text/csv; charset=utf-8", headers.Get("Content-Type"))

	// update, with the order, and delete.
	status, headers, body = send(t, f.rest, http.MethodPatch, path+"?id=in.(5,6)&select=id,title&order=id.desc", f.svc, csv, `{"body":"edited"}`)
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "id,title\n6,\"c, 2\"\n5,c1", body)
	require.Equal(t, "text/csv; charset=utf-8", headers.Get("Content-Type"))
	status, _, body = send(t, f.rest, http.MethodDelete, path+"?id=eq.5&select=id,title", f.svc, csv, "")
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "id,title\n5,c1", body)

	// Without return=representation there is nothing to format, as before.
	status, _, body = send(t, f.rest, http.MethodPost, path, f.svc, accepting("text/csv"), `{"title":"c3"}`)
	require.Equal(t, http.StatusCreated, status, body)
	require.Empty(t, body)

	// A change that matches no row: the empty header and the newline.
	status, _, body = send(t, f.rest, http.MethodPatch, path+"?id=eq.999&select=id", f.svc, csv, `{"body":"x"}`)
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "\n", body)
}

func TestPostgRESTMedia_csvOfAFunctionCall(t *testing.T) {
	f := newCallFixture(t)

	// A function that returns a table.
	status, headers, body := send(t, f.rest, http.MethodPost, "/rest/v1/rpc/rest_pgc_titles?order=id&limit=2", f.a, accepting("text/csv"), `{"min_id":1}`)
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "id,title\n1,a1\n2,a2", body)
	require.Equal(t, "text/csv; charset=utf-8", headers.Get("Content-Type"))
	require.Equal(t, "0-1/*", headers.Get("Content-Range"))

	// The same as a GET, with a count.
	status, headers, body = send(t, f.rest, http.MethodGet, "/rest/v1/rpc/rest_pgc_titles?min_id=2&order=id.desc&limit=2", f.svc,
		map[string]string{"Accept": "text/csv", "Prefer": "count=exact"}, "")
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "id,title\n4,b2\n3,b1", body)
	require.Equal(t, "0-1/3", headers.Get("Content-Range"))

	// A function that returns one value: PostgREST wraps it in one column,
	// pgrst_scalar, and writes that.
	status, _, body = send(t, f.rest, http.MethodPost, "/rest/v1/rpc/rest_pgc_hello", f.a, accepting("text/csv"), "")
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "pgrst_scalar\nhello", body)
	status, _, body = send(t, f.rest, http.MethodPost, "/rest/v1/rpc/rest_pgc_nums", f.a, accepting("text/csv"), `{"n":3}`)
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "pgrst_scalar\n1\n2\n3", body)

	// A function of one row of a table: its columns.
	status, _, body = send(t, f.rest, http.MethodPost, "/rest/v1/rpc/rest_pgc_first", f.a, accepting("text/csv"), `{"min_id":2}`)
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, fmt.Sprintf("id,title,owner\n2,a2,%s", userA), body)

	// A function that returns nothing is 204 in any media type.
	status, _, body = send(t, f.rest, http.MethodPost, "/rest/v1/rpc/rest_pgc_nothing", f.a, accepting("text/csv"), "")
	require.Equal(t, http.StatusNoContent, status, body)
	require.Empty(t, body)
}

func TestPostgRESTMedia_stripNullsOmitsTheNullKeys(t *testing.T) {
	f := newCallFixture(t)
	stageCountries(t, f.db)
	strip := accepting("application/vnd.pgrst.array+json;nulls=stripped")
	object := accepting("application/vnd.pgrst.object+json;nulls=stripped")

	// .stripNulls(): the array form. Row 2 and row 4 lose their null keys,
	// a key that holds false stays.
	status, headers, body := send(t, f.rest, http.MethodGet, "/rest/v1/"+countries+"?select=id,note,tags,flag&order=id&id=in.(2,4)", f.anon, strip, "")
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `[{"id":2,"flag":false},{"id":4,"note":"back\\slash"}]`, body)
	require.Equal(t, "application/vnd.pgrst.array+json;nulls=stripped; charset=utf-8", headers.Get("Content-Type"))

	// A null inside a value is stripped too, as json_strip_nulls does.
	_, _, body = send(t, f.rest, http.MethodGet, "/rest/v1/"+countries+"?select=id,meta&id=eq.1", f.anon, strip, "")
	require.JSONEq(t, `[{"id":1,"meta":{"k":1}}]`, body)

	// No row: the empty array. A column that is null in every row: the key
	// is gone from every row, and the rows are still there.
	_, _, body = send(t, f.rest, http.MethodGet, "/rest/v1/"+countries+"?id=eq.999", f.anon, strip, "")
	require.Equal(t, "[]", body)
	_, _, body = send(t, f.rest, http.MethodGet, "/rest/v1/"+countries+"?select=meta&id=in.(2,4)&order=id", f.anon, strip, "")
	require.JSONEq(t, `[{},{}]`, body)

	// .single().stripNulls(): the object form.
	status, headers, body = send(t, f.rest, http.MethodGet, "/rest/v1/"+countries+"?select=id,note,tags&id=eq.2", f.anon, object, "")
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `{"id":2}`, body)
	require.Equal(t, "application/vnd.pgrst.object+json;nulls=stripped; charset=utf-8", headers.Get("Content-Type"))

	// The object form with several rows is PGRST116, as it is without a strip.
	status, _, body = send(t, f.rest, http.MethodGet, "/rest/v1/"+countries+"?select=id", f.anon, object, "")
	require.Equal(t, http.StatusNotAcceptable, status, body)
	require.Equal(t, "PGRST116", refusalOf(t, body)["code"])

	// A function's rows, and a write's.
	status, _, body = send(t, f.rest, http.MethodPost, "/rest/v1/rpc/rest_pgc_first", f.a, strip, `{"min_id":2}`)
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, fmt.Sprintf(`{"id":2,"title":"a2","owner":%q}`, userA), body)
	status, headers, body = send(t, f.rest, http.MethodPost, "/rest/v1/rpc/rest_pgc_titles?order=id&limit=1", f.a, strip, `{"min_id":1}`)
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `[{"id":1,"title":"a1"}]`, body)
	require.Equal(t, "application/vnd.pgrst.array+json;nulls=stripped; charset=utf-8", headers.Get("Content-Type"))
}

func TestPostgRESTMedia_stripNullsOfAWrite(t *testing.T) {
	f := newNotesFixture(t)
	path := "/rest/v1/" + notes
	status, headers, body := send(t, f.rest, http.MethodPost, path+"?select=id,title,tags,meta", f.svc,
		map[string]string{"Accept": "application/vnd.pgrst.array+json;nulls=stripped", "Prefer": "return=representation"}, `{"title":"s1","tags":null}`)
	require.Equal(t, http.StatusCreated, status, body)
	require.JSONEq(t, `[{"id":5,"title":"s1"}]`, body)
	require.Equal(t, "application/vnd.pgrst.array+json;nulls=stripped; charset=utf-8", headers.Get("Content-Type"))

	status, headers, body = send(t, f.rest, http.MethodPost, path+"?select=id,title,meta", f.svc,
		map[string]string{"Accept": "application/vnd.pgrst.object+json;nulls=stripped", "Prefer": "return=representation"}, `{"title":"s2"}`)
	require.Equal(t, http.StatusCreated, status, body)
	require.JSONEq(t, `{"id":6,"title":"s2"}`, body)
	require.Equal(t, "application/vnd.pgrst.object+json;nulls=stripped; charset=utf-8", headers.Get("Content-Type"))
}

func TestPostgRESTMedia_whatRestCannotProduceIs406PGRST107(t *testing.T) {
	f := newNotesFixture(t)
	stageCountries(t, f.db)
	path := "/rest/v1/" + notes

	for accept, listed := range map[string]string{
		// .explain() in the forms supabase-js 2.118 sends, with options.
		"application/vnd.pgrst.plan+json; for=\"application/json\"":                                               "application/vnd.pgrst.plan+json; for=\"application/json\"",
		"application/vnd.pgrst.plan+text; for=\"application/json\"":                                               "application/vnd.pgrst.plan+text; for=\"application/json\"",
		"application/vnd.pgrst.plan+json; for=\"application/json\"; options=analyze|verbose|settings|buffers|wal": "application/vnd.pgrst.plan+json; for=\"application/json\"; options=analyze|verbose|settings|buffers|wal",
		"application/vnd.pgrst.plan": "application/vnd.pgrst.plan+text; for=\"application/json\"",
		// Types the server has no way to produce on a table.
		"application/x-bogus":      "application/x-bogus",
		"text/plain":               "text/plain",
		"application/octet-stream": "application/octet-stream",
		"text/xml":                 "text/xml",
	} {
		status, headers, body := send(t, f.rest, http.MethodGet, "/rest/v1/"+countries+"?select=id", f.anon, accepting(accept), "")
		require.Equal(t, http.StatusNotAcceptable, status, "%s: %s", accept, body)
		require.JSONEq(t, `{"code":"PGRST107","details":null,"hint":null,"message":"None of these media types are available: `+
			jsonEscape(listed)+`"}`, body, accept)
		require.Equal(t, "application/json; charset=utf-8", headers.Get("Content-Type"))

		// A write is refused before it runs: nothing changed.
		before := theNotes(t, f.db)
		status, _, body = send(t, f.rest, http.MethodPost, path+"?select=id", f.svc,
			map[string]string{"Accept": accept, "Prefer": "return=representation"}, `{"title":"never"}`)
		require.Equal(t, http.StatusNotAcceptable, status, "%s: %s", accept, body)
		require.Equal(t, "PGRST107", refusalOf(t, body)["code"], accept)
		require.Equal(t, before, theNotes(t, f.db), accept)

		// And a call.
		status, _, body = send(t, f.rest, http.MethodPost, "/rest/v1/rpc/rest_pgc_hello", f.anon, accepting(accept), "")
		require.Equal(t, http.StatusNotAcceptable, status, "%s: %s", accept, body)
		require.Equal(t, "PGRST107", refusalOf(t, body)["code"], accept)
	}

	// The rest of the list: the entries are listed in quality order, and one
	// that can be produced wins over any that cannot.
	status, _, body := send(t, f.rest, http.MethodGet, "/rest/v1/"+countries+"?select=id&id=eq.1", f.anon, accepting("text/plain;q=0.9, application/vnd.pgrst.plan+json, application/json;q=0.1"), "")
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `[{"id":1}]`, body)
}

func jsonEscape(s string) string {
	out := ""
	for _, r := range s {
		if r == '"' || r == '\\' {
			out += "\\"
		}
		out += string(r)
	}
	return out
}

func TestPostgRESTMedia_jsonIsUnchanged(t *testing.T) {
	f := newCallFixture(t)
	stageCountries(t, f.db)
	for _, accept := range []string{"", "*/*", "application/json", "application/json, text/plain, */*", "application/vnd.pgrst.array+json"} {
		headers := map[string]string{}
		if accept != "" {
			headers["Accept"] = accept
		}
		status, h, body := send(t, f.rest, http.MethodGet, "/rest/v1/"+countries+"?select=id,name&order=id&limit=2", f.anon, headers, "")
		require.Equal(t, http.StatusOK, status, "%q: %s", accept, body)
		require.JSONEq(t, `[{"id":1,"name":"Thailand"},{"id":2,"name":"Japan"}]`, body, accept)
		require.Equal(t, "application/json; charset=utf-8", h.Get("Content-Type"), accept)
	}

	// .single()
	status, h, body := send(t, f.rest, http.MethodGet, "/rest/v1/"+countries+"?select=id,note&id=eq.2", f.anon, accepting("application/vnd.pgrst.object+json"), "")
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `{"id":2,"note":null}`, body)
	require.Equal(t, "application/vnd.pgrst.object+json; charset=utf-8", h.Get("Content-Type"))
}

// The tests below need PostGIS: the first where it is not installed, the
// second where it is. The compose Postgres of CI has none, so the second skips
// there and runs against any image that carries it (Supabase's, PostGIS's).

func postgisAvailable(t *testing.T, db *sql.DB) (installed, available bool) {
	t.Helper()
	require.NoError(t, db.QueryRow(`SELECT to_regprocedure('st_asgeojson(record)') IS NOT NULL`).Scan(&installed))
	require.NoError(t, db.QueryRow(`SELECT EXISTS (SELECT FROM pg_available_extensions WHERE name = 'postgis')`).Scan(&available))
	return installed, available
}

// refusedWithoutPostGIS holds an answer to the Database's own refusal, which
// PostgREST passes on: ST_AsGeoJSON is not a function, 42883, a 404.
func refusedWithoutPostGIS(t *testing.T, status int, body string) {
	t.Helper()
	require.Equal(t, http.StatusNotFound, status, body)
	refusal := refusalOf(t, body)
	require.Equal(t, "42883", refusal["code"], body)
	require.Contains(t, refusal["message"], "st_asgeojson", body)
	require.Contains(t, refusal, "details")
	require.Contains(t, refusal, "hint")
}

func TestPostgRESTMedia_geojsonWithoutPostGISIsTheDatabasesRefusal(t *testing.T) {
	f := newCallFixture(t)
	stageCountries(t, f.db)
	if installed, _ := postgisAvailable(t, f.db); installed {
		t.Skip("PostGIS is installed in this Database")
	}
	geo := accepting("application/geo+json")

	status, headers, body := send(t, f.rest, http.MethodGet, "/rest/v1/"+countries+"?select=id", f.anon, geo, "")
	refusedWithoutPostGIS(t, status, body)
	require.Equal(t, "application/json; charset=utf-8", headers.Get("Content-Type"))

	status, _, body = send(t, f.rest, http.MethodPost, "/rest/v1/rpc/rest_pgc_titles", f.a, geo, `{"min_id":1}`)
	refusedWithoutPostGIS(t, status, body)
}

func TestPostgRESTMedia_geojsonWithoutPostGISChangesNothingOnAWrite(t *testing.T) {
	f := newNotesFixture(t)
	if installed, _ := postgisAvailable(t, f.db); installed {
		t.Skip("PostGIS is installed in this Database")
	}
	before := theNotes(t, f.db)
	status, _, body := send(t, f.rest, http.MethodPost, "/rest/v1/"+notes+"?select=id", f.svc,
		map[string]string{"Accept": "application/geo+json", "Prefer": "return=representation"}, `{"title":"never"}`)
	refusedWithoutPostGIS(t, status, body)
	require.Equal(t, before, theNotes(t, f.db))
}

func TestPostgRESTMedia_geojsonWithPostGISIsAFeatureCollection(t *testing.T) {
	f := newCallFixture(t)
	if installed, available := postgisAvailable(t, f.db); !installed {
		if !available {
			t.Skip("PostGIS is not available in this Database")
		}
		exec(t, f.db, `CREATE EXTENSION IF NOT EXISTS postgis`)
		t.Cleanup(func() { exec(t, f.db, `DROP EXTENSION IF EXISTS postgis CASCADE`) })
	}
	shops := "rest_pgm_shops"
	exec(t, f.db, fmt.Sprintf(`
		DROP TABLE IF EXISTS public.%[1]s;
		CREATE TABLE public.%[1]s (id int PRIMARY KEY, address text, shop_geom geometry(Point, 4326), note text);
		INSERT INTO public.%[1]s VALUES
			(1, '1369 Cambridge St', ST_SetSRID(ST_MakePoint(-71.10044, 42.373695), 4326), NULL),
			(2, '757 Massachusetts Ave', ST_SetSRID(ST_MakePoint(-71.10543, 42.366432), 4326), 'x');
		GRANT SELECT ON public.%[1]s TO %[2]s;
		CREATE TABLE IF NOT EXISTS public.%[1]s_plain (id int PRIMARY KEY);
		GRANT SELECT ON public.%[1]s_plain TO %[2]s;
	`, shops, reader))
	t.Cleanup(func() {
		exec(t, f.db, fmt.Sprintf(`DROP TABLE IF EXISTS public.%[1]s, public.%[1]s_plain;`, shops))
	})
	geo := accepting("application/geo+json")

	// .geojson(): a FeatureCollection, the other columns its properties.
	status, headers, body := send(t, f.rest, http.MethodGet, "/rest/v1/"+shops+"?select=id,address,shop_geom&order=id", f.anon, geo, "")
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `{"type":"FeatureCollection","features":[
		{"type":"Feature","geometry":{"type":"Point","coordinates":[-71.10044,42.373695]},"properties":{"id":1,"address":"1369 Cambridge St"}},
		{"type":"Feature","geometry":{"type":"Point","coordinates":[-71.10543,42.366432]},"properties":{"id":2,"address":"757 Massachusetts Ave"}}]}`, body)
	require.Equal(t, "application/geo+json; charset=utf-8", headers.Get("Content-Type"))
	require.Equal(t, "0-1/*", headers.Get("Content-Range"))

	// No row: an empty feature list.
	_, _, body = send(t, f.rest, http.MethodGet, "/rest/v1/"+shops+"?id=gt.9", f.anon, geo, "")
	require.JSONEq(t, `{"type":"FeatureCollection","features":[]}`, body)

	// A table with no geometry column is the Database's own refusal, 22023,
	// as PostgREST passes it on.
	status, _, body = send(t, f.rest, http.MethodGet, "/rest/v1/"+shops+"_plain", f.anon, geo, "")
	require.Equal(t, http.StatusBadRequest, status, body)
	require.JSONEq(t, `{"code":"22023","details":null,"hint":null,"message":"geometry column is missing"}`, body)
}
