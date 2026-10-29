package middlewares

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"

	"github.com/prest/prest/v2/middlewares/statements"

	"github.com/clbanning/mxj/j2x"
)

var (
	ErrXMLBadRequest = `<?xml version="1.0" encoding="utf-8"?>
<errors xmlns="http://schemas.google.com/g/2005">
  <error>
    <reason>internal</reason>
    <internalReason>%s</internalReason>
  </error>
  <code>400</code> 
</errors>`
)

// getVars extracts {database}/{schema}/{table} from a request path. A real
// http.Request.URL.Path always starts with "/", so a 3-segment resource route
// splits into 4 elements (leading "") and a "/batch/..." route (one extra
// literal segment) splits into 5. Both prefixes are stripped explicitly here
// rather than inferred from segment count alone — that used to make any
// 4-element split fall through to "not a table path" (nil, which
// AccessControl treats as unenforced), silently skipping TablePermissions on
// /batch/{database}/{schema}/{table}. /show/{database}/{schema}/{table} is
// stripped the same way so describing a table honours its read permission.
func getVars(path string) (paths map[string]string) {
	segments := strings.Split(path, "/")
	if len(segments) > 0 && segments[0] == "" {
		segments = segments[1:]
	}
	if len(segments) == 4 && (segments[0] == "batch" || segments[0] == "show") {
		segments = segments[1:]
	}
	if len(segments) != 3 {
		return nil
	}
	// miniship (#1302): /rest/v1/{table} is the Supabase SDK's address, not a
	// table of a Database called rest in a schema called v1. What a caller may
	// read there is the Database's own grants and row security, as the role
	// the read becomes (patch 5). pREST's list is not consulted on it, and so
	// neither are its reserved control-plane names (GHSA-2x9q-r7j9-g85f): a
	// Project's own table may be called prest_users, and rest keeps no
	// control-plane table in a Project's Database to protect.
	if segments[0] == "rest" && segments[1] == "v1" {
		return nil
	}
	return map[string]string{
		"database": segments[0],
		"schema":   segments[1],
		"table":    segments[2],
	}
}

func permissionByMethod(method string) (permission string) {
	switch method {
	case "GET":
		permission = statements.READ
	case "POST", "PATCH", "PUT":
		permission = statements.WRITE
	case "DELETE":
		permission = statements.DELETE
	default:
		permission = ""
	}

	return
}

func renderFormat(w http.ResponseWriter, recorder *httptest.ResponseRecorder, format string) {
	for key := range recorder.Header() {
		w.Header().Set(key, recorder.Header().Get(key))
	}
	byt, _ := io.ReadAll(recorder.Body)
	if recorder.Code >= 400 {
		trimmed := strings.TrimSpace(string(byt))
		// Pass through bodies that are already JSON (e.g. MCP JSON-RPC errors,
		// controller {"error":"..."} payloads) to avoid double-wrapping.
		if json.Valid([]byte(trimmed)) {
			byt = []byte(trimmed)
		} else {
			m := map[string]string{"error": trimmed}
			byt, _ = json.MarshalIndent(m, "", "\t")
		}
	}
	switch format {
	case "xml":
		xmldata, err := j2x.JsonToXml(byt)
		if err != nil {
			http.Error(w, fmt.Sprintf(ErrXMLBadRequest, err.Error()), http.StatusBadRequest)
			return
		}
		xmlStr := fmt.Sprintf("<objects>%s</objects>", string(xmldata))
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(recorder.Code)
		w.Write([]byte(xmlStr))
	default:
		// miniship (miniship-cloud#1886): the media types /rest/v1 answers
		// besides JSON keep the Content-Type the handler chose.
		if !pgrMediaContentType(recorder.Header().Get("Content-Type")) {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(recorder.Code)
		w.Write(byt)
	}
}

var defaultAllowMethods = []string{
	"GET",
	"POST",
	"PUT",
	"PATCH",
	"DELETE",
	"OPTIONS",
}

const (
	headerAllowOrigin      = "Access-Control-Allow-Origin"
	headerAllowCredentials = "Access-Control-Allow-Credentials"
	headerAllowHeaders     = "Access-Control-Allow-Headers"
	headerAllowMethods     = "Access-Control-Allow-Methods"
	headerOrigin           = "Origin"
)

func checkCors(r *http.Request, origin []string) (allowed bool) {
	var mAllowed bool
	for _, m := range defaultAllowMethods {
		if m == r.Method {
			mAllowed = true
			break
		}
	}
	if !mAllowed {
		return
	}
	org := r.Header.Get(headerOrigin)
	var oAllowed bool
	for _, o := range origin {
		if o == org || o == "*" || org == "*" {
			oAllowed = true
			break
		}
	}
	if oAllowed && mAllowed {
		allowed = true
	}
	return
}

// MatchURL matches the given url with a whitelist.
func MatchURL(url string, whitelist []string) (match bool, err error) {
	for _, exp := range whitelist {
		match, err = regexp.Match(exp, []byte(url))
		if match || err != nil {
			return
		}
	}
	return
}

// pgrMediaContentType is whether a handler answered in one of the media types
// of PostgREST that are not application/json: CSV, GeoJSON, and the vendored
// object and array types, whose Content-Type names the null strip.
func pgrMediaContentType(contentType string) bool {
	// miniship (miniship-cloud#1888): application/json; charset=utf-8 is
	// PostgREST's own Content-Type for JSON (toContentType, MediaType.hs). A
	// handler that wrote it chose it; every other JSON answer is plain
	// application/json, as it always was.
	if contentType == "application/json; charset=utf-8" {
		return true
	}
	for _, prefix := range []string{"text/csv", "application/geo+json", "application/vnd.pgrst."} {
		if strings.HasPrefix(contentType, prefix) {
			return true
		}
	}
	return false
}
