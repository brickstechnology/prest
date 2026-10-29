package pgrwire

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWrite_hasAllFourKeysAndPostgRESTsHeaders(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	Write(rec, Error{Status: http.StatusNotFound, Code: "PGRST205", Message: "m"})
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, `{"code":"PGRST205","details":null,"hint":null,"message":"m"}`, rec.Body.String())
	require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
	require.Equal(t, "PostgREST; error=PGRST205", rec.Header().Get("Proxy-Status"))
	require.Empty(t, rec.Header().Get("WWW-Authenticate"), "only a 401 carries a challenge")
}

func TestWrite_aPlain401ChallengesWithBearer(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	Write(rec, Error{Status: http.StatusUnauthorized, Code: "42501", Message: "permission denied", Details: "d", Hint: "h"})
	require.Equal(t, "Bearer", rec.Header().Get("WWW-Authenticate"))
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "d", body["details"])
	require.Equal(t, "h", body["hint"])
}

func TestInvalidTokenChallenge_quotesTheDescriptionAsHaskellShowsIt(t *testing.T) {
	t.Parallel()
	require.Equal(t, `Bearer error="invalid_token", error_description="JWT expired"`, InvalidTokenChallenge("JWT expired"))
	require.Equal(t, `Bearer error="invalid_token", error_description="The JWT 'exp' claim must be a number"`,
		InvalidTokenChallenge("The JWT 'exp' claim must be a number"))
}
