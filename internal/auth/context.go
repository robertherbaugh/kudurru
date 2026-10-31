package auth

import "context"

type ctxKey int

const (
	principalKey ctxKey = iota
	sessionKey
)

// WithPrincipal returns a context carrying the authenticated principal. The
// Authenticate step of the middleware chain (§8.8) sets it; Authorize and
// handlers read it with PrincipalFrom.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// PrincipalFrom returns the principal set by WithPrincipal while any private
// verified credential time bounds still hold. No store or network read occurs.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey).(Principal)
	if ok && !p.credentialCurrent() {
		return Principal{}, false
	}
	return p, ok
}

// WithSession returns a context carrying the cookie session that
// authenticated the request. Bearer-authenticated requests have none.
func WithSession(ctx context.Context, s Session) context.Context {
	return context.WithValue(ctx, sessionKey, s)
}

// SessionFrom returns the session set by WithSession.
func SessionFrom(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(sessionKey).(Session)
	return s, ok
}
