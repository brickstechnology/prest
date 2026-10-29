package controllers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPostgRESTPrefer_theFirstValueOfEachPreferenceWins(t *testing.T) {
	t.Parallel()
	// PostgREST: the first part that is one of a preference's values, across
	// every Prefer header (Preferences.hs, parsePrefs).
	got := pgrParsePrefer([]string{"return=foo, return=minimal, return=representation", "count=exact", "count=planned", "tx=rollback, tx=commit"})
	require.Equal(t, "minimal", got.ret)
	require.Equal(t, "exact", got.count)
	require.Equal(t, "rollback", got.tx)
	require.True(t, got.rollback())
	require.True(t, got.counted())
	require.False(t, got.representation())
}

func TestPostgRESTPrefer_whatPostgRESTCallsInvalid(t *testing.T) {
	t.Parallel()
	// A part that is no value of a preference, and is not timezone= or
	// max-affected=, is invalid. A value of another preference is invalid
	// under the wrong key.
	got := pgrParsePrefer([]string{"handling=strict, foo=bar,return=headers-only ,missing=maybe", "timezone=Asia/Bangkok, max-affected=abc, Return=minimal, params=single-object, "})
	require.Equal(t, []string{"foo=bar", "missing=maybe", "Return=minimal", "params=single-object", ""}, got.invalid)
	require.True(t, got.headersOnly())
	require.True(t, got.strict())
	require.False(t, got.maxAffectedSet, "a max-affected that is not a whole number is no max-affected, and not invalid")
	require.True(t, got.singleObject)
}

func TestPostgRESTPrefer_strictRefusesWithPGRST122AndLenientIgnores(t *testing.T) {
	t.Parallel()
	strict := pgrParsePrefer([]string{"handling=strict, foo=bar, baz"})
	err := strict.strictFault()
	require.Error(t, err)
	refused := err.(*pgrError)
	require.Equal(t, http.StatusBadRequest, refused.status)
	require.Equal(t, "PGRST122", refused.code)
	require.Equal(t, "Invalid preferences given with handling=strict", refused.message)
	require.Equal(t, "Invalid preferences: foo=bar, baz", refused.details)

	for _, header := range []string{"handling=lenient, foo=bar", "foo=bar", "return=minimal", "handling=strict, return=minimal"} {
		require.NoError(t, pgrParsePrefer([]string{header}).strictFault(), header)
	}
}

func TestPostgRESTPrefer_maxAffectedOnlyBindsUnderStrict(t *testing.T) {
	t.Parallel()
	strict := pgrParsePrefer([]string{"handling=strict, max-affected=2"})
	require.True(t, strict.exceeds(3))
	require.False(t, strict.exceeds(2))
	lenient := pgrParsePrefer([]string{"max-affected=2"})
	require.False(t, lenient.exceeds(3), "without handling=strict max-affected is ignored")
	negative := pgrParsePrefer([]string{"handling=strict, max-affected=-1"})
	require.True(t, negative.exceeds(0), "PostgREST reads -1 as a number and every write breaks it")
}

func TestPostgRESTPrefer_preferenceAppliedEchoesWhatPostgRESTEchoes(t *testing.T) {
	t.Parallel()
	everything := pgrParsePrefer([]string{"max-affected=7, handling=strict, tx=rollback, count=exact, return=representation, missing=default, resolution=merge-duplicates"})

	// Order: resolution, missing, return, count, tx, handling, max-affected.
	all := pgrApplied{resolution: true, missing: true, representation: true, maxAffected: true}
	require.Equal(t, "resolution=merge-duplicates, missing=default, return=representation, count=exact, tx=rollback, handling=strict, max-affected=7",
		everything.applied(all))

	// A read echoes count, tx and handling.
	require.Equal(t, "count=exact, tx=rollback, handling=strict", everything.applied(pgrApplied{}))
	// An insert has no max-affected, a delete no missing or resolution.
	require.Equal(t, "resolution=merge-duplicates, missing=default, return=representation, count=exact, tx=rollback, handling=strict",
		everything.applied(pgrApplied{resolution: true, missing: true, representation: true}))
	require.Equal(t, "return=representation, count=exact, tx=rollback, handling=strict, max-affected=7",
		everything.applied(pgrApplied{representation: true, maxAffected: true}))

	// max-affected is echoed under strict only.
	lenient := pgrParsePrefer([]string{"max-affected=7, handling=lenient"})
	require.Equal(t, "handling=lenient", lenient.applied(all))
	// Each of the others on its own.
	for header, want := range map[string]string{
		"return=minimal":                  "return=minimal",
		"return=headers-only":             "return=headers-only",
		"count=planned":                   "count=planned",
		"count=estimated":                 "count=estimated",
		"resolution=ignore-duplicates":    "resolution=ignore-duplicates",
		"missing=null":                    "missing=null",
		"tx=commit":                       "tx=commit",
		"handling=lenient":                "handling=lenient",
		"max-affected=5, handling=strict": "handling=strict, max-affected=5",
		"foo=bar":                         "",
		"":                                "",
	} {
		require.Equal(t, want, pgrParsePrefer([]string{header}).applied(all), header)
	}

	rec := httptest.NewRecorder()
	pgrParsePrefer([]string{"foo=bar"}).setApplied(rec, all)
	require.NotContains(t, rec.Header(), "Preference-Applied", "an unrecognised preference is not echoed")
	pgrParsePrefer([]string{"count=exact"}).setApplied(rec, all)
	require.Equal(t, "count=exact", rec.Header().Get("Preference-Applied"))
}

func TestPostgRESTLocation_isTheKeyAsRenderSimpleQueryWritesIt(t *testing.T) {
	t.Parallel()
	one, two := "7", "a b&c/é"
	require.Equal(t, "/notes?id=eq.7", pgrMutationResult{key: []*string{&one}}.location("notes", []string{"id"}))
	// A composite key, in the order given; a value is percent-encoded the way
	// urlEncode True does (space is %20, not +), and a NULL is is.null.
	require.Equal(t, "/t?a=eq.7&b=eq.a%20b%26c%2F%C3%A9", pgrMutationResult{key: []*string{&one, &two}}.location("t", []string{"a", "b"}))
	require.Equal(t, "/t?a=is.null", pgrMutationResult{key: []*string{nil}}.location("t", []string{"a"}))
	// More or fewer than the key, or none: no header.
	require.Empty(t, pgrMutationResult{}.location("t", []string{"a"}))
	require.Empty(t, pgrMutationResult{key: []*string{&one}}.location("t", nil))
}

func TestPostgRESTMissingTable_isPGRST205(t *testing.T) {
	t.Parallel()
	missing := pgrTableNotFound("userz", "users")
	require.Equal(t, http.StatusNotFound, missing.status)
	require.Equal(t, "PGRST205", missing.code)
	require.Equal(t, "Could not find the table 'public.userz' in the schema cache", missing.message)
	require.Equal(t, "Perhaps you meant the table 'public.users'", missing.hint)
	require.Empty(t, missing.details)
	require.Empty(t, pgrTableNotFound("userz", "").hint)
}

func TestPostgRESTWriteError_carriesProxyStatusAndTheChallenge(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err       error
		challenge string
	}{
		"a 401 from the Database": {&pgrError{status: 401, code: "42501", message: "permission denied"}, "Bearer"},
		"an own code":             {&pgrError{status: 400, code: "MS100", message: "too many"}, ""},
		"a token fault":           {pgrTokenRefused("another Project"), `Bearer error="invalid_token", error_description="No suitable key or wrong key type"`},
	} {
		rec := httptest.NewRecorder()
		pgrWriteError(rec, tc.err)
		require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"), name)
		require.Contains(t, rec.Header().Get("Proxy-Status"), "PostgREST; error=", name)
		require.Equal(t, tc.challenge, rec.Header().Get("WWW-Authenticate"), name)
		require.Contains(t, rec.Body.String(), `"details"`, name)
		require.Contains(t, rec.Body.String(), `"hint"`, name)
	}
}
