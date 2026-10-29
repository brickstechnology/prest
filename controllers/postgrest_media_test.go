package controllers

// miniship (miniship-cloud#1886, ADR 0074): the media type negotiation, which
// needs no Database. Every expected word is PostgREST v14.0's: MediaType.hs,
// Plan.hs (negotiateContent) and Error.hs (MediaTypeError).

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPgrNegotiate_whatSupabaseJsSendsIsProducedAsItIs(t *testing.T) {
	for _, c := range []struct {
		accept      string
		media       pgrMedia
		contentType string
	}{
		// No Accept, */*, and the json types: the rows as JSON, as before.
		{"", pgrMedia{kind: pgrMediaJSON}, "application/json; charset=utf-8"},
		{"*/*", pgrMedia{kind: pgrMediaJSON}, "application/json; charset=utf-8"},
		{"application/json", pgrMedia{kind: pgrMediaJSON}, "application/json; charset=utf-8"},
		{"application/json, text/plain, */*", pgrMedia{kind: pgrMediaJSON}, "application/json; charset=utf-8"},
		{"application/vnd.pgrst.array+json", pgrMedia{kind: pgrMediaJSON}, "application/json; charset=utf-8"},
		// .single()
		{"application/vnd.pgrst.object+json", pgrMedia{kind: pgrMediaObject}, "application/vnd.pgrst.object+json; charset=utf-8"},
		// .stripNulls(), the array and the object form.
		{"application/vnd.pgrst.array+json;nulls=stripped", pgrMedia{kind: pgrMediaJSON, strip: true}, "application/vnd.pgrst.array+json;nulls=stripped; charset=utf-8"},
		{"application/vnd.pgrst.array;nulls=stripped", pgrMedia{kind: pgrMediaJSON, strip: true}, "application/vnd.pgrst.array+json;nulls=stripped; charset=utf-8"},
		{"application/vnd.pgrst.object+json;nulls=stripped", pgrMedia{kind: pgrMediaObject, strip: true}, "application/vnd.pgrst.object+json;nulls=stripped; charset=utf-8"},
		{"Application/Vnd.PgRsT.Object+Json", pgrMedia{kind: pgrMediaObject}, "application/vnd.pgrst.object+json; charset=utf-8"},
		// .csv() and .geojson()
		{"text/csv", pgrMedia{kind: pgrMediaCSV}, "text/csv; charset=utf-8"},
		{"text/csv; charset=utf-8", pgrMedia{kind: pgrMediaCSV}, "text/csv; charset=utf-8"},
		{"application/geo+json", pgrMedia{kind: pgrMediaGeoJSON}, "application/geo+json; charset=utf-8"},
		// The first entry in quality order that can be produced.
		{"text/plain, text/csv", pgrMedia{kind: pgrMediaCSV}, "text/csv; charset=utf-8"},
		{"application/json;q=0.5, text/csv", pgrMedia{kind: pgrMediaCSV}, "text/csv; charset=utf-8"},
		{"application/vnd.pgrst.plan+json, application/json", pgrMedia{kind: pgrMediaJSON}, "application/json; charset=utf-8"},
	} {
		media, err := pgrNegotiate(c.accept)
		require.NoError(t, err, c.accept)
		require.Equal(t, c.media, media, c.accept)
		require.Equal(t, c.contentType, media.contentType(), c.accept)
	}
}

func TestPgrNegotiate_whatRestCannotProduceIs406WithPostgRESTsWords(t *testing.T) {
	for accept, listed := range map[string]string{
		"application/x-bogus":       "application/x-bogus",
		"text/plain":                "text/plain",
		"text/xml":                  "text/xml",
		"application/octet-stream":  "application/octet-stream",
		"text/plain;charset=utf-8":  "text/plain",
		"application/openapi+json":  "application/openapi+json",
		"image/png, text/plain":     "image/png, text/plain",
		"image/png;q=0.1, text/xml": "text/xml, image/png",
		// .explain(): the plan types, in each of the forms supabase-js sends
		// and the parameters PostgREST reads, written back as toMime writes
		// them.
		"application/vnd.pgrst.plan+json; for=\"application/json\"":                          "application/vnd.pgrst.plan+json; for=\"application/json\"",
		"application/vnd.pgrst.plan+text; for=\"application/json\"; options=analyze|verbose": "application/vnd.pgrst.plan+text; for=\"application/json\"; options=analyze|verbose",
		"application/vnd.pgrst.plan": "application/vnd.pgrst.plan+text; for=\"application/json\"",
		"application/vnd.pgrst.plan+json;for=\"text/csv\";options=wal|buffers|settings":             "application/vnd.pgrst.plan+json; for=\"text/csv\"; options=settings|buffers|wal",
		"application/vnd.pgrst.plan+json; for=\"application/vnd.pgrst.object+json;nulls=stripped\"": "application/vnd.pgrst.plan+json; for=\"application/vnd.pgrst.object+json;nulls=stripped\"",
	} {
		_, err := pgrNegotiate(accept)
		refused, ok := err.(*pgrError)
		require.True(t, ok, accept)
		require.Equal(t, 406, refused.status, accept)
		require.Equal(t, "PGRST107", refused.code, accept)
		require.Equal(t, "None of these media types are available: "+listed, refused.message, accept)
		require.Empty(t, refused.details, accept)
		require.Empty(t, refused.hint, accept)
	}
}

func TestPgrNegotiate_theEntriesAreListedInQualityOrder(t *testing.T) {
	_, err := pgrNegotiate("text/plain;q=0.2, application/x-bogus, application/octet-stream;q=0.9")
	require.EqualError(t, err, "None of these media types are available: application/x-bogus, application/octet-stream, text/plain")
}

func TestPgrMedia_aggregateIsParameterlessSQLForEachBody(t *testing.T) {
	source := `SELECT "t"."name" FROM "public"."countries" AS "t" LIMIT 2 OFFSET 0`

	// Plain JSON is the statement rest has always run.
	require.Equal(t, `SELECT coalesce(json_agg(x), '[]'::json)::text FROM (`+source+`) AS x`,
		pgrMedia{}.aggregate(source, "x", false))
	require.Equal(t, `SELECT coalesce(json_agg("x"."pgrst_scalar"), '[]'::json)::text FROM (`+source+`) AS "x"`,
		pgrMedia{}.aggregate(source, `"x"`, true))

	// nulls=stripped strips the aggregate, as addNullsToSnip does.
	require.Equal(t, `SELECT coalesce(json_strip_nulls(json_agg(x)), '[]'::json)::text FROM (`+source+`) AS x`,
		pgrMedia{kind: pgrMediaObject, strip: true}.aggregate(source, "x", false))

	// asCsvF: the header from the first row, the body from the records.
	csv := pgrMedia{kind: pgrMediaCSV}.aggregate(source, "x", false)
	require.Contains(t, csv, `WITH "pgrst_s" AS (`+source+`)`)
	require.Contains(t, csv, `string_agg(a.k, ',')`)
	require.Contains(t, csv, `json_object_keys(r)::text AS k`)
	require.Contains(t, csv, `SELECT row_to_json(hh) AS r FROM "pgrst_s" AS hh LIMIT 1`)
	require.Contains(t, csv, `substring("pgrst_t"::text, 2, length("pgrst_t"::text) - 2), E'\n'`)
	require.Contains(t, csv, ` || E'\n' || `)
	require.NotContains(t, csv, "$")

	// asGeoJsonF
	geo := pgrMedia{kind: pgrMediaGeoJSON}.aggregate(source, "x", false)
	require.Equal(t, `SELECT json_build_object('type', 'FeatureCollection', 'features', coalesce(json_agg(ST_AsGeoJSON("pgrst_t")::json), '[]'))::text FROM (`+source+`) AS "pgrst_t"`, geo)

	// The row count rides along for the bodies that cannot be read back.
	require.True(t, strings.HasSuffix(pgrMedia{kind: pgrMediaCSV}.counted(source), `, count(*) FROM "pgrst_s" AS "pgrst_t"`) ||
		strings.Contains(pgrMedia{kind: pgrMediaCSV}.counted(source), `, count(*) FROM "pgrst_s"`))
}

func TestPgrCallPlan_aCsvCallReadsTheRowsAsCsvAndCountsThePage(t *testing.T) {
	read, err := pgrParseRead(url.Values{"order": {"id"}, "limit": {"2"}}, 5000)
	require.NoError(t, err)
	table := &pgrRoutine{name: "titles", result: pgrResultSetOfComposite}
	statement, _, err := pgrPlanCall(table, read, keys(), false, "").statementAs(false, pgrMedia{kind: pgrMediaCSV})
	require.NoError(t, err)
	require.Contains(t, statement, `WITH "pgrst_s" AS (SELECT "t".* FROM "pgrst_source" AS "t"`)
	require.Contains(t, statement, `(SELECT count(*) FROM (SELECT "t".* FROM "pgrst_source" AS "t" ORDER BY "t"."id" ASC LIMIT 2 OFFSET 0) AS "n")`)
}
