package anonymousrole_test

// miniship (miniship-cloud#1888, ADR 0075): what the SDK sees when /rest/v1
// refuses, and what it is told about its preferences, against a live Postgres.
// The expected strings, codes, statuses and headers are those of PostgREST
// v14.18 (Error.hs, Preferences.hs, Response.hs, Plan.hs), read from its source.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

const (
	pgrJSON  = "application/json; charset=utf-8"
	boardKey = "rest_pge_board"
)

// stageBoard is a table whose primary key is two columns, the second text that
// needs percent-encoding in a Location, and one with no primary key.
func stageBoard(t *testing.T, f notesFixture) {
	t.Helper()
	drop := fmt.Sprintf(`DROP TABLE IF EXISTS public.%[1]s, public.%[1]s_loose;`, boardKey)
	exec(t, f.db, drop)
	t.Cleanup(func() { exec(t, f.db, drop) })
	exec(t, f.db, fmt.Sprintf(`
		CREATE TABLE public.%[1]s (lane int, label text, note text, PRIMARY KEY (label, lane));
		CREATE TABLE public.%[1]s_loose (a int, b text);
		GRANT SELECT, INSERT, UPDATE, DELETE ON public.%[1]s, public.%[1]s_loose TO %[2]s, %[3]s;
	`, boardKey, signedIn, service))
}

func signedWith(t *testing.T, alg jose.SignatureAlgorithm, key any, kid string, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid))
	require.NoError(t, err)
	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	require.NoError(t, err)
	return token
}

func refusalWith(t *testing.T, status int, headers http.Header, body string) map[string]any {
	t.Helper()
	got := refusalOf(t, body)
	require.Equal(t, pgrJSON, headers.Get("Content-Type"), "status %d: %s", status, body)
	require.Equal(t, "PostgREST; error="+fmt.Sprint(got["code"]), headers.Get("Proxy-Status"), body)
	return got
}

func TestPostgRESTErrors_aMissingTableIsPGRST205WithTheHintPostgRESTComputes(t *testing.T) {
	f := newNotesFixture(t)

	// A name one letter short of a table that is there: PGRST205 and the hint.
	for _, call := range []struct{ method, path, body string }{
		{http.MethodGet, "/rest/v1/" + notes + "x", ""},
		{http.MethodHead, "/rest/v1/" + notes + "x", ""},
		{http.MethodPost, "/rest/v1/" + notes + "x", `{"title":"t"}`},
		{http.MethodPatch, "/rest/v1/" + notes + "x?id=eq.1", `{"title":"t"}`},
		{http.MethodDelete, "/rest/v1/" + notes + "x?id=eq.1", ""},
	} {
		status, headers, body := send(t, f.rest, call.method, call.path, f.svc, nil, call.body)
		require.Equal(t, http.StatusNotFound, status, call.method+": "+body)
		if call.method == http.MethodHead {
			require.Empty(t, body)
			continue
		}
		got := refusalWith(t, status, headers, body)
		require.Equal(t, "PGRST205", got["code"], call.method)
		require.Equal(t, "Could not find the table 'public."+notes+"x' in the schema cache", got["message"])
		require.Nil(t, got["details"])
		require.Equal(t, "Perhaps you meant the table 'public."+notes+"'", got["hint"], call.method)
	}

	// A name with nothing close in the schema: no hint, and still PGRST205.
	status, headers, body := send(t, f.rest, http.MethodGet, "/rest/v1/qqqzzz_no_such_thing", f.anon, nil, "")
	require.Equal(t, http.StatusNotFound, status, body)
	got := refusalWith(t, status, headers, body)
	require.Equal(t, "PGRST205", got["code"])
	require.Equal(t, "Could not find the table 'public.qqqzzz_no_such_thing' in the schema cache", got["message"])
	require.Nil(t, got["details"])
	require.Nil(t, got["hint"])

	// A select that embeds, on a table that is not there, is still the table.
	status, _, body = send(t, f.rest, http.MethodGet, "/rest/v1/qqqzzz_no_such_thing?select=id,other(id)", f.anon, nil, "")
	require.Equal(t, http.StatusNotFound, status, body)
	require.Equal(t, "PGRST205", refusalOf(t, body)["code"])

	// A table that is there and that the role may not read is the Database's
	// refusal, not a missing table: 42501, and 401 for the anonymous role.
	exec(t, f.db, fmt.Sprintf(`REVOKE ALL ON public.%s FROM %s`, notes, reader))
	status, headers, body = send(t, f.rest, http.MethodGet, "/rest/v1/"+notes, f.anon, nil, "")
	require.Equal(t, http.StatusUnauthorized, status, body)
	got = refusalWith(t, status, headers, body)
	require.Equal(t, "42501", got["code"])
	require.Equal(t, "Bearer", headers.Get("WWW-Authenticate"))
	status, _, body = send(t, f.rest, http.MethodGet, "/rest/v1/"+notes+"?select=id", f.a, nil, "")
	require.Equal(t, http.StatusOK, status, "a signed-in User still reads: "+body)
}

func TestPostgRESTErrors_theTableIsFoundBeforeTheMediaTypeAndThePreferences(t *testing.T) {
	f := newNotesFixture(t)
	bogus := accepting("application/bogus")

	// PostgREST finds the table, plans the request, then negotiates the media
	// type (Plan.hs, wrappedReadPlan): a missing table is 404 with any Accept,
	// and a table that is there is 406.
	status, _, body := send(t, f.rest, http.MethodGet, "/rest/v1/qqqzzz_no_such_thing", f.anon, bogus, "")
	require.Equal(t, http.StatusNotFound, status, body)
	require.Equal(t, "PGRST205", refusalOf(t, body)["code"])
	status, _, body = send(t, f.rest, http.MethodGet, "/rest/v1/"+notes, f.svc, bogus, "")
	require.Equal(t, http.StatusNotAcceptable, status, body)
	require.Equal(t, "PGRST107", refusalOf(t, body)["code"])

	// The same for handling=strict, on a read and on a write.
	strict := map[string]string{"Prefer": "handling=strict, foo=bar"}
	status, _, body = send(t, f.rest, http.MethodGet, "/rest/v1/qqqzzz_no_such_thing", f.anon, strict, "")
	require.Equal(t, http.StatusNotFound, status, body)
	require.Equal(t, "PGRST205", refusalOf(t, body)["code"])
	status, _, body = send(t, f.rest, http.MethodPost, "/rest/v1/qqqzzz_no_such_thing", f.svc, strict, `{"a":1}`)
	require.Equal(t, http.StatusNotFound, status, body)
	require.Equal(t, "PGRST205", refusalOf(t, body)["code"])

	// A write plans its columns before it checks preferences and negotiates
	// (Plan.hs, mutateReadPlan): PGRST204 comes first.
	status, _, body = send(t, f.rest, http.MethodPost, "/rest/v1/"+notes, f.svc, strict, `{"no_such_column":1}`)
	require.Equal(t, http.StatusBadRequest, status, body)
	require.Equal(t, "PGRST204", refusalOf(t, body)["code"])
	// A write is refused before it runs: nothing changed.
	before := theNotes(t, f.db)
	status, _, body = send(t, f.rest, http.MethodPost, "/rest/v1/"+notes, f.svc, strict, `{"title":"never"}`)
	require.Equal(t, http.StatusBadRequest, status, body)
	require.Equal(t, "PGRST122", refusalOf(t, body)["code"])
	require.Equal(t, before, theNotes(t, f.db))
}

func TestPostgRESTErrors_aTokenThatFailsIsPostgRESTs401(t *testing.T) {
	cfg, db := needsPostgres(t)
	stageNotes(t, db)
	install := newInstall(t)
	pool := newProjectPool(t, alias)
	rest := restForTheSDK(t, cfg, install, pool)
	const path = "/rest/v1/" + notes + "?select=id"

	check := func(name, token, code, message string) {
		t.Helper()
		status, headers, body := send(t, rest, http.MethodGet, path, token, nil, "")
		require.Equal(t, http.StatusUnauthorized, status, name+": "+body)
		got := refusalWith(t, status, headers, body)
		require.Equal(t, code, got["code"], name)
		require.Equal(t, message, got["message"], name)
		require.Equal(t, `Bearer error="invalid_token", error_description="`+message+`"`, headers.Get("WWW-Authenticate"), name)
	}
	soon, past := time.Now().Add(time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()

	// Malformed: three parts that are not a JWT, and the wrong number of parts.
	check("abc.def.ghi", "abc.def.ghi", "PGRST301", "JWT couldn't be decoded")
	check("two parts", "abc.def", "PGRST301", "Expected 3 parts in JWT; got 2")
	check("a bare api key", "ms_secret_not_a_token", "PGRST301", "Expected 3 parts in JWT; got 1")

	// Wrong key: signed by a key the install does not hold, under its kid.
	foreign := newInstall(t)
	check("wrong key", projectToken(t, foreign.private, service, alias), "PGRST301", "JWT cryptographic operation failed")
	// A key id the install's set lacks.
	check("unknown kid", signedWith(t, jose.ES256, install.private, "another", map[string]any{
		"role": service, "project": alias, "exp": soon}), "PGRST301", "No suitable key or wrong key type")

	// Expired, and not yet valid, signed by the right key.
	check("expired", signedWith(t, jose.ES256, install.private, "install", map[string]any{
		"role": service, "project": alias, "exp": past}), "PGRST303", "JWT expired")
	check("not yet valid", signedWith(t, jose.ES256, install.private, "install", map[string]any{
		"role": service, "project": alias, "nbf": soon, "exp": soon + 3600}), "PGRST303", "JWT not yet valid")

	// A User's token against the Project's key set: wrong key, and expired.
	check("a User's token, wrong key", userToken(t, newProjectPool(t, alias).private, userA), "PGRST301", "JWT cryptographic operation failed")
	check("a User's token, expired", signedWith(t, jose.EdDSA, pool.private, "k_pool", map[string]any{
		"sub": userA, "aud": "authenticated", "role": "authenticated", "exp": past}), "PGRST303", "JWT expired")

	// Every refusal reached no row, and a good token still reads.
	status, _, body := send(t, rest, http.MethodGet, path, projectToken(t, install.private, service, alias), nil, "")
	require.Equal(t, http.StatusOK, status, body)
}

func TestPostgRESTErrors_theRefusalsRestWritesItselfKeepTheEnvelope(t *testing.T) {
	f := newNotesFixture(t)

	for name, tc := range map[string]struct {
		method, path, body string
		headers            map[string]string
		status             int
		code               string
	}{
		"a filter that does not parse": {http.MethodGet, "/rest/v1/" + notes + "?id=wat.1", "", nil, 400, "PGRST100"},
		"a limit over the ceiling":     {http.MethodGet, "/rest/v1/" + notes + "?limit=100000000", "", nil, 400, "MS100"},
		"a schema that is not served":  {http.MethodGet, "/rest/v1/" + notes, "", map[string]string{"Accept-Profile": "private"}, 406, "PGRST106"},
		"a body that is not JSON":      {http.MethodPost, "/rest/v1/" + notes, `nope`, nil, 400, "PGRST102"},
		"an unknown function":          {http.MethodPost, "/rest/v1/rpc/no_such_function", `{}`, nil, 404, "PGRST202"},
		"a constraint":                 {http.MethodPost, "/rest/v1/" + notes, `{"title":"a1","owner":"` + userA + `"}`, nil, 409, "23505"},
		"an update with no filter":     {http.MethodPatch, "/rest/v1/" + notes, `{"body":"x"}`, nil, 400, "21000"},
	} {
		status, headers, body := send(t, f.rest, tc.method, tc.path, f.a, tc.headers, tc.body)
		require.Equal(t, tc.status, status, name+": "+body)
		got := refusalWith(t, status, headers, body)
		require.Equal(t, tc.code, got["code"], name)
	}

	// A call to a project that has none, before any Database is asked: the
	// envelope with rest's own code.
	req, err := http.NewRequest(http.MethodGet, f.rest.URL+"/rest/v1/"+notes, nil)
	require.NoError(t, err)
	resp, err := f.rest.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Equal(t, pgrJSON, resp.Header.Get("Content-Type"))
	require.Equal(t, "PostgREST; error=MS102", resp.Header.Get("Proxy-Status"))
}

func TestPostgRESTErrors_aDatabaseRefusalCarriesItsSQLSTATEAsTheProxyStatus(t *testing.T) {
	f := newNotesFixture(t)

	// An anonymous write: 42501 is 401, with the Bearer challenge.
	status, headers, body := send(t, f.rest, http.MethodPost, "/rest/v1/"+notes, f.anon, nil, `{"title":"anon"}`)
	require.Equal(t, http.StatusUnauthorized, status, body)
	require.Equal(t, "42501", refusalWith(t, status, headers, body)["code"])
	require.Equal(t, "Bearer", headers.Get("WWW-Authenticate"))

	// A signed-in User's: 403, and no challenge.
	status, headers, body = send(t, f.rest, http.MethodPost, "/rest/v1/"+notes, f.a, nil, `{"title":"x","owner":"`+userB+`"}`)
	require.Equal(t, http.StatusForbidden, status, body)
	require.Equal(t, "42501", refusalWith(t, status, headers, body)["code"])
	require.Empty(t, headers.Get("WWW-Authenticate"))

	// A unique violation: 409.
	status, headers, body = send(t, f.rest, http.MethodPost, "/rest/v1/"+notes, f.svc, nil, `{"title":"a1"}`)
	require.Equal(t, http.StatusConflict, status, body)
	require.Equal(t, "PostgREST; error=23505", headers.Get("Proxy-Status"))
}

func TestPostgRESTPrefer_handlingStrictRefusesWhatLenientIgnores(t *testing.T) {
	f := newNotesFixture(t)

	bad := "handling=strict, foo=bar"
	want := func(status int, headers http.Header, body string) {
		t.Helper()
		require.Equal(t, http.StatusBadRequest, status, body)
		got := refusalWith(t, status, headers, body)
		require.Equal(t, "PGRST122", got["code"])
		require.Equal(t, "Invalid preferences given with handling=strict", got["message"])
		require.Equal(t, "Invalid preferences: foo=bar", got["details"])
		require.Nil(t, got["hint"])
	}

	// A read, an insert, an update, a delete and a call, each refused before it runs.
	before := theNotes(t, f.db)
	status, headers, body := send(t, f.rest, http.MethodGet, "/rest/v1/"+notes, f.a, map[string]string{"Prefer": bad}, "")
	want(status, headers, body)
	status, headers, body = send(t, f.rest, http.MethodHead, "/rest/v1/"+notes, f.a, map[string]string{"Prefer": bad}, "")
	require.Equal(t, http.StatusBadRequest, status)
	status, headers, body = send(t, f.rest, http.MethodPost, "/rest/v1/"+notes, f.a, map[string]string{"Prefer": bad}, `{"title":"s","owner":"`+userA+`"}`)
	want(status, headers, body)
	status, headers, body = send(t, f.rest, http.MethodPatch, "/rest/v1/"+notes+"?id=eq.1", f.a, map[string]string{"Prefer": bad}, `{"body":"s"}`)
	want(status, headers, body)
	status, headers, body = send(t, f.rest, http.MethodDelete, "/rest/v1/"+notes+"?id=eq.1", f.a, map[string]string{"Prefer": bad}, "")
	want(status, headers, body)
	require.Equal(t, before, theNotes(t, f.db), "a refused write changes nothing")

	// More than one invalid preference lists them in order, and an empty part
	// is one.
	status, _, body = send(t, f.rest, http.MethodGet, "/rest/v1/"+notes, f.a, map[string]string{"Prefer": "handling=strict, a=b, return=sideways,"}, "")
	require.Equal(t, http.StatusBadRequest, status, body)
	require.Equal(t, "Invalid preferences: a=b, return=sideways, ", refusalOf(t, body)["details"])

	// Lenient, the default, and valid preferences under strict: answered, and
	// the unrecognised one is not echoed.
	for _, prefer := range []string{"foo=bar", "handling=lenient, foo=bar", "handling=strict, return=minimal", "handling=strict, timezone=Asia/Bangkok"} {
		status, headers, body = send(t, f.rest, http.MethodGet, "/rest/v1/"+notes+"?select=id&order=id&limit=1", f.a, map[string]string{"Prefer": prefer}, "")
		require.Equal(t, http.StatusOK, status, prefer+": "+body)
		require.JSONEq(t, `[{"id":1}]`, body, prefer)
		require.NotContains(t, headers.Get("Preference-Applied"), "foo", prefer)
	}
}

func TestPostgRESTPrefer_preferenceAppliedEchoesWhatPostgREST14Echoes(t *testing.T) {
	f := newNotesFixture(t)
	path := "/rest/v1/" + notes
	applied := func(headers http.Header) string { return headers.Get("Preference-Applied") }
	prefer := func(value string) map[string]string { return map[string]string{"Prefer": value} }

	// A read echoes count, tx and handling, and nothing else it was given.
	_, headers, body := send(t, f.rest, http.MethodGet, path+"?select=id", f.a,
		prefer("return=representation, count=exact, handling=lenient, max-affected=3, missing=default, resolution=merge-duplicates"), "")
	require.Equal(t, "count=exact, handling=lenient", applied(headers), body)
	_, headers, _ = send(t, f.rest, http.MethodGet, path+"?select=id", f.a, prefer("count=planned"), "")
	require.Equal(t, "count=planned", applied(headers))
	_, headers, _ = send(t, f.rest, http.MethodGet, path+"?select=id", f.a, prefer("count=estimated, tx=commit"), "")
	require.Equal(t, "count=estimated, tx=commit", applied(headers))
	_, headers, _ = send(t, f.rest, http.MethodGet, path+"?select=id", f.a, nil, "")
	require.Empty(t, headers.Values("Preference-Applied"), "no preference, no header")
	_, headers, _ = send(t, f.rest, http.MethodGet, path+"?select=id", f.a, prefer("foo=bar"), "")
	require.Empty(t, headers.Values("Preference-Applied"), "an unrecognised preference is not echoed")

	// The first of a preference given twice is the one that is applied.
	_, headers, _ = send(t, f.rest, http.MethodGet, path+"?select=id", f.a, prefer("count=exact, count=planned"), "")
	require.Equal(t, "count=exact", applied(headers))

	// An insert: return, count and missing; a resolution with the primary key;
	// not max-affected.
	status, headers, body := send(t, f.rest, http.MethodPost, path, f.a,
		prefer("return=minimal, count=exact, missing=default, max-affected=1, handling=strict"), fmt.Sprintf(`{"title":"p1","owner":"%s"}`, userA))
	require.Equal(t, http.StatusCreated, status, body)
	require.Equal(t, "missing=default, return=minimal, count=exact, handling=strict", applied(headers))
	status, headers, body = send(t, f.rest, http.MethodPost, path+"?select=title", f.a,
		prefer("resolution=merge-duplicates, return=representation, tx=rollback"), fmt.Sprintf(`{"id":1,"title":"a1","owner":"%s"}`, userA))
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "resolution=merge-duplicates, return=representation, tx=rollback", applied(headers))
	status, headers, body = send(t, f.rest, http.MethodPost, path+"?on_conflict=title", f.a,
		prefer("resolution=ignore-duplicates"), fmt.Sprintf(`{"title":"a1","owner":"%s"}`, userA))
	require.Equal(t, http.StatusCreated, status, body)
	require.Equal(t, "resolution=ignore-duplicates", applied(headers))

	// An update: return, count, missing and max-affected under strict.
	status, headers, body = send(t, f.rest, http.MethodPatch, path+"?id=eq.2", f.a,
		prefer("return=representation, count=exact, missing=null, resolution=merge-duplicates, handling=strict, max-affected=5"), `{"body":"u"}`)
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "missing=null, return=representation, count=exact, handling=strict, max-affected=5", applied(headers))
	// A delete: not missing, not resolution.
	status, headers, body = send(t, f.rest, http.MethodDelete, path+"?id=eq.2", f.a,
		prefer("return=minimal, missing=default, resolution=ignore-duplicates, handling=strict, max-affected=5, tx=commit"), "")
	require.Equal(t, http.StatusNoContent, status, body)
	require.Equal(t, "return=minimal, tx=commit, handling=strict, max-affected=5", applied(headers))
	// max-affected without handling=strict is neither enforced nor echoed.
	status, headers, body = send(t, f.rest, http.MethodPatch, path+"?id=in.(1,3)&select=id", f.svc,
		prefer("return=representation, max-affected=1"), `{"body":"two"}`)
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "return=representation", applied(headers))
}

func TestPostgRESTPrefer_maxAffectedBindsUnderStrictWithPGRST124(t *testing.T) {
	f := newNotesFixture(t)
	path := "/rest/v1/" + notes
	before := theNotes(t, f.db)

	status, headers, body := send(t, f.rest, http.MethodPatch, path+"?id=in.(1,2,3)", f.svc,
		map[string]string{"Prefer": "handling=strict, max-affected=2"}, `{"body":"x"}`)
	require.Equal(t, http.StatusBadRequest, status, body)
	got := refusalWith(t, status, headers, body)
	require.Equal(t, "PGRST124", got["code"])
	require.Equal(t, "Query result exceeds max-affected preference constraint", got["message"])
	require.Equal(t, "The query affects 3 rows", got["details"])
	status, _, body = send(t, f.rest, http.MethodDelete, path+"?id=in.(1,2,3)", f.svc,
		map[string]string{"Prefer": "handling=strict, max-affected=2"}, "")
	require.Equal(t, http.StatusBadRequest, status, body)
	require.Equal(t, before, theNotes(t, f.db), "the writes were rolled back")

	// At the limit it goes through, and an insert is never bound by it.
	status, _, body = send(t, f.rest, http.MethodPatch, path+"?id=in.(1,2)", f.svc,
		map[string]string{"Prefer": "handling=strict, max-affected=2"}, `{"body":"y"}`)
	require.Equal(t, http.StatusNoContent, status, body)
	status, _, body = send(t, f.rest, http.MethodPost, path, f.svc,
		map[string]string{"Prefer": "handling=strict, max-affected=0"}, `[{"title":"m1"},{"title":"m2"}]`)
	require.Equal(t, http.StatusCreated, status, body)
}

func TestPostgRESTPrefer_headersOnlyAnswersALocation(t *testing.T) {
	f := newNotesFixture(t)
	stageBoard(t, f)
	headersOnly := map[string]string{"Prefer": "return=headers-only"}

	// insert() with return=headers-only: 201, no body, and a Location naming
	// the row by its primary key (Response.hs, rsLocation).
	status, headers, body := send(t, f.rest, http.MethodPost, "/rest/v1/"+notes, f.svc, headersOnly, `{"title":"loc1"}`)
	require.Equal(t, http.StatusCreated, status, body)
	require.Empty(t, body)
	require.Equal(t, "/"+notes+"?id=eq.5", headers.Get("Location"))
	require.Equal(t, "return=headers-only", headers.Get("Preference-Applied"))
	require.Equal(t, "*/*", headers.Get("Content-Range"))

	// The row is there, and the Location finds it.
	status, _, body = send(t, f.rest, http.MethodGet, "/rest/v1/"+notes+"?id=eq.5&select=title", f.svc, nil, "")
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `[{"title":"loc1"}]`, body)

	// A composite key lists its columns in the table's order, and a value is
	// percent-encoded as renderSimpleQuery encodes it. (The table is
	// (lane, label, note); the key is (label, lane).)
	status, headers, body = send(t, f.rest, http.MethodPost, "/rest/v1/"+boardKey, f.svc, headersOnly, `{"lane":3,"label":"a b&c/é","note":"n"}`)
	require.Equal(t, http.StatusCreated, status, body)
	require.Equal(t, "/"+boardKey+"?lane=eq.3&label=eq.a%20b%26c%2F%C3%A9", headers.Get("Location"))

	// More than one row, or a table with no primary key: no Location.
	status, headers, body = send(t, f.rest, http.MethodPost, "/rest/v1/"+notes, f.svc, headersOnly, `[{"title":"loc2"},{"title":"loc3"}]`)
	require.Equal(t, http.StatusCreated, status, body)
	require.Empty(t, headers.Get("Location"))
	require.Equal(t, "return=headers-only", headers.Get("Preference-Applied"))
	status, headers, body = send(t, f.rest, http.MethodPost, "/rest/v1/"+boardKey+"_loose", f.svc, headersOnly, `{"a":1,"b":"x"}`)
	require.Equal(t, http.StatusCreated, status, body)
	require.Empty(t, headers.Get("Location"))

	// An upsert that inserts names its row; one that ignores a duplicate wrote
	// no row, so it names none.
	status, headers, body = send(t, f.rest, http.MethodPost, "/rest/v1/"+notes+"?on_conflict=title", f.svc,
		map[string]string{"Prefer": "return=headers-only, resolution=ignore-duplicates"}, `{"title":"loc4"}`)
	require.Equal(t, http.StatusCreated, status, body)
	require.Regexp(t, `^/`+notes+`\?id=eq\.\d+$`, headers.Get("Location"))
	require.Equal(t, "resolution=ignore-duplicates, return=headers-only", headers.Get("Preference-Applied"))
	status, headers, _ = send(t, f.rest, http.MethodPost, "/rest/v1/"+notes+"?on_conflict=title", f.svc,
		map[string]string{"Prefer": "return=headers-only, resolution=ignore-duplicates"}, `{"title":"loc4"}`)
	require.Equal(t, http.StatusCreated, status)
	require.Empty(t, headers.Get("Location"))

	// An update or a delete with headers-only: 204, and no Location.
	status, headers, body = send(t, f.rest, http.MethodPatch, "/rest/v1/"+notes+"?id=eq.5", f.svc, headersOnly, `{"body":"h"}`)
	require.Equal(t, http.StatusNoContent, status, body)
	require.Empty(t, headers.Get("Location"))
	require.Equal(t, "return=headers-only", headers.Get("Preference-Applied"))
	status, headers, _ = send(t, f.rest, http.MethodDelete, "/rest/v1/"+notes+"?id=eq.5", f.svc, headersOnly, "")
	require.Equal(t, http.StatusNoContent, status)
	require.Empty(t, headers.Get("Location"))

	// return=minimal and no preference name no Location, as before.
	status, headers, _ = send(t, f.rest, http.MethodPost, "/rest/v1/"+notes, f.svc, map[string]string{"Prefer": "return=minimal"}, `{"title":"loc5"}`)
	require.Equal(t, http.StatusCreated, status)
	require.Empty(t, headers.Get("Location"))
	require.Equal(t, "return=minimal", headers.Get("Preference-Applied"))
}

func TestPostgRESTPrefer_aCallEchoesCountTxHandlingAndMaxAffected(t *testing.T) {
	f := newCallFixture(t)

	status, headers, body := send(t, f.rest, http.MethodPost, "/rest/v1/rpc/rest_pgc_nums", f.a,
		map[string]string{"Prefer": "count=exact, return=representation, missing=default, handling=strict, max-affected=9, tx=commit"}, `{"n":3}`)
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "count=exact, tx=commit, handling=strict, max-affected=9", headers.Get("Preference-Applied"))

	// max-affected under strict binds a function that returns a set.
	status, headers, body = send(t, f.rest, http.MethodPost, "/rest/v1/rpc/rest_pgc_nums", f.a,
		map[string]string{"Prefer": "handling=strict, max-affected=2"}, `{"n":3}`)
	require.Equal(t, http.StatusBadRequest, status, body)
	require.Equal(t, "PGRST124", refusalWith(t, status, headers, body)["code"])

	// ... and is refused outright for one that returns a single value.
	status, headers, body = send(t, f.rest, http.MethodPost, "/rest/v1/rpc/rest_pgc_add", f.a,
		map[string]string{"Prefer": "handling=strict, max-affected=2"}, `{"a":1,"b":2}`)
	require.Equal(t, http.StatusBadRequest, status, body)
	got := refusalWith(t, status, headers, body)
	require.Equal(t, "PGRST128", got["code"])
	require.Equal(t, "Function must return SETOF or TABLE when max-affected preference is used with handling=strict", got["message"])

	// handling=strict with an invalid preference, and the function is not found first.
	status, headers, body = send(t, f.rest, http.MethodPost, "/rest/v1/rpc/rest_pgc_add", f.a,
		map[string]string{"Prefer": "handling=strict, foo=bar"}, `{"a":1,"b":2}`)
	require.Equal(t, http.StatusBadRequest, status, body)
	require.Equal(t, "PGRST122", refusalWith(t, status, headers, body)["code"])
	status, _, body = send(t, f.rest, http.MethodPost, "/rest/v1/rpc/no_such_function", f.a,
		map[string]string{"Prefer": "handling=strict, foo=bar"}, `{}`)
	require.Equal(t, http.StatusNotFound, status, body)
	require.Equal(t, "PGRST202", refusalOf(t, body)["code"])
}

func TestPostgRESTJSON_isSentWithTheCharsetPostgRESTAdds(t *testing.T) {
	f := newNotesFixture(t)

	for name, call := range map[string]struct{ method, path, body, prefer string }{
		"a read":                     {http.MethodGet, "/rest/v1/" + notes + "?select=id", "", ""},
		"an insert's rows":           {http.MethodPost, "/rest/v1/" + notes, `{"title":"j1"}`, "return=representation"},
		"an update's rows":           {http.MethodPatch, "/rest/v1/" + notes + "?id=eq.1", `{"body":"j"}`, "return=representation"},
		"a delete's rows":            {http.MethodDelete, "/rest/v1/" + notes + "?title=eq.j1", "", "return=representation"},
		"an application/json Accept": {http.MethodGet, "/rest/v1/" + notes + "?select=id", "", ""},
	} {
		headers := map[string]string{}
		if call.prefer != "" {
			headers["Prefer"] = call.prefer
		}
		if strings.Contains(name, "Accept") {
			headers["Accept"] = "application/json"
		}
		status, h, body := send(t, f.rest, call.method, call.path, f.svc, headers, call.body)
		require.Less(t, status, 300, name+": "+body)
		require.Equal(t, pgrJSON, h.Get("Content-Type"), name)
	}

	// A refusal is sent the same way.
	_, h, _ := send(t, f.rest, http.MethodGet, "/rest/v1/"+notes+"?id=wat.1", f.svc, nil, "")
	require.Equal(t, pgrJSON, h.Get("Content-Type"))
	// Another media type keeps its own, and the one-object type is not plain JSON.
	_, h, _ = send(t, f.rest, http.MethodGet, "/rest/v1/"+notes+"?id=eq.1&select=id", f.svc, accepting("application/vnd.pgrst.object+json"), "")
	require.Equal(t, "application/vnd.pgrst.object+json; charset=utf-8", h.Get("Content-Type"))
	_, h, _ = send(t, f.rest, http.MethodGet, "/rest/v1/"+notes+"?select=id", f.svc, accepting("text/csv"), "")
	require.Equal(t, "text/csv; charset=utf-8", h.Get("Content-Type"))
}

func TestPostgRESTErrors_singleOverTheWrongRowsIsPGRST116InPostgREST14sWords(t *testing.T) {
	f := newNotesFixture(t)
	single := accepting("application/vnd.pgrst.object+json")

	// single() over two rows, and over none: PGRST116, 406, PostgREST 14's message.
	for path, rows := range map[string]int{"/rest/v1/" + notes + "?select=id": 4, "/rest/v1/" + notes + "?id=eq.999&select=id": 0} {
		status, headers, body := send(t, f.rest, http.MethodGet, path, f.svc, single, "")
		require.Equal(t, http.StatusNotAcceptable, status, body)
		got := refusalWith(t, status, headers, body)
		require.Equal(t, "PGRST116", got["code"])
		require.Equal(t, "Cannot coerce the result to a single JSON object", got["message"])
		require.Equal(t, fmt.Sprintf("The result contains %d rows", rows), got["details"])
	}
}
