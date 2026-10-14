package projectkeys

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

// pool is one Project's User pool: its signing key and the kid it publishes.
type pool struct {
	kid     string
	private ed25519.PrivateKey
	public  ed25519.PublicKey
}

func newPool(t *testing.T, kid string) pool {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return pool{kid: kid, private: private, public: public}
}

func (p pool) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: p.private},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", p.kid))
	require.NoError(t, err)
	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	require.NoError(t, err)
	return token
}

func userClaims(sub string) map[string]any {
	return map[string]any{
		"sub":  sub,
		"aud":  "authenticated",
		"role": "authenticated",
		"exp":  time.Now().Add(10 * time.Minute).Unix(),
	}
}

// authAPI answers each Project's key set, and counts how often it was asked.
type authAPI struct {
	server *httptest.Server
	asks   atomic.Int32
	sets   map[string][]pool
}

func newAuthAPI(t *testing.T, sets map[string][]pool) *authAPI {
	t.Helper()
	a := &authAPI{sets: sets}
	a.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.asks.Add(1)
		project := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/auth/v1/projects/"), "/.well-known/jwks.json")
		keys := jose.JSONWebKeySet{}
		for _, p := range a.sets[project] {
			keys.Keys = append(keys.Keys, jose.JSONWebKey{Key: p.public, KeyID: p.kid, Algorithm: "EdDSA", Use: "sig"})
		}
		_ = json.NewEncoder(w).Encode(keys)
	}))
	t.Cleanup(a.server.Close)
	return a
}

func (a *authAPI) source(window time.Duration) *Source {
	return New(func(project string) string {
		return a.server.URL + "/auth/v1/projects/" + project + "/.well-known/jwks.json"
	}, time.Second, window, 10)
}

func TestVerify_aUsersTokenVerifiesAgainstItsProjectsKeySet(t *testing.T) {
	alpha := newPool(t, "k_alpha")
	api := newAuthAPI(t, map[string][]pool{"prj_alpha": {alpha}})
	source := api.source(time.Second)

	role, claims, err := source.Verify(context.Background(), "prj_alpha", alpha.sign(t, userClaims("u-1")))
	require.NoError(t, err)
	require.Equal(t, "authenticated", role)
	var named map[string]any
	require.NoError(t, json.Unmarshal(claims, &named))
	require.Equal(t, "u-1", named["sub"])

	// A second token of the same Project costs no second read.
	_, _, err = source.Verify(context.Background(), "prj_alpha", alpha.sign(t, userClaims("u-2")))
	require.NoError(t, err)
	require.Equal(t, int32(1), api.asks.Load())
}

func TestVerify_aTokenOfAnotherProjectIsRefused(t *testing.T) {
	alpha, beta := newPool(t, "k_alpha"), newPool(t, "k_beta")
	api := newAuthAPI(t, map[string][]pool{"prj_alpha": {alpha}, "prj_beta": {beta}})
	source := api.source(time.Second)

	_, _, err := source.Verify(context.Background(), "prj_alpha", beta.sign(t, userClaims("u-1")))
	require.ErrorIs(t, err, ErrRefused)

	// The same kid, another Project's key: the signature is what decides.
	impostor := newPool(t, "k_alpha")
	_, _, err = source.Verify(context.Background(), "prj_alpha", impostor.sign(t, userClaims("u-1")))
	require.ErrorIs(t, err, ErrRefused)
}

func TestVerify_anUnknownKeyIDIsReadAgainOnceAWindow(t *testing.T) {
	old, rotated := newPool(t, "k_old"), newPool(t, "k_new")
	api := newAuthAPI(t, map[string][]pool{"prj_alpha": {old}})
	source := api.source(time.Hour)
	ctx := context.Background()

	_, _, err := source.Verify(ctx, "prj_alpha", old.sign(t, userClaims("u-1")))
	require.NoError(t, err)

	// The pool rotates; a token signed with the new key arrives inside the
	// window. Expected: refused without a second read.
	api.sets["prj_alpha"] = []pool{rotated, old}
	_, _, err = source.Verify(ctx, "prj_alpha", rotated.sign(t, userClaims("u-1")))
	require.ErrorIs(t, err, ErrRefused)
	require.Equal(t, int32(1), api.asks.Load())

	// Past the window, one read, and it verifies.
	source.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	claims := userClaims("u-1")
	claims["exp"] = time.Now().Add(3 * time.Hour).Unix()
	_, _, err = source.Verify(ctx, "prj_alpha", rotated.sign(t, claims))
	require.NoError(t, err)
	require.Equal(t, int32(2), api.asks.Load())
}

func TestVerify_whatAUsersTokenMustSay(t *testing.T) {
	alpha := newPool(t, "k_alpha")
	api := newAuthAPI(t, map[string][]pool{"prj_alpha": {alpha}})
	source := api.source(time.Second)

	for name, change := range map[string]func(map[string]any){
		"expired":               func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() },
		"no expiry":             func(c map[string]any) { delete(c, "exp") },
		"the role service_role": func(c map[string]any) { c["role"] = "service_role" },
		"an App's audience":     func(c map[string]any) { c["aud"] = "app_signs_people_in" },
	} {
		claims := userClaims("u-1")
		change(claims)
		_, _, err := source.Verify(context.Background(), "prj_alpha", alpha.sign(t, claims))
		require.ErrorIs(t, err, ErrRefused, name)
	}
}

func TestVerify_aKeySetThatCannotBeReadIsUnavailable(t *testing.T) {
	alpha := newPool(t, "k_alpha")
	api := newAuthAPI(t, nil)
	api.server.Close()
	source := api.source(time.Second)

	_, _, err := source.Verify(context.Background(), "prj_alpha", alpha.sign(t, userClaims("u-1")))
	require.True(t, errors.Is(err, ErrUnavailable), err)
}
