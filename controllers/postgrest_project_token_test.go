package controllers

// miniship (miniship-cloud#1428): on miniship cloud one rest serves every
// Project, and a token opens only the Project it is for. Two Projects here,
// prj_alpha and prj_beta, each with a User pool of its own key set, and the
// install's key, which the Check signs both Projects' tokens with. What is
// driven is the chain a call goes through: patch 18's token check, then the
// caller the /rest/v1 address resolves for the Project the gateway named.

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/middlewares"
	"github.com/prest/prest/v2/projectkeys"
)

const (
	alphaProject  = "prj_alpha"
	betaProject   = "prj_beta"
	gatewayHeader = "x-miniship-project"
)

// signWith is a JWT of claims, signed by key with alg under kid.
func signWith(t *testing.T, alg jose.SignatureAlgorithm, key any, kid string, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid))
	require.NoError(t, err)
	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	require.NoError(t, err)
	return token
}

// userPool is one Project's User pool: the keys its auth api signs with.
type userPool struct {
	kid     string
	private ed25519.PrivateKey
	public  ed25519.PublicKey
}

func newUserPool(t *testing.T, kid string) userPool {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return userPool{kid: kid, private: private, public: public}
}

// userToken is a User's token as the auth api mints one under /auth/v1.
func (p userPool) userToken(t *testing.T, sub string) string {
	t.Helper()
	return signWith(t, jose.EdDSA, p.private, p.kid, map[string]any{
		"sub": sub, "aud": "authenticated", "role": "authenticated",
		"exp": time.Now().Add(10 * time.Minute).Unix(),
	})
}

// twoProjects is the install: its own key, and each Project's User pool,
// published by a fake auth api that counts how often it is asked.
type twoProjects struct {
	install *ecdsa.PrivateKey
	jwks    string
	pools   map[string][]userPool
	asks    atomic.Int32
	keySets *httptest.Server
}

func newTwoProjects(t *testing.T) *twoProjects {
	t.Helper()
	install, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	raw, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &install.PublicKey, KeyID: "install", Algorithm: "ES256", Use: "sig",
	}}})
	require.NoError(t, err)
	p := &twoProjects{install: install, jwks: string(raw), pools: map[string][]userPool{
		alphaProject: {newUserPool(t, "k_alpha")},
		betaProject:  {newUserPool(t, "k_beta")},
	}}
	p.keySets = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.asks.Add(1)
		project := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/auth/v1/projects/"), "/.well-known/jwks.json")
		set := jose.JSONWebKeySet{}
		for _, pool := range p.pools[project] {
			set.Keys = append(set.Keys, jose.JSONWebKey{Key: pool.public, KeyID: pool.kid, Algorithm: "EdDSA", Use: "sig"})
		}
		_ = json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(p.keySets.Close)
	return p
}

// checkToken is the token the Check puts in place of a Project's key: ES256
// under the install's key, the key's role, and the Project it belongs to.
func (p *twoProjects) checkToken(t *testing.T, role, project string) string {
	t.Helper()
	claims := map[string]any{"iss": "miniship", "role": role, "exp": time.Now().Add(time.Hour).Unix()}
	if project != "" {
		claims["project"] = project
	}
	return signWith(t, jose.ES256, p.install, "install", claims)
}

// rest is the token check and the /rest/v1 caller, as a cloud install
// composes them: PROJECT_HEADER set, PROJECT_KEYS_URL at the fake auth api.
// It answers 200 and the role the call would read as, or the refusal.
func (p *twoProjects) rest(t *testing.T, projectHeader string, window time.Duration) http.Handler {
	t.Helper()
	check, err := middlewares.TokenRole("role", p.jwks, "ES256")
	require.NoError(t, err)
	h := &CRUDHandler{
		projectHeader: projectHeader,
		userTokens: projectkeys.New(func(project string) string {
			return p.keySets.URL + "/auth/v1/projects/" + project + "/.well-known/jwks.json"
		}, time.Second, window, 10),
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.ServeHTTP(w, r, func(w http.ResponseWriter, r *http.Request) {
			project := alphaProject
			if projectHeader != "" {
				project = r.Header.Get(projectHeader)
			}
			caller, err := h.resolveCaller(r, project, nil)
			if err != nil {
				pgrWriteError(w, err)
				return
			}
			_, _ = w.Write([]byte(caller.Role))
		})
	})
}

// callAt is one call on project's address carrying token, as the gateway
// hands it on: the Project in the header the Check wrote.
func callAt(t *testing.T, rest http.Handler, project, token string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/rest/v1/notes?select=*", nil)
	req.Header.Set(gatewayHeader, project)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	rest.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func requireRefused(t *testing.T, status int, body, why string) {
	t.Helper()
	require.Equal(t, http.StatusUnauthorized, status, why+": "+body)
	require.Contains(t, body, `"code":"PGRST301"`, why)
}

func TestPostgRESTCaller_aUsersTokenOpensOnlyItsOwnProject(t *testing.T) {
	t.Parallel()
	install := newTwoProjects(t)
	rest := install.rest(t, gatewayHeader, time.Second)
	alphaUser := install.pools[alphaProject][0].userToken(t, "u-alpha")

	// Alpha's User on alpha. Expected: authenticated.
	status, body := callAt(t, rest, alphaProject, alphaUser)
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "authenticated", body)

	// The same token on beta's address. Expected: 401 PGRST301, as PostgREST
	// refuses a token its key set did not sign.
	status, body = callAt(t, rest, betaProject, alphaUser)
	requireRefused(t, status, body, "alpha's User on beta")

	// Beta's own User on beta still reads.
	status, body = callAt(t, rest, betaProject, install.pools[betaProject][0].userToken(t, "u-beta"))
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "authenticated", body)
}

func TestPostgRESTCaller_theChecksTokenOpensOnlyTheProjectItNames(t *testing.T) {
	t.Parallel()
	install := newTwoProjects(t)
	rest := install.rest(t, gatewayHeader, time.Second)

	// The publishable key's token and the secret key's, on their own
	// Project. Expected: anon and service_role.
	for _, role := range []string{"anon", "service_role"} {
		status, body := callAt(t, rest, betaProject, install.checkToken(t, role, betaProject))
		require.Equal(t, http.StatusOK, status, body)
		require.Equal(t, role, body)
	}

	// Alpha's tokens carried to beta's address. The signature verifies, since
	// one install key signs both; the project claim is what refuses them.
	for _, role := range []string{"anon", "service_role"} {
		status, body := callAt(t, rest, betaProject, install.checkToken(t, role, alphaProject))
		requireRefused(t, status, body, role+" of alpha on beta")
		require.Contains(t, body, "another Project")
	}

	// An install token that names no Project (the api service token's
	// claims). Expected: refused on an address that is one Project's.
	status, body := callAt(t, rest, betaProject, install.checkToken(t, "service_role", ""))
	requireRefused(t, status, body, "the install's token with no project")
}

func TestPostgRESTCaller_anUnknownKeyIDIsReadAgainOnce(t *testing.T) {
	t.Parallel()
	install := newTwoProjects(t)
	// A window of zero: an unknown kid is always worth one read.
	rest := install.rest(t, gatewayHeader, 0)
	first := install.pools[alphaProject][0]

	status, body := callAt(t, rest, alphaProject, first.userToken(t, "u-alpha"))
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, int32(1), install.asks.Load())

	// Alpha's pool rotates. A token under the new kid reads alpha's set once
	// more, and verifies.
	rotated := newUserPool(t, "k_alpha_2")
	install.pools[alphaProject] = []userPool{rotated, first}
	status, body = callAt(t, rest, alphaProject, rotated.userToken(t, "u-alpha"))
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, int32(2), install.asks.Load())

	// A kid no pool holds: one read, then refused, and not a second read for
	// the same call.
	stranger := newUserPool(t, "k_nobody")
	status, body = callAt(t, rest, alphaProject, stranger.userToken(t, "u-alpha"))
	requireRefused(t, status, body, "a kid alpha's set does not hold")
	require.Equal(t, int32(3), install.asks.Load())
}

func TestPostgRESTCaller_aSelfHostInstallTakesTheInstallsTokens(t *testing.T) {
	t.Parallel()
	install := newTwoProjects(t)
	// No PROJECT_HEADER: the install's one Project, alpha here.
	rest := install.rest(t, "", time.Second)

	// The install's tokens name no Project and read as their role, as before.
	for _, role := range []string{"anon", "service_role"} {
		status, body := callAt(t, rest, "", install.checkToken(t, role, ""))
		require.Equal(t, http.StatusOK, status, body)
		require.Equal(t, role, body)
	}
	// Its User still verifies against the one Project's key set.
	status, body := callAt(t, rest, "", install.pools[alphaProject][0].userToken(t, "u-alpha"))
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "authenticated", body)
}
