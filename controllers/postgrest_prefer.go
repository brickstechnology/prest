package controllers

// miniship (miniship-cloud#1888, ADR 0075): the Prefer header as PostgREST 14
// reads it (ApiRequest/Preferences.hs, fromHeaders), and the Preference-Applied
// header it answers with (prefAppliedHeader and Response.hs, responsePreferences).
//
//   - Every Prefer header is split on commas and each part trimmed. For each
//     preference the first part that is one of its values wins, so
//     `return=foo, return=minimal` is minimal.
//   - A part that is none of the values, nor timezone= nor max-affected=, is an
//     invalid preference. Leniently (the default) it is ignored; with
//     handling=strict it is PGRST122.
//   - Preference-Applied lists, in this order and joined with ", ", the
//     resolution, missing, return, count, tx and handling that were given, then
//     max-affected when handling=strict. A handler echoes only the ones that
//     mean something to it: a read echoes count, tx and handling.
//
// timezone= is not applied here, so it is neither echoed nor invalid.

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// pgrPrefer is the preferences of Prefer a handler acts on.
type pgrPrefer struct {
	// resolution is "merge-duplicates", "ignore-duplicates" or "".
	resolution string
	// ret is "representation", "minimal", "headers-only" or "".
	ret string
	// count is "exact", "planned", "estimated" or "".
	count string
	// tx is "commit", "rollback" or "".
	tx string
	// missing is "default", "null" or "".
	missing string
	// handling is "strict", "lenient" or "".
	handling       string
	maxAffectedSet bool
	maxAffected    int64
	// invalid are the parts of Prefer that are no preference PostgREST knows.
	invalid []string
	// singleObject is `params=single-object`, which PostgREST 12 read and
	// PostgREST 14 does not: a function call still reads it
	// (miniship-cloud#1304), and it is an invalid preference under strict.
	singleObject bool
}

var (
	pgrResolutions = []string{"merge-duplicates", "ignore-duplicates"}
	pgrReturns     = []string{"representation", "minimal", "headers-only"}
	pgrCounts      = []string{"exact", "planned", "estimated"}
	pgrTxs         = []string{"commit", "rollback"}
	pgrMissings    = []string{"default", "null"}
	pgrHandlings   = []string{"strict", "lenient"}
)

// pgrParsePrefer reads every Prefer header.
func pgrParsePrefer(values []string) pgrPrefer {
	var parts []string
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			parts = append(parts, strings.TrimSpace(part))
		}
	}
	first := func(key string, options []string) string {
		for _, part := range parts {
			name, setting, _ := strings.Cut(part, "=")
			if name != key {
				continue
			}
			for _, option := range options {
				if setting == option {
					return option
				}
			}
		}
		return ""
	}
	prefer := pgrPrefer{
		resolution: first("resolution", pgrResolutions),
		ret:        first("return", pgrReturns),
		count:      first("count", pgrCounts),
		tx:         first("tx", pgrTxs),
		missing:    first("missing", pgrMissings),
		handling:   first("handling", pgrHandlings),
	}
	for _, part := range parts {
		if setting, ok := strings.CutPrefix(part, "max-affected="); ok {
			// The first max-affected= decides, and it counts only when it is
			// a whole number (readMaybe).
			if n, err := strconv.ParseInt(setting, 10, 64); err == nil {
				prefer.maxAffectedSet, prefer.maxAffected = true, n
			}
			break
		}
	}
	for _, part := range parts {
		name, setting, _ := strings.Cut(part, "=")
		if name == "params" && setting == "single-object" {
			prefer.singleObject = true
		}
		if !pgrKnownPrefer(name, setting, part) {
			prefer.invalid = append(prefer.invalid, part)
		}
	}
	return prefer
}

// pgrKnownPrefer is not isUnacceptable (Preferences.hs): a part that is one of
// the values of a preference, or timezone= or max-affected= with anything.
func pgrKnownPrefer(name, setting, part string) bool {
	if strings.HasPrefix(part, "timezone=") || strings.HasPrefix(part, "max-affected=") {
		return true
	}
	for key, options := range map[string][]string{
		"resolution": pgrResolutions, "return": pgrReturns, "count": pgrCounts,
		"tx": pgrTxs, "missing": pgrMissings, "handling": pgrHandlings,
	} {
		if name != key {
			continue
		}
		for _, option := range options {
			if setting == option {
				return true
			}
		}
	}
	return false
}

func (p pgrPrefer) representation() bool { return p.ret == "representation" }
func (p pgrPrefer) headersOnly() bool    { return p.ret == "headers-only" }
func (p pgrPrefer) counted() bool        { return p.count != "" }
func (p pgrPrefer) rollback() bool       { return p.tx == "rollback" }
func (p pgrPrefer) strict() bool         { return p.handling == "strict" }
func (p pgrPrefer) missingDefault() bool { return p.missing == "default" }

// strictFault is PGRST122: invalid preferences given with handling=strict
// (Error.hs, InvalidPreferences).
func (p pgrPrefer) strictFault() error {
	if !p.strict() || len(p.invalid) == 0 {
		return nil
	}
	return &pgrError{
		status:  http.StatusBadRequest,
		code:    "PGRST122",
		message: "Invalid preferences given with handling=strict",
		details: "Invalid preferences: " + strings.Join(p.invalid, ", "),
	}
}

// exceeds is whether affected rows break a max-affected the caller asked to
// have enforced: only with handling=strict (MainTx.hs, failExceedsMaxAffectedPref).
func (p pgrPrefer) exceeds(affected int64) bool {
	return p.strict() && p.maxAffectedSet && affected > p.maxAffected
}

// pgrMaxAffectedFault is PGRST124 (Error.hs, MaxAffectedViolationError).
func pgrMaxAffectedFault(affected int64) error {
	return &pgrError{status: http.StatusBadRequest, code: "PGRST124",
		message: "Query result exceeds max-affected preference constraint",
		details: fmt.Sprintf("The query affects %d rows", affected)}
}

// pgrApplied says which of the preferences a response echoes
// (Response.hs, responsePreferences).
type pgrApplied struct {
	resolution, missing, representation, maxAffected bool
}

// applied is the Preference-Applied value, or "" for no header.
func (p pgrPrefer) applied(which pgrApplied) string {
	var out []string
	if which.resolution && p.resolution != "" {
		out = append(out, "resolution="+p.resolution)
	}
	if which.missing && p.missing != "" {
		out = append(out, "missing="+p.missing)
	}
	if which.representation && p.ret != "" {
		out = append(out, "return="+p.ret)
	}
	if p.count != "" {
		out = append(out, "count="+p.count)
	}
	if p.tx != "" {
		out = append(out, "tx="+p.tx)
	}
	if p.handling != "" {
		out = append(out, "handling="+p.handling)
	}
	if which.maxAffected && p.strict() && p.maxAffectedSet {
		out = append(out, "max-affected="+strconv.FormatInt(p.maxAffected, 10))
	}
	return strings.Join(out, ", ")
}

// setApplied writes Preference-Applied when anything was applied.
func (p pgrPrefer) setApplied(w http.ResponseWriter, which pgrApplied) {
	if value := p.applied(which); value != "" {
		w.Header().Set("Preference-Applied", value)
	}
}
