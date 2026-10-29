package controllers

// miniship (miniship-cloud#1888, ADR 0075): a User's token that does not
// verify against its Project's key set is refused as PostgREST 14 refuses a
// token, with the code and message of Error.hs and the WWW-Authenticate header.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"
)

func TestPostgRESTCaller_aUsersTokenThatFailsIsPostgRESTsRefusal(t *testing.T) {
	t.Parallel()
	install := newTwoProjects(t)
	rest := install.rest(t, gatewayHeader, time.Second)
	pool := install.pools[alphaProject][0]
	valid := pool.userToken(t, "u")
	claims := func(mutate func(map[string]any)) string {
		c := map[string]any{"sub": "u", "aud": "authenticated", "role": "authenticated", "exp": time.Now().Add(time.Hour).Unix()}
		mutate(c)
		return signWith(t, jose.EdDSA, pool.private, pool.kid, c)
	}
	stranger := newUserPool(t, pool.kid)

	for name, tc := range map[string]struct {
		token         string
		code, message string
	}{
		"expired":                {claims(func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }), "PGRST303", "JWT expired"},
		"not valid yet":          {claims(func(c map[string]any) { c["nbf"] = time.Now().Add(time.Hour).Unix() }), "PGRST303", "JWT not yet valid"},
		"another audience":       {claims(func(c map[string]any) { c["aud"] = "pool-app" }), "PGRST303", "JWT not in audience"},
		"no expiry":              {claims(func(c map[string]any) { delete(c, "exp") }), "PGRST303", "Parsing claims failed"},
		"another role":           {claims(func(c map[string]any) { c["role"] = "service_role" }), "PGRST303", "Parsing claims failed"},
		"wrong key":              {stranger.userToken(t, "u"), "PGRST301", "JWT cryptographic operation failed"},
		"a key id the set lacks": {newUserPool(t, "k_nobody").userToken(t, "u"), "PGRST301", "No suitable key or wrong key type"},
		"a changed signature":    {valid[:len(valid)-6] + "AAAAAA", "PGRST301", "JWT cryptographic operation failed"},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := callAt(t, rest, alphaProject, tc.token)
			require.Equal(t, http.StatusUnauthorized, status, body)
			var got map[string]any
			require.NoError(t, json.Unmarshal([]byte(body), &got), body)
			require.Equal(t, tc.code, got["code"], body)
			require.Equal(t, tc.message, got["message"], body)
			require.Contains(t, got, "details")
			require.Contains(t, got, "hint")
		})
	}
}

func TestPostgRESTCaller_theRefusalCarriesTheHeadersPostgRESTSends(t *testing.T) {
	t.Parallel()
	install := newTwoProjects(t)
	rest := install.rest(t, gatewayHeader, time.Second)
	expired := signWith(t, jose.EdDSA, install.pools[alphaProject][0].private, "k_alpha", map[string]any{
		"aud": "authenticated", "role": "authenticated", "exp": time.Now().Add(-time.Hour).Unix(),
	})
	req := httptest.NewRequest(http.MethodGet, "/rest/v1/notes", nil)
	req.Header.Set(gatewayHeader, alphaProject)
	req.Header.Set("Authorization", "Bearer "+expired)
	rec := httptest.NewRecorder()
	rest.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
	require.Equal(t, "PostgREST; error=PGRST303", rec.Header().Get("Proxy-Status"))
	require.Equal(t, `Bearer error="invalid_token", error_description="JWT expired"`, rec.Header().Get("WWW-Authenticate"))
}
