package adapters

import (
	"context"
	"errors"
)

// miniship: rest reads a database as that database's anonymous role, never as
// the login it connected with. The login holds nothing; each read opens a
// transaction, becomes the role with SET LOCAL ROLE, reads, and ends. Row
// security in the database then decides which rows a caller receives, rather
// than code here deciding after the rows have crossed the wire.
//
// These interfaces are new rather than methods added to upstream's
// QueryExecutor and DatabaseRegistry, so an upstream rebase does not conflict
// on those interfaces or on their generated mocks.

// AnonymousRoles names the role a registered database's reads become.
type AnonymousRoles interface {
	// AnonymousRole returns the role configured for alias. ok is false when
	// alias is not a database rest was given, or when it was given with no
	// role: there is no default, and such a database is not served.
	AnonymousRole(alias string) (role string, ok bool)
}

// RoleReader runs a read inside a read-only transaction that has become role.
// The database is the one named in ctx, as QueryCtx reads it.
type RoleReader interface {
	QueryAsRoleCtx(ctx context.Context, role, SQL string, params ...interface{}) (sc Scanner)
	QueryCountAsRoleCtx(ctx context.Context, role, SQL string, params ...interface{}) (sc Scanner)
}

var (
	// ErrNoAnonymousRole is returned for a read asked to run as no role.
	ErrNoAnonymousRole = errors.New("no anonymous role is configured for this database")
	// ErrRoleNotEntered is returned when the transaction could not become the
	// role. Nothing is read: the statement never runs as the login.
	ErrRoleNotEntered = errors.New("could not become the anonymous role")
)
