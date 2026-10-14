package context

import stdcontext "context"

// miniship (miniship-cloud#1302): what the /rest/v1 address needs of a token
// beyond the role patch 18 carries. The claims are handed to the Database as
// request.jwt.claims, as PostgREST hands them, so a row rule can read the
// User's id. A User's own token is not the install's, so the token check
// cannot verify it before the Project is known; it carries the token on to
// the handler, which verifies it against that Project's key set.
//
// Keys of their own types, for the reason token_role.go gives.
type (
	tokenClaimsKey struct{}
	userTokenKey   struct{}
)

// WithTokenClaims is ctx carrying the payload of a token that verified, as
// the JSON object it was signed as.
func WithTokenClaims(ctx stdcontext.Context, claims []byte) stdcontext.Context {
	return stdcontext.WithValue(ctx, tokenClaimsKey{}, claims)
}

// TokenClaims is the payload of the token that verified, or nil.
func TokenClaims(ctx stdcontext.Context) []byte {
	claims, _ := ctx.Value(tokenClaimsKey{}).([]byte)
	return claims
}

// WithUserToken is ctx carrying a User's token that is still to be verified,
// against the key set of the Project the call is for.
func WithUserToken(ctx stdcontext.Context, token string) stdcontext.Context {
	return stdcontext.WithValue(ctx, userTokenKey{}, token)
}

// UserToken is the User's token still to be verified, and ok is false when
// the request carried none.
func UserToken(ctx stdcontext.Context) (token string, ok bool) {
	token, ok = ctx.Value(userTokenKey{}).(string)
	return token, ok && token != ""
}
