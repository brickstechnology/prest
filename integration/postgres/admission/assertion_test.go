package admission_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// heldUp is checkAssertion, asked in Go: the answerer here verifies rest's
// service token the way the api's own route does, so the credential half of
// this package is exercised rather than assumed.
//
// The MAC is checked before anything is parsed, and alg and typ are compared
// to constants rather than read out of the token (RFC 8725 §3.1). The public
// repository's packages/api-client/src/assertion.ts is the original.
func heldUp(offered string) bool {
	parts := strings.Split(offered, ".")
	if len(parts) != 3 {
		return false
	}
	key, err := base64.RawURLEncoding.DecodeString(theLookupKey)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal([]byte(base64.RawURLEncoding.EncodeToString(mac.Sum(nil))), []byte(parts[2])) {
		return false
	}

	var head struct{ Alg, Typ string }
	var body struct {
		Sub, Aud string
		Exp      int64
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(raw, &head) != nil {
		return false
	}
	raw, err = base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(raw, &body) != nil {
		return false
	}
	switch {
	case head.Alg != "HS256", head.Typ != "miniship-link+jwt":
		return false
	case body.Aud != "database", body.Sub != "rest":
		return false
	case time.Now().Unix() > body.Exp+30:
		return false
	}
	return true
}
