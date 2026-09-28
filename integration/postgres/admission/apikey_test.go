package admission_test

import (
	"crypto/subtle"
	"net/http"

	"github.com/prest/prest/v2/admission"
)

// heldUp is the gateway's service listener's question, asked in Go
// (miniship-cloud#801): does apikey hold the install's api secret key. The
// listener compares it in constant time and, on a match, hands the lookup
// route the api service token instead. A call that also carries the header the
// retired service token travelled in is not one rest makes any more, so it is
// refused here rather than accepted beside the key.
func heldUp(header http.Header) bool {
	if header.Get("x-miniship-api-client") != "" {
		return false
	}
	offered := header.Get(admission.APIKeyHeader)
	return subtle.ConstantTimeCompare([]byte(offered), []byte(theAPISecretKey)) == 1
}
