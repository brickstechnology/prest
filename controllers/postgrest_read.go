package controllers

// miniship (miniship-cloud#1302, public ADR 0077): GET and HEAD on
// /rest/v1/{table}, the address the Supabase SDK's database client reads.
//
// The path names no Project, as the SDK's never does. The Project is the one
// the address the call arrived at belongs to: on miniship cloud the gateway's
// Check names it in a header rest is told to read, and on a self-host install
// it is the install's one Project, which rest asks the lookup for once.
//
// The caller is who the token says: the install's anon token, which the
// gateway puts in place of the api publishable key, becomes anon; the api
// service token becomes service_role; a User's own token is verified against
// the key set of the Project the call is for and becomes authenticated. The
// token's claims go to the Database as request.jwt.claims, so a row rule reads
// the User's id. Nothing in rest decides which rows arrive.
//
// The bounds are the table read's: a page of at most MaxPageSize rows, the
// statement time limit, and the longest query string. Over any of them is a
// refusal in PostgREST's shape, which the SDK shows as an error with a
// message, a code, details and a hint.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/lib/pq"
	"github.com/structy/log"

	"github.com/prest/prest/v2/adapters"
	"github.com/prest/prest/v2/admission"
	pctx "github.com/prest/prest/v2/context"
	"github.com/prest/prest/v2/internal/ident"
	"github.com/prest/prest/v2/internal/logsafe"
	"github.com/prest/prest/v2/internal/pgrwire"
	"github.com/prest/prest/v2/projectkeys"
)

// UserTokenVerifier checks a User's token against one Project's key set.
// projectkeys.Source is the one there is.
type UserTokenVerifier interface {
	Verify(ctx context.Context, project, token string) (role string, claims []byte, err error)
}

// OnlyProjectAdmitter finds the install's one Project, for an address that
// names none. admission.Gate is the one there is; it is asked by type so the
// Admitter interface, and every stand-in of it, are unchanged.
type OnlyProjectAdmitter interface {
	AdmitOnly(ctx context.Context) (adapters.Adapter, string, error)
}

// The Accept and Prefer values the read answers.
const (
	pgrObjectMediaType = "application/vnd.pgrst.object+json"
	pgrJSONMediaType   = "application/json; charset=utf-8"
)

// PostgRESTRead serves GET and HEAD on /rest/v1/{table}.
func (h *CRUDHandler) PostgRESTRead(w http.ResponseWriter, r *http.Request) {
	table := pathVars(r)["table"]
	if err := pgrCheckName(table, "table"); err != nil {
		pgrWriteError(w, err)
		return
	}
	if len(r.URL.RawQuery) > h.bounds.MaxQueryLen {
		pgrWriteError(w, &pgrError{
			status:  http.StatusRequestURITooLong,
			code:    "MS101",
			message: queryTooLong,
			details: fmt.Sprintf("rest reads at most %d bytes of query string", h.bounds.MaxQueryLen),
			hint:    "send fewer filters, or a shorter in list",
		})
		return
	}
	if profile := r.Header.Get("Accept-Profile"); profile != "" && profile != pgrSchema {
		pgrWriteError(w, &pgrError{
			status:  http.StatusNotAcceptable,
			code:    "PGRST106",
			message: "The schema must be one of the following: " + pgrSchema,
		})
		return
	}
	read, err := pgrParseRead(r.URL.Query(), h.bounds.MaxPageSize)
	if err != nil {
		pgrWriteError(w, err)
		return
	}

	project, adapter, err := h.resolveProject(r)
	if err != nil {
		pgrWriteError(w, err)
		return
	}
	caller, err := h.resolveCaller(r, project, adapter)
	if err != nil {
		pgrWriteError(w, err)
		return
	}

	// PostgREST finds the table, plans the read, and only then negotiates the
	// media type and checks the preferences, so a refusal of either waits for
	// the table (PGRST205) and the plan (Plan.hs, wrappedReadPlan). It is
	// known now, because it needs no Database, and raised in the transaction.
	media, early := pgrNegotiate(r.Header.Get("Accept"))
	prefer := pgrParsePrefer(r.Header.Values("Prefer"))
	if early == nil {
		early = prefer.strictFault()
	}

	ctx, cancel := requestContext(r, project)
	defer cancel()

	counted := prefer.counted()
	var rows []json.RawMessage
	var text string
	var pageRows int64
	var total int64
	run := func(reader adapters.CallerReader) error {
		return reader.ReadAsCallerCtx(ctx, caller, func(tx *sql.Tx) error {
			// A retry builds the read again, so the arguments are the first
			// attempt's only once.
			again := *read
			again.where, again.args = append([]string(nil), read.where...), append([]any(nil), read.args...)
			if early != nil || read.hasEmbeds() {
				if missing := pgrMissingTable(ctx, tx, table); missing != nil {
					return missing
				}
			}
			if err := again.plan(ctx, tx, table); err != nil {
				return err
			}
			if early != nil {
				return early
			}
			if media.isJSON() {
				rowsSQL, err := again.rowsSQLAs(table, media)
				if err != nil {
					return err
				}
				var out string
				if err := tx.QueryRowContext(ctx, rowsSQL, again.args...).Scan(&out); err != nil {
					return err
				}
				if err := json.Unmarshal([]byte(out), &rows); err != nil {
					return err
				}
			} else {
				page, err := again.pageSQL(table)
				if err != nil {
					return err
				}
				if err := tx.QueryRowContext(ctx, media.counted(page), again.args...).Scan(&text, &pageRows); err != nil {
					return err
				}
			}
			if counted {
				return tx.QueryRowContext(ctx, again.countSQL(table), again.countArgs()...).Scan(&total)
			}
			return nil
		})
	}

	reader, ok := adapter.(adapters.CallerReader)
	if !ok {
		pgrWriteError(w, &pgrError{status: http.StatusInternalServerError, code: "PGRST000", message: adapters.ErrRoleNotEntered.Error()})
		return
	}
	err = run(reader)
	// The one failure a fresh lookup fixes, asked once, as the table read
	// asks it (#548).
	if err != nil && credentialWasRotated(err) && h.admitter != nil {
		if fresh, freshErr := h.admitter.Readmit(ctx, project); freshErr == nil {
			if freshReader, ok := fresh.(adapters.CallerReader); ok {
				err = run(freshReader)
			}
		}
	}
	if err != nil {
		log.Errorln(logsafe.Error(err))
		failure := pgrReadFailure(err)
		var pqErr *pq.Error
		if errors.As(err, &pqErr) {
			switch {
			case string(pqErr.Code) == undefinedTable:
				// A table the Database does not have is PostgREST's PGRST205,
				// with the hint its schema cache computes.
				if missing := h.tableNotFound(ctx, adapter, caller, table); missing != nil {
					failure = missing
				}
			case media.kind == pgrMediaGeoJSON:
				// PostgREST runs the query and passes the Database's refusal on
				// as it is: without PostGIS, 42883 and a 404.
				failure = pgrWriteFailure(err, true)
			}
		}
		pgrWriteError(w, failure)
		return
	}

	returned := len(rows)
	if !media.isJSON() {
		returned = int(pageRows)
	}
	w.Header().Set("Content-Range", pgrContentRange(read.offset, returned, total, counted))
	w.Header().Set("Content-Type", media.contentType())
	prefer.setApplied(w, pgrApplied{})
	var body []byte
	switch {
	case media.object():
		if len(rows) != 1 {
			w.Header().Del("Content-Type")
			pgrWriteError(w, &pgrError{
				status:  http.StatusNotAcceptable,
				code:    "PGRST116",
				message: "Cannot coerce the result to a single JSON object",
				details: fmt.Sprintf("The result contains %d rows", len(rows)),
			})
			return
		}
		body = rows[0]
	case media.isJSON():
		body = []byte("[]")
		if len(rows) > 0 {
			body, _ = json.Marshal(rows)
		}
	default:
		body = []byte(text)
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// resolveProject is the Project the call is for and the adapter that holds its
// Database: the gateway's header where rest is told to read one, and the
// install's one Project where it is not.
func (h *CRUDHandler) resolveProject(r *http.Request) (string, adapters.Adapter, error) {
	if h.projectHeader != "" {
		project := r.Header.Get(h.projectHeader)
		if !ident.IsSafeSegment(project) {
			return "", nil, &pgrError{status: http.StatusNotFound, code: "MS102", message: noSuchProject,
				hint: "the address this call arrived at names no Project"}
		}
		adapter, err := h.databaseInPath(r.Context(), project)
		if err != nil {
			return "", nil, pgrAdmissionFailure(err)
		}
		return project, adapter, nil
	}
	only, ok := h.admitter.(OnlyProjectAdmitter)
	if !ok {
		return "", nil, &pgrError{status: http.StatusNotFound, code: "MS102", message: noSuchProject,
			hint: "this rest was given no way to find the install's Project"}
	}
	adapter, project, err := only.AdmitOnly(r.Context())
	if err != nil {
		return "", nil, pgrAdmissionFailure(err)
	}
	return project, adapter, nil
}

// resolveCaller is the role the read becomes and the claims it carries.
func (h *CRUDHandler) resolveCaller(r *http.Request, project string, adapter adapters.Adapter) (adapters.Caller, error) {
	ctx := r.Context()
	if token, ok := pctx.UserToken(ctx); ok {
		if h.userTokens == nil {
			return adapters.Caller{}, pgrTokenRefused("this rest was given no address to read a Project's key set at")
		}
		role, claims, err := h.userTokens.Verify(ctx, project, token)
		switch {
		case errors.Is(err, projectkeys.ErrUnavailable):
			log.Errorln(logsafe.Error(err))
			return adapters.Caller{}, &pgrError{status: http.StatusServiceUnavailable, code: "PGRST001",
				message: "the Project's key set could not be read, so a User's token cannot be checked now; try again"}
		case err != nil:
			return adapters.Caller{}, pgrTokenFault(err)
		}
		return adapters.Caller{Role: role, Claims: claims}, nil
	}
	if role, ok := pctx.TokenRole(ctx); ok {
		claims := pctx.TokenClaims(ctx)
		if err := h.checkTokenProject(claims, project); err != nil {
			return adapters.Caller{}, err
		}
		return adapters.Caller{Role: role, Claims: claims}, nil
	}
	roles, _ := h.rolesOf(adapter)
	role, ok := anonymousRole(roles, project)
	if !ok {
		return adapters.Caller{}, &pgrError{status: http.StatusInternalServerError, code: "PGRST000", message: adapters.ErrNoAnonymousRole.Error()}
	}
	return adapters.Caller{Role: role}, nil
}

// pgrTokenRefused is a token rest refuses and PostgREST would not: one signed
// for another Project, or with claims rest cannot read. It is PGRST301, "No
// suitable key or wrong key type", the answer to a token the Project's keys did
// not sign, and details says what rest checked.
func pgrTokenRefused(details string) error {
	return pgrwire.WrongKey(details)
}

// pgrTokenFault is a User's token that did not verify, as PostgREST 14
// refuses it: the fault projectkeys carries, or PGRST301 when it carries none.
func pgrTokenFault(err error) error {
	var fault *pgrwire.TokenError
	if errors.As(err, &fault) {
		return fault
	}
	return pgrwire.WrongKey("")
}

// pgrAdmissionFailure is admissionFailure's two answers in PostgREST's shape.
func pgrAdmissionFailure(err error) error {
	status, message := admissionFailure(err)
	code := "MS102"
	if errors.Is(err, admission.ErrUnavailable) || errors.Is(err, admission.ErrTooSoon) {
		code = "MS103"
	}
	return &pgrError{status: status, code: code, message: message}
}

// pgrContentRange is PostgREST's Content-Range: the rows of the page, and the
// total when a count was asked for, `*` when it was not.
func pgrContentRange(offset, rows int, total int64, counted bool) string {
	all := "*"
	if counted {
		all = strconv.FormatInt(total, 10)
	}
	if rows == 0 {
		return "*/" + all
	}
	return fmt.Sprintf("%d-%d/%s", offset, offset+rows-1, all)
}

// pgrReadFailure is readFailure's answer in PostgREST's shape: the same
// status and the same fixed message, with the SQLSTATE as the code where the
// Database gave one, so the SDK can tell a refused row rule from a mistyped
// filter. Nothing of the driver's own words is passed on but the one message
// readFailure already passes, the Database's account of a privilege the role
// lacks.
func pgrReadFailure(err error) error {
	var refused *pgrError
	if errors.As(err, &refused) {
		return refused
	}
	status, message := readFailure(err, "")
	failure := &pgrError{status: status, code: "PGRST000", message: message}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		failure.code = string(pqErr.Code)
		switch string(pqErr.Code) {
		case undefinedTable:
			failure.status = http.StatusNotFound
		case queryCanceled:
			failure.hint = "ask for fewer rows, or add a filter an index can answer"
		case "22023":
			// PostGIS's own refusal of application/geo+json on a table with no
			// geometry column, which PostgREST passes on (PostGISSpec.hs).
			if pqErr.Message == "geometry column is missing" {
				failure.message = pqErr.Message
			}
		case "22P02", "22007", "22008", "42883":
			failure.message = "a filter's value is not of its column's type"
			if string(pqErr.Code) == "42883" {
				// An operator the column's type lacks (cs on an integer) is
				// 404 as PostgREST gives it (Error.hs, pgErrorStatus).
				failure.status = http.StatusNotFound
			}
		}
	}
	if status == http.StatusGatewayTimeout {
		failure.code = queryCanceled
		failure.hint = "ask for fewer rows, or add a filter an index can answer"
	}
	return failure
}

// pgrWriteError answers err in PostgREST's error shape: the four-field body
// with its Content-Type, the Proxy-Status PostgREST adds to every error, and
// WWW-Authenticate on a 401 (internal/pgrwire).
func pgrWriteError(w http.ResponseWriter, err error) {
	var refused *pgrError
	if !errors.As(err, &refused) {
		var fault *pgrwire.TokenError
		if errors.As(err, &fault) {
			pgrwire.Write(w, fault.Envelope())
			return
		}
		refused = &pgrError{status: http.StatusInternalServerError, code: "PGRST000", message: "the read could not be completed"}
	}
	nullable := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	details := nullable(refused.details)
	if refused.detailsList != nil {
		details = refused.detailsList
	}
	pgrwire.Write(w, pgrwire.Error{
		Status:    refused.status,
		Code:      refused.code,
		Message:   refused.message,
		Details:   details,
		Hint:      nullable(refused.hint),
		Challenge: refused.challenge,
	})
}
