package adapters

import (
	"context"
	"database/sql"
)

// miniship (miniship-cloud#1302): the SDK's /rest/v1 address reads as its
// caller, as the table read does, and hands the Database the caller's claims,
// as PostgREST does: request.jwt.claims holds the token's payload, and
// request.jwt.claim.sub and request.jwt.claim.role its two fields a row rule
// reads most. A new interface for the reason anonymous_role.go gives.

// CallerReader runs read inside one read-only transaction on the Database
// named in ctx that has become role, holds the time limit, and carries claims.
type CallerReader interface {
	ReadAsCallerCtx(ctx context.Context, caller Caller, read func(tx *sql.Tx) error) error
}

// Caller is who a read is for: the role it becomes, and the claims of the
// token that named it, or nil for a read that carried no token.
type Caller struct {
	Role   string
	Claims []byte
}
