package controllers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/prest/prest/v2/adapters"
	pctx "github.com/prest/prest/v2/context"
	"github.com/prest/prest/v2/internal/ident"

	"github.com/gorilla/mux"
)

func requestContext(r *http.Request, database string) (context.Context, context.CancelFunc) {
	ctx := context.WithValue(r.Context(), pctx.DBNameKey, database)
	timeout, ok := ctx.Value(pctx.HTTPTimeoutKey).(int)
	if !ok || timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Second*time.Duration(timeout))
}

func validateDatabase(database string, registry adapters.DatabaseRegistry, singleDB bool) error {
	if registry != nil && !registry.IsRegistered(database) {
		return fmt.Errorf("database not registered: %v", database)
	}
	if singleDB && registry != nil && registry.GetDatabase() != database {
		return fmt.Errorf("database not registered: %v", database)
	}
	return nil
}

func validatePathSegments(segments ...string) bool {
	for _, s := range segments {
		if !ident.IsSafeSegment(s) {
			return false
		}
	}
	return true
}

func pathVars(r *http.Request) map[string]string {
	return mux.Vars(r)
}

// withQuery returns a shallow copy of r carrying query in place of its own
// (miniship). The builders downstream all read r.URL.Query(), so handing them
// a request whose query string is the screened one is what keeps the caller's
// own bytes from reaching any of them.
func withQuery(r *http.Request, query url.Values) *http.Request {
	clone := *r
	u := *r.URL
	u.RawQuery = query.Encode()
	clone.URL = &u
	return &clone
}
