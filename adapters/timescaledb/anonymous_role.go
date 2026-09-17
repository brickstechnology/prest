package timescaledb

import (
	"context"

	"github.com/prest/prest/v2/adapters"
	"github.com/prest/prest/v2/adapters/scanner"
)

// miniship: the embedded field is the adapters.Adapter interface, which does
// not carry rest's read-as-a-role methods, so they are delegated here. Without
// these a database detected as TimescaleDB would be refused rather than read.

var (
	_ adapters.AnonymousRoles = (*Adapter)(nil)
	_ adapters.RoleReader     = (*Adapter)(nil)
)

// AnonymousRole delegates to the wrapped postgres adapter.
func (a *Adapter) AnonymousRole(alias string) (string, bool) {
	roles, ok := a.Adapter.(adapters.AnonymousRoles)
	if !ok {
		return "", false
	}
	return roles.AnonymousRole(alias)
}

// QueryAsRoleCtx delegates to the wrapped postgres adapter.
func (a *Adapter) QueryAsRoleCtx(ctx context.Context, role, SQL string, params ...interface{}) adapters.Scanner {
	reader, ok := a.Adapter.(adapters.RoleReader)
	if !ok {
		return &scanner.PrestScanner{Error: adapters.ErrRoleNotEntered}
	}
	return reader.QueryAsRoleCtx(ctx, role, SQL, params...)
}

// QueryCountAsRoleCtx delegates to the wrapped postgres adapter.
func (a *Adapter) QueryCountAsRoleCtx(ctx context.Context, role, SQL string, params ...interface{}) adapters.Scanner {
	reader, ok := a.Adapter.(adapters.RoleReader)
	if !ok {
		return &scanner.PrestScanner{Error: adapters.ErrRoleNotEntered}
	}
	return reader.QueryCountAsRoleCtx(ctx, role, SQL, params...)
}
