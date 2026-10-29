// Package pgrwire holds the parts of PostgREST 14's wire behaviour that more
// than one package of rest writes: the error envelope with its headers, the
// token faults a JWT check answers with, and the fuzzy match PostgREST hints
// a missing table with (miniship-cloud#1888, ADR 0075).
//
// Every statement here is read from PostgREST v14.18, and each function names
// the file it follows.
package pgrwire

import (
	"encoding/json"
	"net/http"
)

// JSONContentType is toContentType MTApplicationJSON (MediaType.hs): the mime,
// then "; charset=utf-8".
const JSONContentType = "application/json; charset=utf-8"

// Error is one refusal in PostgREST's shape: the status, and the four fields
// the SDK reads. Details and Hint are nil where PostgREST writes null.
type Error struct {
	Status  int
	Code    string
	Message string
	Details any
	Hint    any
	// Challenge is the WWW-Authenticate value of a 401. An empty one on a 401
	// is PostgREST's plain "Bearer" (Error.hs, PgError headers).
	Challenge string
}

// Body is the JSON PostgREST writes: all four keys, null where absent
// (Error.hs, toJsonPgrstError).
func (e Error) Body() []byte {
	body, _ := json.Marshal(map[string]any{
		"code":    e.Code,
		"message": e.Message,
		"details": e.Details,
		"hint":    e.Hint,
	})
	return body
}

// Write answers e: the JSON body, its Content-Type, the Proxy-Status header
// PostgREST adds to every error it writes (Error.hs, proxyStatusHeader), and
// the WWW-Authenticate header of a 401.
func Write(w http.ResponseWriter, e Error) {
	h := w.Header()
	h.Set("Content-Type", JSONContentType)
	h.Set("Proxy-Status", "PostgREST; error="+e.Code)
	if e.Status == http.StatusUnauthorized {
		challenge := e.Challenge
		if challenge == "" {
			challenge = "Bearer"
		}
		h.Set("WWW-Authenticate", challenge)
	}
	w.WriteHeader(e.Status)
	_, _ = w.Write(e.Body())
}
