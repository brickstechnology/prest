package pgrwire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// TokenError is a token PostgREST 14 refuses: 401, PGRST301 for a token that
// cannot be decoded or verified and PGRST303 for claims it cannot accept, with
// the message of Error.hs and the WWW-Authenticate header it sends.
type TokenError struct {
	Code    string
	Message string
	Details string
}

func (e *TokenError) Error() string { return e.Message }

// Envelope is the refusal as the answer rest writes.
func (e *TokenError) Envelope() Error {
	out := Error{
		Status:    http.StatusUnauthorized,
		Code:      e.Code,
		Message:   e.Message,
		Challenge: InvalidTokenChallenge(e.Message),
	}
	if e.Details != "" {
		out.Details = e.Details
	}
	return out
}

// InvalidTokenChallenge is invalidTokenHeader (Error.hs): the description is
// the message, quoted as Haskell's show quotes a Text.
func InvalidTokenChallenge(message string) string {
	return `Bearer error="invalid_token", error_description=` + haskellShow(message)
}

func haskellShow(s string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			out.WriteString(`\"`)
		case r == '\\':
			out.WriteString(`\\`)
		case r == '\n':
			out.WriteString(`\n`)
		case r < 0x20 || r > 0x7e:
			fmt.Fprintf(&out, `\%d`, r)
		default:
			out.WriteRune(r)
		}
	}
	out.WriteByte('"')
	return out.String()
}

// The decode faults of JwtDecodeError and the claims faults of JwtClaimsError,
// with the messages of Error.hs (instance ErrorBody JwtError).
func decodeFault(message, details string) *TokenError {
	return &TokenError{Code: "PGRST301", Message: message, Details: details}
}

func claimsFault(message string) *TokenError {
	return &TokenError{Code: "PGRST303", Message: message}
}

// Refused is a token PostgREST would accept that rest does not: a claim rest's
// own policy requires, or a Project the token is not for. PostgREST has no
// message for it, so it answers with the nearest PostgREST fault and says in
// details what rest checked.
func Refused(code, message, details string) *TokenError {
	return &TokenError{Code: code, Message: message, Details: details}
}

// WrongKey is KeyError: "No suitable key or wrong key type", the answer to a
// token the key set cannot have signed.
func WrongKey(details string) *TokenError {
	return decodeFault("No suitable key or wrong key type", details)
}

// BadClaims is ParsingClaimsFailed, the answer to claims rest cannot use.
func BadClaims(details string) *TokenError {
	return &TokenError{Code: "PGRST303", Message: "Parsing claims failed", Details: details}
}

// KeyLookup finds the key of kid. found is false for a key id the set does
// not hold; err is a failure of the lookup itself, which Decode passes on.
type KeyLookup func(kid string) (key jose.JSONWebKey, found bool, err error)

// allowedSkew is allowedSkewSeconds (Auth/Jwt.hs).
const allowedSkew = 30 * time.Second

// Decode is parseToken and the claim checks of Auth/Jwt.hs: the token has three
// parts, a header naming alg, a key the lookup holds and a signature that
// verifies, and claims that are an object whose exp, nbf, iat and aud are of
// the right type and in time. A fault of the token is a *TokenError; a failure
// of lookup is returned as it came.
func Decode(token string, alg jose.SignatureAlgorithm, lookup KeyLookup, now time.Time) (map[string]any, error) {
	if token == "" {
		return nil, decodeFault("Empty JWT is sent in Authorization header", "")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, decodeFault(fmt.Sprintf("Expected 3 parts in JWT; got %d", len(parts)), "")
	}
	undecodable := decodeFault("JWT couldn't be decoded", "")
	var header map[string]any
	for i, part := range parts {
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(part, "="))
		if err != nil {
			return nil, undecodable
		}
		if i == 0 && json.Unmarshal(raw, &header) != nil {
			return nil, undecodable
		}
	}
	if _, encrypted := header["enc"]; encrypted {
		return nil, decodeFault("Unsupported token type", "")
	}
	switch got, _ := header["alg"].(string); got {
	case "":
		return nil, undecodable
	case "none":
		return nil, decodeFault("Wrong or unsupported encoding algorithm", "JWT is unsecured but expected 'alg' was not 'none'")
	case string(alg):
	default:
		return nil, decodeFault("Wrong or unsupported encoding algorithm", "Expected 'alg' doesn't match JWS header")
	}
	signed, err := jose.ParseSigned(token, []jose.SignatureAlgorithm{alg})
	if err != nil || len(signed.Signatures) != 1 {
		return nil, undecodable
	}
	kid, _ := header["kid"].(string)
	key, found, err := lookup(kid)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, WrongKey("No suitable key was found to decode the JWT")
	}
	payload, err := signed.Verify(key.Public())
	if err != nil {
		return nil, decodeFault("JWT cryptographic operation failed", "")
	}
	return checkClaims(payload, now)
}

// checkClaims is decodeClaims and checkForErrors (Auth/Jwt.hs), in PostgREST's
// order: exp, nbf, iat, aud.
func checkClaims(payload []byte, now time.Time) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var claims map[string]any
	if err := decoder.Decode(&claims); err != nil || claims == nil {
		return nil, claimsFault("Parsing claims failed")
	}
	seconds := func(key string) (value int64, present bool, notNumber bool) {
		raw, ok := claims[key]
		if !ok || raw == nil {
			return 0, false, false
		}
		number, isNumber := raw.(json.Number)
		if !isNumber {
			return 0, true, true
		}
		whole, err := number.Int64()
		if err != nil {
			// sciToInt: a number that is not a whole Int is 0.
			return 0, true, false
		}
		return whole, true, false
	}
	at := now.Unix()
	skew := int64(allowedSkew / time.Second)
	type check struct {
		key, notNumber, late string
		violated             func(int64) bool
	}
	for _, c := range []check{
		{"exp", "The JWT 'exp' claim must be a number", "JWT expired", func(v int64) bool { return at-skew > v }},
		{"nbf", "The JWT 'nbf' claim must be a number", "JWT not yet valid", func(v int64) bool { return at+skew < v }},
		{"iat", "The JWT 'iat' claim must be a number", "JWT issued at future", func(v int64) bool { return at+skew < v }},
	} {
		value, present, notNumber := seconds(c.key)
		switch {
		case notNumber:
			return nil, claimsFault(c.notNumber)
		case present && c.violated(value):
			return nil, claimsFault(c.late)
		}
	}
	if aud, ok := claims["aud"]; ok && aud != nil {
		switch typed := aud.(type) {
		case string:
		case []any:
			for _, entry := range typed {
				if _, isString := entry.(string); !isString {
					return nil, claimsFault("The JWT 'aud' claim must be a string or an array of strings")
				}
			}
		default:
			return nil, claimsFault("The JWT 'aud' claim must be a string or an array of strings")
		}
	}
	return claims, nil
}

// NotInAudience is JWTNotInAudience, for a token whose aud is not the one rest
// checks.
func NotInAudience() *TokenError { return claimsFault("JWT not in audience") }
