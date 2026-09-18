package admission

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// THE CREDENTIAL REST PRESENTS, WHICH IS NOT A SECRET ON THE WIRE.
//
// miniship's internal addresses are not opened by sending a key. The caller
// mints a short-lived service token with the route's key and the route
// verifies it with the same key, so anything that reads one call gets five
// minutes at one route and then nothing. The whole reasoning is in the public
// repository's packages/api-client/src/assertion.ts, and this file is the Go
// half of the same thing: rest is the first service outside that package's
// language that has to mint one.
//
// What is copied, and what each is for:
//
//   - HS256, pinned. The verifier compares alg to a constant and never reads
//     it out of the token (RFC 8725 §3.1), so a token minted with anything
//     else is refused rather than trusted.
//   - typ "miniship-link+jwt", so one of these cannot be mistaken for another
//     kind of JWT (RFC 8725 §3.11).
//   - aud "rest", the route. The keys already differ per route; the audience
//     means a captured token cannot be re-aimed at another one either.
//   - exp five minutes, no nbf. These are used the moment they are made.
//   - jti, minted and not enforced, so a log can tell two identical calls
//     apart.
//
// The key rest is given is the route's derived key and never the install's
// root: the api derives it by HKDF-SHA256 and hands rest that one, as the edge
// is handed the three it needs. rest cannot compute the root from it and
// therefore cannot compute any other route's key.
const (
	// assertionAlg is the one algorithm, written here so the verifier's
	// constant has a counterpart.
	assertionAlg = "HS256"
	// assertionTyp keeps a service token from being read as another JWT.
	assertionTyp = "miniship-link+jwt"
	// assertionAudience is the route this token opens, and the only one.
	assertionAudience = "rest"
	// assertionIssuer is who is calling. rest is a service, not a person.
	assertionIssuer = "rest"
	// assertionLifetime is five minutes: long enough for a slow call, short
	// enough to be worth little.
	assertionLifetime = 5 * time.Minute
)

// assertionHeader and assertionClaims are marshalled rather than written by
// hand, and the verifier re-signs the bytes it received, so the field order
// here need not match the minting side in the other language — only the
// values do.
type assertionHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

type assertionClaims struct {
	Sub string `json:"sub"`
	Aud string `json:"aud"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Jti string `json:"jti"`
}

// mintAssertion makes one service token, for the lookup route, good for five
// minutes. key is the route's derived key as the api answered it: base64url,
// which is what it is carried as everywhere else.
func mintAssertion(key string, now time.Time) (string, error) {
	secret, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil {
		return "", fmt.Errorf("the lookup key is not base64url: %w", err)
	}
	if len(secret) == 0 {
		return "", fmt.Errorf("rest was given no key for the lookup address")
	}

	issued := now.Unix()
	jti := make([]byte, 12)
	if _, err := rand.Read(jti); err != nil {
		return "", fmt.Errorf("could not mint a service token: %w", err)
	}

	header, err := json.Marshal(assertionHeader{Alg: assertionAlg, Typ: assertionTyp})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(assertionClaims{
		Sub: assertionIssuer,
		Aud: assertionAudience,
		Iat: issued,
		Exp: issued + int64(assertionLifetime.Seconds()),
		Jti: base64.RawURLEncoding.EncodeToString(jti),
	})
	if err != nil {
		return "", err
	}

	signingInput := base64.RawURLEncoding.EncodeToString(header) +
		"." + base64.RawURLEncoding.EncodeToString(claims)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
