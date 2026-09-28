package middlewares

// miniship (miniship-cloud#801, private ADR 0030): rest verifies the token it
// is handed and becomes the role the token names, as PostgREST does.
//
//   - A request whose Authorization carries a token that verifies against the
//     install's JWKS reads as the role in the token's role claim. For the
//     install's api service token, that is service_role.
//   - A request with no Authorization at all reads as its Database's anonymous
//     role, as every request did before this.
//   - Anything else in Authorization is refused, 401, before any Database is
//     asked about: a token that fails is never served as the anonymous role.
//
// Upstream's JwtMiddleware is left in the tree and in New's upstream branch.
// It does two things this does not: it refuses a request with no token, and it
// throws the token's claims away, so a verified token could not change which
// role a read becomes. miniship-cloud#801's thread has the measurement.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/urfave/negroni/v3"

	pctx "github.com/prest/prest/v2/context"
)

var (
	// ErrTokenRoleNoJWKS is a token check that was given no key to verify with.
	ErrTokenRoleNoJWKS = errors.New("the token check was given no JWKS to verify with")
	// ErrTokenRoleAlgorithm is a token check asked to verify a shared-secret
	// algorithm, which a JWKS of public keys cannot.
	ErrTokenRoleAlgorithm = errors.New("the token check verifies an asymmetric algorithm only")
)

// The refusal is PostgREST's shape: 401 with a Bearer challenge. The body
// says which check failed and quotes nothing the caller sent.
const tokenRoleChallenge = `Bearer error="invalid_token"`

// TokenRole is the token check. claim names the claim that carries the role,
// jwks is the install's public keys as JSON, and algo is the one algorithm a
// token may be signed with.
//
// The JWKS is read once, here. A rotated key reaches rest when it restarts,
// which is the restart the install's rotation already prints.
func TokenRole(claim, jwks, algo string) (negroni.Handler, error) {
	alg, err := tokenRoleAlgorithm(algo)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(jwks) == "" {
		return nil, ErrTokenRoleNoJWKS
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal([]byte(jwks), &set); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrJWKSetParse, err)
	}
	if len(set.Keys) == 0 {
		return nil, ErrTokenRoleNoJWKS
	}
	if claim == "" {
		claim = "role"
	}
	check := tokenRoleCheck{claim: claim, alg: alg, keys: set, now: time.Now}
	return negroni.HandlerFunc(check.serve), nil
}

// tokenRoleAlgorithm admits the asymmetric algorithms and nothing else. An
// empty algo is refused rather than defaulted: upstream's default is HS256.
func tokenRoleAlgorithm(algo string) (jose.SignatureAlgorithm, error) {
	switch jose.SignatureAlgorithm(algo) {
	case jose.ES256, jose.ES384, jose.ES512,
		jose.RS256, jose.RS384, jose.RS512,
		jose.PS256, jose.PS384, jose.PS512,
		jose.EdDSA:
		return jose.SignatureAlgorithm(algo), nil
	}
	return "", fmt.Errorf("%w: %q", ErrTokenRoleAlgorithm, algo)
}

type tokenRoleCheck struct {
	claim string
	alg   jose.SignatureAlgorithm
	keys  jose.JSONWebKeySet
	now   func() time.Time
}

func (c tokenRoleCheck) serve(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	values, present := r.Header["Authorization"]
	if !present {
		// No token: the Database's anonymous role, as before.
		next(w, r)
		return
	}
	if len(values) != 1 {
		c.refuse(w, "more than one Authorization header")
		return
	}
	role, why := c.verify(values[0])
	if why != "" {
		c.refuse(w, why)
		return
	}
	next(w, r.WithContext(pctx.WithTokenRole(r.Context(), role)))
}

// verify answers the role the token names, or why it is refused.
func (c tokenRoleCheck) verify(authorization string) (role, why string) {
	scheme, token, found := strings.Cut(authorization, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", "Authorization is not a Bearer token"
	}
	parsed, err := jwt.ParseSigned(strings.TrimSpace(token), []jose.SignatureAlgorithm{c.alg})
	if err != nil || len(parsed.Headers) != 1 {
		return "", "the token is not a JWT signed with " + string(c.alg)
	}
	keys := c.keys.Key(parsed.Headers[0].KeyID)
	if len(keys) != 1 {
		return "", ErrJWKSetKeyNotFound.Error()
	}
	var registered jwt.Claims
	var named map[string]any
	if err := parsed.Claims(keys[0].Public(), &registered, &named); err != nil {
		return "", "the token's signature does not verify"
	}
	if registered.Expiry == nil {
		return "", "the token has no expiry"
	}
	if err := registered.ValidateWithLeeway(jwt.Expected{Time: c.now()}, jwt.DefaultLeeway); err != nil {
		return "", "the token is expired or not valid yet"
	}
	role, _ = named[c.claim].(string)
	if role == "" {
		return "", "the token names no role"
	}
	return role, ""
}

func (c tokenRoleCheck) refuse(w http.ResponseWriter, why string) {
	slog.Warn("rest refused a token", "why", why)
	w.Header().Set("WWW-Authenticate", tokenRoleChallenge)
	http.Error(w, fmt.Sprintf(jsonErrFormat, why), http.StatusUnauthorized)
}
