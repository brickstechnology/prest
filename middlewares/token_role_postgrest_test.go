package middlewares

// miniship (miniship-cloud#1888, ADR 0075): on the SDK's address a token that
// fails is refused in PostgREST 14's words: 401, the four-field envelope,
// PGRST301 for a token that cannot be decoded or verified and PGRST303 for
// claims it cannot accept, with the Proxy-Status and WWW-Authenticate headers
// PostgREST sends (Error.hs, Auth/Jwt.hs). Every other address keeps its
// {"error": ...} body.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"

	pctx "github.com/prest/prest/v2/context"
)

type sdkAnswer struct {
	status  int
	reached bool
	role    string
	header  http.Header
	body    map[string]any
}

func onSDK(t *testing.T, key installKey, path, authorization string) sdkAnswer {
	t.Helper()
	check := tokenCheck(t, key.jwks, "ES256")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	check.ServeHTTP(rec, req)
	out := sdkAnswer{status: rec.Code, reached: rec.Header().Get("X-Reached") == "yes", role: rec.Header().Get("X-Role"), header: rec.Header()}
	if rec.Code == http.StatusUnauthorized {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out.body), rec.Body.String())
	}
	return out
}

func TestTokenRole_onTheSDKAddressARefusalIsPostgRESTs(t *testing.T) {
	key := newInstallKey(t, "install-key")
	stranger := newInstallKey(t, "install-key")
	otherKid := newInstallKey(t, "another-key")
	now := time.Now()
	expired := key.mint(t, jose.ES256, key.private, tokenClaims{
		Role: "service_role", Claims: jwt.Claims{Expiry: jwt.NewNumericDate(now.Add(-time.Hour))},
	})
	notYet := key.mint(t, jose.ES256, key.private, tokenClaims{
		Role: "service_role", Claims: jwt.Claims{
			NotBefore: jwt.NewNumericDate(now.Add(time.Hour)), Expiry: jwt.NewNumericDate(now.Add(2 * time.Hour)),
		},
	})
	good := key.serviceToken(t)

	for name, tc := range map[string]struct {
		authorization string
		code, message string
		details       any
	}{
		"malformed (abc.def.ghi)": {"Bearer abc.def.ghi", "PGRST301", "JWT couldn't be decoded", nil},
		"two parts":               {"Bearer abc.def", "PGRST301", "Expected 3 parts in JWT; got 2", nil},
		"an api key, not a token": {"Bearer ms_secret_an-api-secret-key", "PGRST301", "Expected 3 parts in JWT; got 1", nil},
		"empty Bearer":            {"Bearer ", "PGRST301", "Empty JWT is sent in Authorization header", nil},
		"wrong key, same kid":     {"Bearer " + stranger.serviceToken(t), "PGRST301", "JWT cryptographic operation failed", nil},
		"a kid the set lacks":     {"Bearer " + otherKid.serviceToken(t), "PGRST301", "No suitable key or wrong key type", "No suitable key was found to decode the JWT"},
		"a signature changed":     {"Bearer " + good[:len(good)-6] + "AAAAAA", "PGRST301", "JWT cryptographic operation failed", nil},
		"expired":                 {"Bearer " + expired, "PGRST303", "JWT expired", nil},
		"not valid yet":           {"Bearer " + notYet, "PGRST303", "JWT not yet valid", nil},
		"no expiry":               {"Bearer " + key.mint(t, jose.ES256, key.private, tokenClaims{Role: "service_role"}), "PGRST303", "Parsing claims failed", "the token has no exp claim"},
		"no role": {"Bearer " + key.mint(t, jose.ES256, key.private, tokenClaims{Claims: jwt.Claims{Expiry: jwt.NewNumericDate(now.Add(time.Hour))}}),
			"PGRST303", "Parsing claims failed", "the token names no role"},
	} {
		t.Run(name, func(t *testing.T) {
			got := onSDK(t, key, "/rest/v1/notes", tc.authorization)
			require.Equal(t, http.StatusUnauthorized, got.status)
			require.False(t, got.reached)
			require.Equal(t, map[string]any{"code": tc.code, "message": tc.message, "details": tc.details, "hint": nil}, got.body)
			require.Equal(t, "application/json; charset=utf-8", got.header.Get("Content-Type"))
			require.Equal(t, "PostgREST; error="+tc.code, got.header.Get("Proxy-Status"))
			require.Equal(t, `Bearer error="invalid_token", error_description="`+tc.message+`"`, got.header.Get("WWW-Authenticate"))
		})
	}
}

func TestTokenRole_onTheSDKAddressOnlyABearerHeaderIsAToken(t *testing.T) {
	key := newInstallKey(t, "install-key")
	// PostgREST extracts a token from a Bearer header only (Wai's
	// extractBearerAuth), so any other Authorization is the anonymous role's
	// call, not a refusal.
	for _, authorization := range []string{"Basic b3BlcmF0b3I6cGFzc3dvcmQ=", key.serviceToken(t)} {
		got := onSDK(t, key, "/rest/v1/notes", authorization)
		require.Equal(t, http.StatusOK, got.status, authorization)
		require.True(t, got.reached)
		require.Empty(t, got.role, "no token must leave the Database's anonymous role in place")
	}
	// Elsewhere it is still refused.
	got := onSDK(t, key, "/prj_a/public/posts", "Basic b3BlcmF0b3I6cGFzc3dvcmQ=")
	require.Equal(t, http.StatusUnauthorized, got.status)
}

func TestTokenRole_otherAddressesKeepTheirBody(t *testing.T) {
	key := newInstallKey(t, "install-key")
	got := onSDK(t, key, "/prj_a/public/posts", "Bearer abc.def.ghi")
	require.Equal(t, http.StatusUnauthorized, got.status)
	require.Equal(t, `Bearer error="invalid_token"`, got.header.Get("WWW-Authenticate"))
	require.Contains(t, got.body, "error")
	require.NotContains(t, got.body, "code")
	require.Empty(t, got.header.Get("Proxy-Status"))
}

func TestTokenRole_aUsersTokenStillPassesToTheHandler(t *testing.T) {
	key := newInstallKey(t, "install-key")
	// A token signed EdDSA is a User's: the handler verifies it against the
	// Project's key set.
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: edKey(t)},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k_pool"))
	require.NoError(t, err)
	token, err := jwt.Signed(signer).Claims(map[string]any{"role": "authenticated"}).Serialize()
	require.NoError(t, err)

	check, err := TokenRole("role", key.jwks, "ES256")
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/rest/v1/notes", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	var carried string
	check.ServeHTTP(rec, req, func(w http.ResponseWriter, r *http.Request) {
		carried, _ = pctx.UserToken(r.Context())
	})
	require.Equal(t, token, carried)
}

func TestRenderFormat_keepsTheContentTypeAHandlerChoseForJSON(t *testing.T) {
	handler := HandlerSet()
	for contentType, want := range map[string]string{
		"application/json; charset=utf-8": "application/json; charset=utf-8",
		"application/json":                "application/json",
		"text/plain; charset=utf-8":       "application/json",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/v1/notes", nil), func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", contentType)
			_, _ = w.Write([]byte(`[]`))
		})
		require.Equal(t, want, rec.Header().Get("Content-Type"), contentType)
	}
}

func edKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return private
}
