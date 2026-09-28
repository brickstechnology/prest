package anonymousrole_test

// miniship (miniship-cloud#801, private ADR 0030): rest verifies the token it
// is handed and becomes the role the token names. Asked of a live Postgres,
// which says who the read ran as: a view of current_user, and the posts table
// whose row security shows each role its own rows.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/config"
)

// service is this package's service_role: NOLOGIN and BYPASSRLS, as a
// Project's Database's service_role is (public #794), and granted to the
// login beside the reader, so rest can become either.
const service = "rest_anonrole_service"

// theInstall is an install's signing key and its JWKS.
type theInstall struct {
	private *ecdsa.PrivateKey
	jwks    string
}

func newInstall(t *testing.T) theInstall {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	raw, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &private.PublicKey, KeyID: "install", Algorithm: "ES256", Use: "sig",
	}}})
	require.NoError(t, err)
	return theInstall{private: private, jwks: string(raw)}
}

// token is an ES256 token for role, signed with key under the install's kid.
func token(t *testing.T, key *ecdsa.PrivateKey, role string) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "install"))
	require.NoError(t, err)
	serialized, err := jwt.Signed(signer).Claims(struct {
		Role string `json:"role"`
		jwt.Claims
	}{role, jwt.Claims{Issuer: "miniship", Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour))}}).Serialize()
	require.NoError(t, err)
	return serialized
}

// stageTokenRole adds what these subjects read to the package's staging:
// service_role, a view that answers who the read ran as, and the grants.
func stageTokenRole(t *testing.T, db *sql.DB) {
	t.Helper()
	exec(t, db, fmt.Sprintf(`
		DROP VIEW IF EXISTS public.rest_anonrole_whoami;
		DO $$ BEGIN
			IF EXISTS (SELECT FROM pg_roles WHERE rolname = '%[1]s') THEN
				EXECUTE 'DROP OWNED BY %[1]s';
				EXECUTE 'DROP ROLE %[1]s';
			END IF;
		END $$;
		CREATE ROLE %[1]s NOLOGIN BYPASSRLS;
		GRANT %[1]s TO %[2]s;
		GRANT SELECT ON public.rest_anonrole_posts TO %[1]s;
		CREATE VIEW public.rest_anonrole_whoami AS SELECT current_user::text AS role;
		GRANT SELECT ON public.rest_anonrole_whoami TO %[1]s, %[3]s;
	`, service, login, reader))
	t.Cleanup(func() {
		exec(t, db, fmt.Sprintf(`
			DROP VIEW IF EXISTS public.rest_anonrole_whoami;
			DROP OWNED BY %[1]s;
			DROP ROLE %[1]s;
		`, service))
	})
}

func restCheckingTokens(t *testing.T, cfg *config.Prest, install theInstall) *httptest.Server {
	t.Helper()
	return restAs(t, cfg, reader, func(c *config.Prest) {
		c.JWTRoleClaim = "role"
		c.JWTAlgo = "ES256"
		c.JWTJWKS = install.jwks
	})
}

func getWith(t *testing.T, server *httptest.Server, path, authorization string) (int, string) {
	t.Helper()
	header := http.Header{}
	if authorization != "" {
		header.Set("Authorization", authorization)
	}
	status, _, body := call(t, server, http.MethodGet, path, header)
	return status, body
}

func TestRest_aVerifiedTokenBecomesTheRoleItNames(t *testing.T) {
	cfg, db := needsPostgres(t)
	stageTokenRole(t, db)
	install := newInstall(t)
	rest := restCheckingTokens(t, cfg, install)
	bearer := "Bearer " + token(t, install.private, service)

	// Ask the Database who the read ran as.
	// Expected: the role the token names, not the anonymous role and not the
	// login.
	status, body := getWith(t, rest, "/"+alias+"/public/rest_anonrole_whoami", bearer)
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `[{"role": "`+service+`"}]`, body)

	// Read the posts table, whose policy shows the reader two rows.
	// Expected: all four, because service_role bypasses row security.
	status, body = getWith(t, rest, "/"+alias+"/public/rest_anonrole_posts?_count=*&_count_first=true", bearer)
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `{"count": 4}`, body)
}

func TestRest_noTokenIsTheAnonymousRole(t *testing.T) {
	cfg, db := needsPostgres(t)
	stageTokenRole(t, db)
	rest := restCheckingTokens(t, cfg, newInstall(t))

	// The same two reads with no token, on a rest that checks tokens.
	// Expected: the anonymous role, and the two rows its policy shows it.
	status, body := getWith(t, rest, "/"+alias+"/public/rest_anonrole_whoami", "")
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `[{"role": "`+reader+`"}]`, body)

	status, body = getWith(t, rest, "/"+alias+"/public/rest_anonrole_posts?_count=*&_count_first=true", "")
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `{"count": 2}`, body)
}

func TestRest_aTokenThatFailsIsRefused(t *testing.T) {
	cfg, db := needsPostgres(t)
	stageTokenRole(t, db)
	install := newInstall(t)
	rest := restCheckingTokens(t, cfg, install)

	forger, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	for name, authorization := range map[string]string{
		"signed by a key outside the JWKS": "Bearer " + token(t, forger, service),
		"not a token":                      "Bearer ms_secret_an-api-secret-key-is-not-a-token",
	} {
		// Ask with a token that does not verify.
		// Expected: 401, and no row — not the anonymous role's rows either.
		status, body := getWith(t, rest, "/"+alias+"/public/rest_anonrole_posts", authorization)
		require.Equal(t, http.StatusUnauthorized, status, name)
		require.NotContains(t, body, "a post", name)
	}
}
