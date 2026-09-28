package middlewares

// miniship (miniship-cloud#801): the token check private ADR 0030 asks of
// rest. A verified token becomes the role it names, no token keeps the
// Database's anonymous role, and a token that fails is refused.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/config"
	pctx "github.com/prest/prest/v2/context"
)

// installKey is an install's signing key pair, and jwks is the public half as
// the install publishes it: one ES256 key, found by its kid.
type installKey struct {
	private *ecdsa.PrivateKey
	kid     string
	jwks    string
}

func newInstallKey(t *testing.T, kid string) installKey {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &private.PublicKey, KeyID: kid, Algorithm: "ES256", Use: "sig",
	}}}
	raw, err := json.Marshal(set)
	require.NoError(t, err)
	return installKey{private: private, kid: kid, jwks: string(raw)}
}

// tokenClaims is what the install's api service token carries.
type tokenClaims struct {
	Role string `json:"role,omitempty"`
	jwt.Claims
}

func (k installKey) mint(t *testing.T, alg jose.SignatureAlgorithm, key any, claims tokenClaims) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", k.kid))
	require.NoError(t, err)
	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	require.NoError(t, err)
	return token
}

// serviceToken is an api service token as setup mints it: ES256, role
// service_role, a long expiry.
func (k installKey) serviceToken(t *testing.T) string {
	t.Helper()
	return k.mint(t, jose.ES256, k.private, tokenClaims{
		Role: "service_role",
		Claims: jwt.Claims{
			Issuer:   "miniship",
			IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
			Expiry:   jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
		},
	})
}

// through sends one request through the check and says what reached the
// handler behind it: whether anything did, and the role it carried.
func through(t *testing.T, check http.Handler, authorization string) (status int, reached bool, role string, hasRole bool) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/prj_a/public/posts", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	check.ServeHTTP(rec, req)
	return rec.Code, rec.Header().Get("X-Reached") == "yes", rec.Header().Get("X-Role"), rec.Header().Get("X-Has-Role") == "yes"
}

func tokenCheck(t *testing.T, jwks, algo string) http.Handler {
	t.Helper()
	mw, err := TokenRole("role", jwks, algo)
	require.NoError(t, err)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mw.ServeHTTP(w, r, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Reached", "yes")
			if role, ok := pctx.TokenRole(r.Context()); ok {
				w.Header().Set("X-Has-Role", "yes")
				w.Header().Set("X-Role", role)
			}
			w.WriteHeader(http.StatusOK)
		})
	})
}

func TestTokenRole_aVerifiedTokenBecomesTheRoleItNames(t *testing.T) {
	key := newInstallKey(t, "install-key")
	check := tokenCheck(t, key.jwks, "ES256")

	status, reached, role, hasRole := through(t, check, "Bearer "+key.serviceToken(t))
	require.Equal(t, http.StatusOK, status)
	require.True(t, reached)
	require.True(t, hasRole, "the verified token's role did not reach the read")
	require.Equal(t, "service_role", role)

	// The scheme is matched as RFC 9110 matches one, without regard to case.
	status, _, role, _ = through(t, check, "bearer "+key.serviceToken(t))
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "service_role", role)
}

func TestTokenRole_noTokenKeepsTheAnonymousRole(t *testing.T) {
	key := newInstallKey(t, "install-key")
	check := tokenCheck(t, key.jwks, "ES256")

	status, reached, _, hasRole := through(t, check, "")
	require.Equal(t, http.StatusOK, status, "a visitor with no token was refused")
	require.True(t, reached)
	require.False(t, hasRole, "no token must leave the Database's anonymous role in place")
}

func TestTokenRole_aTokenThatFailsIsRefused(t *testing.T) {
	key := newInstallKey(t, "install-key")
	check := tokenCheck(t, key.jwks, "ES256")
	good := key.serviceToken(t)
	now := time.Now()

	stranger := newInstallKey(t, "install-key")
	otherKid := newInstallKey(t, "another-key")
	hmacKey := []byte("a thirty-two byte hmac key, long")

	for name, authorization := range map[string]string{
		"a signature changed": "Bearer " + good[:len(good)-6] + "AAAAAA",
		"signed by a key outside the JWKS, under the same kid": "Bearer " +
			stranger.serviceToken(t),
		"a kid the JWKS does not hold": "Bearer " + otherKid.serviceToken(t),
		"HS256": "Bearer " + key.mint(t, jose.HS256, hmacKey, tokenClaims{
			Role: "service_role", Claims: jwt.Claims{Expiry: jwt.NewNumericDate(now.Add(time.Hour))},
		}),
		"expired": "Bearer " + key.mint(t, jose.ES256, key.private, tokenClaims{
			Role: "service_role", Claims: jwt.Claims{Expiry: jwt.NewNumericDate(now.Add(-time.Hour))},
		}),
		"no expiry": "Bearer " + key.mint(t, jose.ES256, key.private, tokenClaims{
			Role: "service_role",
		}),
		"not valid yet": "Bearer " + key.mint(t, jose.ES256, key.private, tokenClaims{
			Role: "service_role", Claims: jwt.Claims{
				NotBefore: jwt.NewNumericDate(now.Add(time.Hour)),
				Expiry:    jwt.NewNumericDate(now.Add(2 * time.Hour)),
			},
		}),
		"no role": "Bearer " + key.mint(t, jose.ES256, key.private, tokenClaims{
			Claims: jwt.Claims{Expiry: jwt.NewNumericDate(now.Add(time.Hour))},
		}),
		"not a JWT":              "Bearer ms_secret_an-api-secret-key-is-not-a-token",
		"an empty Bearer":        "Bearer ",
		"another scheme":         "Basic b3BlcmF0b3I6cGFzc3dvcmQ=",
		"a token with no scheme": good,
	} {
		t.Run(name, func(t *testing.T) {
			status, reached, _, _ := through(t, check, authorization)
			require.Equal(t, http.StatusUnauthorized, status)
			require.False(t, reached, "a token that failed reached the read, as the anonymous role or as anything else")
		})
	}
}

func TestTokenRole_theRefusalSaysWhyInPostgRESTsWordsAndQuotesNothingBack(t *testing.T) {
	key := newInstallKey(t, "install-key")
	mw, err := TokenRole("role", key.jwks, "ES256")
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/prj_a/public/posts", nil)
	req.Header.Set("Authorization", "Bearer not.a.token-with-a-distinctive-tail")
	mw.ServeHTTP(rec, req, func(http.ResponseWriter, *http.Request) { t.Fatal("reached the read") })

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, `Bearer error="invalid_token"`, rec.Header().Get("WWW-Authenticate"))
	require.NotContains(t, rec.Body.String(), "distinctive-tail")
}

func TestTokenRole_isRefusedWhenItCouldVerifyNothing(t *testing.T) {
	key := newInstallKey(t, "install-key")

	// No JWKS: the check could verify no token at all, and a check that
	// cannot verify is not one to run behind.
	_, err := TokenRole("role", "", "ES256")
	require.Error(t, err)
	_, err = TokenRole("role", `{"keys":[]}`, "ES256")
	require.Error(t, err)
	_, err = TokenRole("role", "not json", "ES256")
	require.Error(t, err)

	// A shared-secret algorithm: an api service token is ES256, and a JWKS of
	// public keys can verify no HMAC.
	for _, algo := range []string{"HS256", "HS384", "HS512", "none", ""} {
		_, err = TokenRole("role", key.jwks, algo)
		require.Error(t, err, algo)
	}
}

func TestNew_aRoleClaimTurnsTheTokenRoleCheckOn(t *testing.T) {
	key := newInstallKey(t, "install-key")
	cfg := &config.Prest{
		JWTRoleClaim: "role",
		JWTJWKS:      key.jwks,
		JWTAlgo:      "ES256",
		// Debug turns upstream's check off. It must not turn this one off:
		// a debug flag is not a reason to serve a forged token.
		Debug: true,
	}
	n := New(cfg)
	n.UseHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if role, ok := pctx.TokenRole(r.Context()); ok {
			w.Header().Set("X-Role", role)
		}
		w.WriteHeader(http.StatusOK)
	})

	for _, c := range []struct {
		authorization string
		status        int
		role          string
	}{
		{"", http.StatusOK, ""},
		{"Bearer " + key.serviceToken(t), http.StatusOK, "service_role"},
		{"Bearer forged", http.StatusUnauthorized, ""},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/prj_a/public/posts", nil)
		if c.authorization != "" {
			req.Header.Set("Authorization", c.authorization)
		}
		n.ServeHTTP(rec, req)
		require.Equal(t, c.status, rec.Code, c.authorization)
		require.Equal(t, c.role, rec.Header().Get("X-Role"), c.authorization)
	}

	// And a role claim with no JWKS answers every request 500, rather than
	// serving a token it cannot check as the anonymous role.
	broken := New(&config.Prest{JWTRoleClaim: "role", JWTAlgo: "ES256"})
	broken.UseHandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	rec := httptest.NewRecorder()
	broken.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/prj_a/public/posts", nil))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}
