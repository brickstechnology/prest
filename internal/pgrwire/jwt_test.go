package pgrwire

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

var theNow = time.Unix(1_800_000_000, 0)

type signer struct {
	key *ecdsa.PrivateKey
	kid string
}

func newSigner(t *testing.T, kid string) signer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return signer{key: key, kid: kid}
}

func (s signer) lookup(kid string) (jose.JSONWebKey, bool, error) {
	if kid != s.kid {
		return jose.JSONWebKey{}, false, nil
	}
	return jose.JSONWebKey{Key: &s.key.PublicKey, KeyID: s.kid, Algorithm: "ES256"}, true, nil
}

func (s signer) mint(t *testing.T, claims map[string]any) string {
	t.Helper()
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: s.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", s.kid))
	require.NoError(t, err)
	token, err := jwt.Signed(sig).Claims(claims).Serialize()
	require.NoError(t, err)
	return token
}

func (s signer) mintRaw(t *testing.T, payload string) string {
	t.Helper()
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: s.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", s.kid))
	require.NoError(t, err)
	signed, err := sig.Sign([]byte(payload))
	require.NoError(t, err)
	token, err := signed.CompactSerialize()
	require.NoError(t, err)
	return token
}

func fault(t *testing.T, err error) *TokenError {
	t.Helper()
	require.Error(t, err)
	got, ok := err.(*TokenError)
	require.True(t, ok, "%T %v", err, err)
	return got
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestDecode_refusesAsPostgREST14Does(t *testing.T) {
	t.Parallel()
	known := newSigner(t, "k1")
	stranger := newSigner(t, "k1")
	other := newSigner(t, "k2")
	valid := known.mint(t, map[string]any{"role": "anon", "exp": theNow.Add(time.Hour).Unix()})
	hs256 := b64(`{"alg":"HS256","typ":"JWT"}`) + "." + b64(`{"role":"anon"}`) + ".c2ln"
	none := b64(`{"alg":"none"}`) + "." + b64(`{}`) + "."

	for name, tc := range map[string]struct {
		token                  string
		code, message, details string
	}{
		"empty":             {"", "PGRST301", "Empty JWT is sent in Authorization header", ""},
		"two parts":         {"abc.def", "PGRST301", "Expected 3 parts in JWT; got 2", ""},
		"four parts":        {"a.b.c.d", "PGRST301", "Expected 3 parts in JWT; got 4", ""},
		"one part":          {"ms_secret_an-api-secret-key", "PGRST301", "Expected 3 parts in JWT; got 1", ""},
		"abc.def.ghi":       {"abc.def.ghi", "PGRST301", "JWT couldn't be decoded", ""},
		"not base64":        {"%%%.%%%.%%%", "PGRST301", "JWT couldn't be decoded", ""},
		"header not json":   {b64("nope") + "." + b64("{}") + ".c2ln", "PGRST301", "JWT couldn't be decoded", ""},
		"another algorithm": {hs256, "PGRST301", "Wrong or unsupported encoding algorithm", "Expected 'alg' doesn't match JWS header"},
		"alg none":          {none, "PGRST301", "Wrong or unsupported encoding algorithm", "JWT is unsecured but expected 'alg' was not 'none'"},
		"a key the set lacks": {other.mint(t, map[string]any{"exp": theNow.Add(time.Hour).Unix()}),
			"PGRST301", "No suitable key or wrong key type", "No suitable key was found to decode the JWT"},
		"wrong key, same kid": {stranger.mint(t, map[string]any{"exp": theNow.Add(time.Hour).Unix()}),
			"PGRST301", "JWT cryptographic operation failed", ""},
		"a signature changed":  {valid[:len(valid)-6] + "AAAAAA", "PGRST301", "JWT cryptographic operation failed", ""},
		"expired":              {known.mint(t, map[string]any{"exp": theNow.Add(-time.Hour).Unix()}), "PGRST303", "JWT expired", ""},
		"not yet valid":        {known.mint(t, map[string]any{"nbf": theNow.Add(time.Hour).Unix()}), "PGRST303", "JWT not yet valid", ""},
		"issued in the future": {known.mint(t, map[string]any{"iat": theNow.Add(time.Hour).Unix()}), "PGRST303", "JWT issued at future", ""},
		"exp a string":         {known.mint(t, map[string]any{"exp": "soon"}), "PGRST303", "The JWT 'exp' claim must be a number", ""},
		"nbf a string":         {known.mint(t, map[string]any{"nbf": "soon"}), "PGRST303", "The JWT 'nbf' claim must be a number", ""},
		"iat a string":         {known.mint(t, map[string]any{"iat": "soon"}), "PGRST303", "The JWT 'iat' claim must be a number", ""},
		"aud a number":         {known.mint(t, map[string]any{"aud": 7}), "PGRST303", "The JWT 'aud' claim must be a string or an array of strings", ""},
		"claims not an object": {known.mintRaw(t, `[1,2]`), "PGRST303", "Parsing claims failed", ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode(tc.token, jose.ES256, known.lookup, theNow)
			got := fault(t, err)
			require.Equal(t, tc.code, got.Code)
			require.Equal(t, tc.message, got.Message)
			require.Equal(t, tc.details, got.Details)
			env := got.Envelope()
			require.Equal(t, 401, env.Status)
			require.Equal(t, `Bearer error="invalid_token", error_description="`+tc.message+`"`, env.Challenge)
		})
	}
}

func TestDecode_acceptsWhatPostgRESTAccepts(t *testing.T) {
	t.Parallel()
	known := newSigner(t, "k1")

	// Thirty seconds of skew either way (allowedSkewSeconds).
	for name, claims := range map[string]map[string]any{
		"valid":            {"role": "anon", "exp": theNow.Add(time.Hour).Unix()},
		"just expired":     {"exp": theNow.Add(-29 * time.Second).Unix()},
		"nbf just ahead":   {"nbf": theNow.Add(29 * time.Second).Unix()},
		"iat just ahead":   {"iat": theNow.Add(29 * time.Second).Unix()},
		"no exp at all":    {"role": "anon"},
		"aud a string":     {"aud": "authenticated"},
		"aud a list":       {"aud": []string{"a", "b"}},
		"exp and nbf null": {"exp": nil, "nbf": nil},
	} {
		got, err := Decode(known.mint(t, claims), jose.ES256, known.lookup, theNow)
		require.NoError(t, err, name)
		require.NotNil(t, got, name)
	}
	_, err := Decode(known.mint(t, map[string]any{"exp": theNow.Add(-31 * time.Second).Unix()}), jose.ES256, known.lookup, theNow)
	require.Equal(t, "JWT expired", fault(t, err).Message)
}

func TestDecode_passesALookupFailureOn(t *testing.T) {
	t.Parallel()
	known := newSigner(t, "k1")
	boom := func(string) (jose.JSONWebKey, bool, error) { return jose.JSONWebKey{}, false, errBoom }
	_, err := Decode(known.mint(t, map[string]any{"exp": theNow.Add(time.Hour).Unix()}), jose.ES256, boom, theNow)
	require.ErrorIs(t, err, errBoom)
	_, isFault := err.(*TokenError)
	require.False(t, isFault)
	require.False(t, strings.Contains(err.Error(), "PGRST"))
}

type boomError struct{}

func (boomError) Error() string { return "boom" }

var errBoom error = boomError{}
