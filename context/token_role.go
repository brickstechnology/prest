package context

import stdcontext "context"

// miniship (miniship-cloud#801): the role a verified token names, carried from
// the token check to the table read. A request that carries none reads as its
// Database's anonymous role.
//
// A key of its own type rather than another value of Key, so upstream's
// iota list in keys.go is not edited and a rebase does not conflict on it.
type tokenRoleKey struct{}

// WithTokenRole is ctx carrying the role a verified token named.
func WithTokenRole(ctx stdcontext.Context, role string) stdcontext.Context {
	return stdcontext.WithValue(ctx, tokenRoleKey{}, role)
}

// TokenRole is the role a verified token named, and ok is false when the
// request carried no token.
func TokenRole(ctx stdcontext.Context) (role string, ok bool) {
	role, ok = ctx.Value(tokenRoleKey{}).(string)
	return role, ok && role != ""
}
