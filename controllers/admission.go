package controllers

import (
	"context"
	"errors"
	"net/http"

	"github.com/lib/pq"

	"github.com/prest/prest/v2/adapters"
	"github.com/prest/prest/v2/admission"
)

// miniship (#548): a Project rest was not given is looked up on a miss, and
// the table read is where the miss happens. The package comment of admission/
// is the shape; this file is only what the handler does with it.

// Admitter learns about a Project rest was not given, while rest runs.
// admission.Gate is the one there is; the interface is here so controllers
// does not import the composition root that builds it.
type Admitter interface {
	// Admit answers with the adapter for project, looking it up when rest
	// has not seen it.
	Admit(ctx context.Context, project string) (adapters.Adapter, error)
	// Readmit looks project up again although rest has a pool for it, which
	// is what a credential that was rotated needs. It refuses a second
	// forced lookup made too soon after the first.
	Readmit(ctx context.Context, project string) (adapters.Adapter, error)
}

// The SQLSTATEs that say rest's own credential for a Database is no longer the
// one that Database accepts. Both are raised before any statement runs, so
// nothing was read: 28P01 is the password, and 28000 is every other way the
// login itself was refused.
const (
	invalidPassword               = "28P01"
	invalidAuthorizationSpecifier = "28000"
)

// credentialWasRotated reports whether err is the Database refusing rest's
// login rather than refusing the read. It is the one failure that a fresh
// answer can fix, so it is the one that causes a second lookup.
func credentialWasRotated(err error) bool {
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		return false
	}
	return pqErr.Code == invalidPassword || pqErr.Code == invalidAuthorizationSpecifier
}

// admissionFailure is the answer a Project rest could not resolve gives.
//
// The two admission answers are different things and a caller can do different
// things about them: there is no such Project, so asking again will not help;
// or rest could not find out, so asking again will — the answerer being down,
// and a Project looked up too recently to look up again, are both that.
// Neither says anything about where a Database is or who the answerer is.
//
// Anything else is upstream's own account of a name it will not serve, in the
// shape and the status #547 left it: a config with no registry at all still
// answers pg.single's question in pREST's words.
func admissionFailure(err error) (int, string) {
	switch {
	case errors.Is(err, admission.ErrNoSuchProject):
		return http.StatusNotFound, noSuchProject
	case errors.Is(err, admission.ErrUnavailable), errors.Is(err, admission.ErrTooSoon):
		return http.StatusServiceUnavailable, notAdmittedYet
	}
	return http.StatusNotFound, err.Error()
}

// The two messages, fixed as #549 made every other error answer fixed: the
// same words for every caller and every Project, and none of the caller's own
// bytes in them. Upstream, and rest until now, answered a name it was not
// given by quoting that name back.
const (
	noSuchProject  = "no such Project"
	notAdmittedYet = "this Project could not be admitted; try again"
)
