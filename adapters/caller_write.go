package adapters

import (
	"context"
	"database/sql"
	"errors"
)

// miniship (miniship-cloud#1303): the SDK's /rest/v1 address writes as its
// caller, in one transaction, with the caller's claims in it as a read has
// them. A new interface for the reason anonymous_role.go gives.

// ErrRollback is what a write returns to end its transaction by rolling it
// back with nothing wrong: Prefer: tx=rollback asks for it, and so does a
// write that finds it changed more than it may. WriteAsCallerCtx answers nil
// for it, and the closure that returned it holds the answer.
var ErrRollback = errors.New("the write's transaction was rolled back")

// CallerWriter runs write inside one read-write transaction on the Database
// named in ctx that has become the caller's role, holds the time limit, and
// carries the caller's claims. It commits when write returns nil, and rolls
// back when write returns anything else.
type CallerWriter interface {
	WriteAsCallerCtx(ctx context.Context, caller Caller, write func(tx *sql.Tx) error) error
}
