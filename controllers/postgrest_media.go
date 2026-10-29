package controllers

// miniship (miniship-cloud#1886, ADR 0074): the response media type of
// /rest/v1, negotiated as PostgREST 14 does for what supabase-js 2.118 sends.
//
//	.csv()        Accept: text/csv
//	.geojson()    Accept: application/geo+json
//	.stripNulls() Accept: application/vnd.pgrst.array+json;nulls=stripped
//	.single()     Accept: application/vnd.pgrst.object+json
//
// Anything else rest cannot produce, the plan types of .explain() included
// (the owner's ruling of 29 October 2026: a default Supabase Project has plans
// disabled), is 406 PGRST107.
//
// Every line below follows PostgREST v14.0: MediaType.hs (decodeMediaType,
// toMime, toContentType), Plan.hs (negotiateContent), Error.hs (MediaTypeError)
// and Query/SqlFragment.hs (asCsvF, asGeoJsonF, asJsonF, addNullsToSnip).

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// pgrMediaKind is how a response is aggregated.
type pgrMediaKind int

const (
	// pgrMediaJSON is a JSON array of rows (application/json, */*, and
	// application/vnd.pgrst.array+json without a null strip).
	pgrMediaJSON pgrMediaKind = iota
	// pgrMediaObject is the one row as an object (vnd.pgrst.object+json).
	pgrMediaObject
	// pgrMediaCSV is text/csv.
	pgrMediaCSV
	// pgrMediaGeoJSON is application/geo+json.
	pgrMediaGeoJSON
)

// pgrMedia is the media type a request is answered in.
type pgrMedia struct {
	kind pgrMediaKind
	// strip is nulls=stripped.
	strip bool
}

// contentType is toContentType: the mime, then "; charset=utf-8" for every
// type but octet-stream and the ones PostgREST does not know.
func (m pgrMedia) contentType() string {
	switch m.kind {
	case pgrMediaObject:
		if m.strip {
			return "application/vnd.pgrst.object+json;nulls=stripped; charset=utf-8"
		}
		return "application/vnd.pgrst.object+json; charset=utf-8"
	case pgrMediaCSV:
		return "text/csv; charset=utf-8"
	case pgrMediaGeoJSON:
		return "application/geo+json; charset=utf-8"
	}
	if m.strip {
		return "application/vnd.pgrst.array+json;nulls=stripped; charset=utf-8"
	}
	return pgrJSONMediaType
}

// isJSON is whether the body is a JSON array of rows rest can read back into
// rows (the kinds that count rows from the body).
func (m pgrMedia) isJSON() bool {
	return m.kind == pgrMediaJSON || m.kind == pgrMediaObject
}

// object is whether the request asked for one row as an object.
func (m pgrMedia) object() bool { return m.kind == pgrMediaObject }

// pgrAcceptEntry is one decoded Accept entry: what PostgREST makes of it and
// how toMime writes it back.
type pgrAcceptEntry struct {
	// media is the producible media type; ok is false for one rest cannot
	// produce.
	media pgrMedia
	ok    bool
	// mime is toMime of the decoded entry, which is what a 406 lists.
	mime string
}

// pgrNegotiate is negotiateContent for a table, a write or a call: the first
// Accept entry (in quality order) rest can produce, or 406 PGRST107 listing
// every entry as PostgREST writes it. No Accept is */*.
func pgrNegotiate(accept string) (pgrMedia, error) {
	return pgrNegotiateWithout(accept, -1)
}

// pgrNegotiateWithout is pgrNegotiate with one kind taken from what can be
// produced; -1 takes none. A missing Accept is */*.
func pgrNegotiateWithout(accept string, without pgrMediaKind) (pgrMedia, error) {
	entries := []pgrAcceptEntry{{media: pgrMedia{kind: pgrMediaJSON}, ok: true, mime: "*/*"}}
	if accept != "" {
		entries = pgrParseAccept(accept)
	}
	for _, entry := range entries {
		if entry.ok && entry.media.kind != without {
			return entry.media, nil
		}
	}
	listed := make([]string, 0, len(entries))
	for _, entry := range entries {
		listed = append(listed, entry.mime)
	}
	return pgrMedia{}, &pgrError{
		status:  http.StatusNotAcceptable,
		code:    "PGRST107",
		message: "None of these media types are available: " + strings.Join(listed, ", "),
	}
}

// pgrParseAccept is parseHttpAccept (Network.Wai.Parse) then decodeMediaType
// on each: spaces are dropped, the quality is read from ";q=", and the entries
// are sorted by quality, then by more semicolons less stars, the sort stable.
func pgrParseAccept(header string) []pgrAcceptEntry {
	type ranked struct {
		text        string
		q           float64
		specificity int
	}
	var list []ranked
	for _, part := range strings.Split(header, ",") {
		part = strings.ReplaceAll(part, " ", "")
		text, q := part, 1.0
		if i := strings.Index(part, ";q="); i >= 0 {
			text = part[:i]
			rest := part[i+3:]
			if j := strings.IndexByte(rest, ';'); j >= 0 {
				rest = rest[:j]
			}
			q = pgrReadQuality(rest)
		}
		list = append(list, ranked{text: text, q: q, specificity: strings.Count(text, ";") - strings.Count(text, "*")})
	}
	sort.SliceStable(list, func(a, b int) bool {
		if list[a].q != list[b].q {
			return list[a].q > list[b].q
		}
		return list[a].specificity > list[b].specificity
	})
	entries := make([]pgrAcceptEntry, 0, len(list))
	for _, item := range list {
		entries = append(entries, pgrDecodeMediaType(item.text))
	}
	return entries
}

// pgrReadQuality is Haskell's `reads` for a Double, 1.0 when it reads none.
func pgrReadQuality(s string) float64 {
	end := 0
	for end < len(s) && (s[end] >= '0' && s[end] <= '9' || s[end] == '.') {
		end++
	}
	// `reads` takes digits, then an optional fraction that has digits after
	// the point.
	if dot := strings.IndexByte(s[:end], '.'); dot >= 0 && dot == end-1 {
		end = dot
	}
	if end == 0 || s[0] == '.' {
		return 1.0
	}
	q, err := strconv.ParseFloat(s[:end], 64)
	if err != nil {
		return 1.0
	}
	return q
}

// pgrDecodeMediaType is decodeMediaType with the mime toMime writes for it.
func pgrDecodeMediaType(text string) pgrAcceptEntry {
	main, sub, params, parsed := pgrTokenizeMediaType(text)
	if !parsed {
		main, sub, params = text, "", nil
	}
	lowerMain, lowerSub := strings.ToLower(main), strings.ToLower(sub)
	param := func(name string) (string, bool) {
		v, ok := params[name]
		return v, ok
	}
	stripped := func() bool {
		v, _ := param("nulls")
		return v == "stripped"
	}
	switch {
	case lowerMain == "application" && lowerSub == "json":
		return pgrAcceptEntry{media: pgrMedia{kind: pgrMediaJSON}, ok: true, mime: "application/json"}
	case lowerMain == "application" && lowerSub == "geo+json":
		return pgrAcceptEntry{media: pgrMedia{kind: pgrMediaGeoJSON}, ok: true, mime: "application/geo+json"}
	case lowerMain == "text" && lowerSub == "csv":
		return pgrAcceptEntry{media: pgrMedia{kind: pgrMediaCSV}, ok: true, mime: "text/csv"}
	case lowerMain == "text" && lowerSub == "plain":
		return pgrAcceptEntry{mime: "text/plain"}
	case lowerMain == "text" && lowerSub == "xml":
		return pgrAcceptEntry{mime: "text/xml"}
	case lowerMain == "application" && lowerSub == "openapi+json":
		return pgrAcceptEntry{mime: "application/openapi+json"}
	case lowerMain == "application" && lowerSub == "x-www-form-urlencoded":
		return pgrAcceptEntry{mime: "application/x-www-form-urlencoded"}
	case lowerMain == "application" && lowerSub == "octet-stream":
		return pgrAcceptEntry{mime: "application/octet-stream"}
	case lowerMain == "application" && (lowerSub == "vnd.pgrst.plan" || lowerSub == "vnd.pgrst.plan+text"):
		return pgrPlanEntry(params, "text")
	case lowerMain == "application" && lowerSub == "vnd.pgrst.plan+json":
		return pgrPlanEntry(params, "json")
	case lowerMain == "application" && (lowerSub == "vnd.pgrst.object+json" || lowerSub == "vnd.pgrst.object"):
		strip := stripped()
		mime := "application/vnd.pgrst.object+json"
		if strip {
			mime += ";nulls=stripped"
		}
		return pgrAcceptEntry{media: pgrMedia{kind: pgrMediaObject, strip: strip}, ok: true, mime: mime}
	case lowerMain == "application" && (lowerSub == "vnd.pgrst.array+json" || lowerSub == "vnd.pgrst.array"):
		if stripped() {
			return pgrAcceptEntry{media: pgrMedia{kind: pgrMediaJSON, strip: true}, ok: true,
				mime: "application/vnd.pgrst.array+json;nulls=stripped"}
		}
		return pgrAcceptEntry{media: pgrMedia{kind: pgrMediaJSON}, ok: true, mime: "application/json"}
	case lowerMain == "*" && lowerSub == "*":
		return pgrAcceptEntry{media: pgrMedia{kind: pgrMediaJSON}, ok: true, mime: "*/*"}
	}
	return pgrAcceptEntry{mime: text}
}

// pgrPlanEntry is a vnd.pgrst.plan entry. Plans are disabled on a Project
// (db-plan-enabled false, a default Supabase Project's), so none is producible,
// and the entry is written back as toMime does: the format, `for` as the
// decoded media type's mime, and the options PostgREST knows in its own order.
func pgrPlanEntry(params map[string]string, format string) pgrAcceptEntry {
	target := "application/json"
	if v, ok := params["for"]; ok {
		target = pgrDecodeMediaType(v).mime
	}
	mime := "application/vnd.pgrst.plan+" + format + "; for=\"" + target + "\""
	have := map[string]bool{}
	for _, option := range strings.Split(params["options"], "|") {
		have[option] = true
	}
	var chosen []string
	for _, option := range []string{"analyze", "verbose", "settings", "buffers", "wal"} {
		if have[option] {
			chosen = append(chosen, option)
		}
	}
	if len(chosen) > 0 {
		mime += "; options=" + strings.Join(chosen, "|")
	}
	return pgrAcceptEntry{mime: mime}
}

// pgrTokenizeMediaType is tokenizeMediaType (MediaType.hs): main/sub, then
// `;key=value` pairs, the value quoted or made of letters, digits, `|` and
// `-`. Parsec stops at the first thing it cannot read and ignores the rest.
func pgrTokenizeMediaType(s string) (main, sub string, params map[string]string, ok bool) {
	runes := []rune(s)
	i := 0
	take := func(allowed func(rune) bool) string {
		start := i
		for i < len(runes) && allowed(runes[i]) {
			i++
		}
		return string(runes[start:i])
	}
	alnum := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
	main = take(func(r rune) bool { return alnum(r) || r == '.' || r == '*' })
	if main == "" || i >= len(runes) || runes[i] != '/' {
		return "", "", nil, false
	}
	i++
	sub = take(func(r rune) bool { return alnum(r) || strings.ContainsRune(".*+-", r) })
	if sub == "" {
		return "", "", nil, false
	}
	params = map[string]string{}
	for {
		save := i
		if i >= len(runes) || runes[i] != ';' {
			break
		}
		i++
		key := take(func(r rune) bool { return alnum(r) || r == '-' })
		if key == "" || i >= len(runes) || runes[i] != '=' {
			i = save
			break
		}
		i++
		var value string
		if i < len(runes) && runes[i] == '"' {
			end := -1
			for j := i + 1; j < len(runes); j++ {
				if runes[j] == '"' {
					end = j
					break
				}
			}
			if end < 0 {
				i = save
				break
			}
			value = string(runes[i+1 : end])
			i = end + 1
		} else {
			value = take(func(r rune) bool { return alnum(r) || r == '|' || r == '-' })
			if value == "" {
				i = save
				break
			}
		}
		// Parameter names are lower-cased, and the last of a name wins, as
		// Data.Map.fromList keeps it.
		params[strings.ToLower(key)] = value
	}
	return main, sub, params, true
}

// pgrRowAlias is the name of the row alias the aggregates read.
const pgrRowAlias = `"pgrst_t"`

// aggregate is the one statement that reads the rows of source (a SELECT) as
// the body this media type answers. A scalar function's rows are read from
// its pgrst_scalar column for JSON, as asJsonF does. The statement is one
// column of text, usable as a sub-select of a larger one.
func (m pgrMedia) aggregate(source, alias string, scalar bool) string {
	return m.aggregateWith(source, alias, scalar, "")
}

// counted is aggregate with a second column, how many rows the body holds, for
// the kinds whose body cannot be read back into rows (CSV and GeoJSON).
func (m pgrMedia) counted(source string) string {
	return m.aggregateWith(source, pgrRowAlias, false, ", count(*)")
}

func (m pgrMedia) aggregateWith(source, alias string, scalar bool, extra string) string {
	switch m.kind {
	case pgrMediaCSV:
		// asCsvF: the header is the first row's column names, the body each
		// row as the Database writes the record, without its parentheses.
		// PostgREST joins with a newline and ends no line, so a read with no
		// rows is the one newline.
		return fmt.Sprintf(`WITH "pgrst_s" AS (%[1]s) SELECT (SELECT coalesce(string_agg(a.k, ','), '') FROM (SELECT json_object_keys(r)::text AS k FROM (SELECT row_to_json(hh) AS r FROM "pgrst_s" AS hh LIMIT 1) s) a) || E'\n' || coalesce(string_agg(substring(%[2]s::text, 2, length(%[2]s::text) - 2), E'\n'), '')%[3]s FROM "pgrst_s" AS %[2]s`,
			source, pgrRowAlias, extra)
	case pgrMediaGeoJSON:
		return fmt.Sprintf(`SELECT json_build_object('type', 'FeatureCollection', 'features', coalesce(json_agg(ST_AsGeoJSON(%[2]s)::json), '[]'))::text%[3]s FROM (%[1]s) AS %[2]s`,
			source, pgrRowAlias, extra)
	}
	value := alias
	if scalar {
		value = alias + `."pgrst_scalar"`
	}
	rows := fmt.Sprintf(`coalesce(json_agg(%s), '[]'::json)`, value)
	if m.strip {
		rows = fmt.Sprintf(`coalesce(json_strip_nulls(json_agg(%s)), '[]'::json)`, value)
	}
	return fmt.Sprintf(`SELECT %s::text%s FROM (%s) AS %s`, rows, extra, source, alias)
}

// empty is the body of a representation of no rows, for the kinds whose body
// is not an array.
func (m pgrMedia) empty() string {
	if m.kind == pgrMediaGeoJSON {
		return `{"type" : "FeatureCollection", "features" : []}`
	}
	return "\n"
}

// hasGeoJSON is the statement that tells whether ST_AsGeoJSON takes a row,
// which is what PostGIS installed and on the search path means.
const hasGeoJSON = `SELECT to_regprocedure('st_asgeojson(record)') IS NOT NULL`

// pgrResolveGeoJSON keeps a geo+json answer only where PostGIS is installed.
// Where it is not, the type is one this Database cannot produce, so the next
// Accept entry rest can produce answers, and 406 PGRST107 when there is none.
func pgrResolveGeoJSON(ctx context.Context, tx *sql.Tx, accept string, media pgrMedia) (pgrMedia, error) {
	if media.kind != pgrMediaGeoJSON {
		return media, nil
	}
	var installed bool
	if err := tx.QueryRowContext(ctx, hasGeoJSON).Scan(&installed); err != nil {
		return media, err
	}
	if installed {
		return media, nil
	}
	return pgrNegotiateWithout(accept, pgrMediaGeoJSON)
}
