package session

import (
	"errors"
	"net/http"

	"github.com/robertherbaugh/kudurru/internal/auth"
)

var _ auth.Authenticator = (*Authenticator)(nil)

// Authenticate implements auth.Authenticator: the cookie step of the
// auth.Middleware chain. It is Middleware's logic with the contract's
// error bodies:
//
//   - no cookie, or a request carrying Authorization (bearer requests are
//     the bearer steps' and CSRF-exempt): auth.ErrNoCredentials;
//   - a cookie that does not open (tampered, expired, idle, revoked by
//     sessionsValidAfter): the cookie is cleared and the request continues
//     anonymous (auth.ErrNoCredentials), exactly like Middleware;
//   - an unsafe method failing CrossOriginProtection or the X-Kudurru-CSRF
//     check: 403 "csrf";
//   - otherwise the idle timeout slides (Set-Cookie at most once a minute)
//     and the principal is the session's, with Grants = the roles snapshot
//     (D48) and GrantsIssuedAt = iat, so rbac.BindingSource honours each
//     grant only while a live binding still provides it. The returned
//     request carries the session (auth.SessionFrom).
func (a *Authenticator) Authenticate(w http.ResponseWriter, r *http.Request) (auth.Principal, *http.Request, error) {
	if r.Header.Get("Authorization") != "" {
		return auth.Principal{}, nil, auth.ErrNoCredentials
	}
	ck, err := r.Cookie(a.cookie.NameFor(r))
	if err != nil || ck.Value == "" {
		return auth.Principal{}, nil, auth.ErrNoCredentials
	}
	ctx := r.Context()
	s, err := a.store.Load(ctx, ck.Value)
	if err != nil {
		a.log.DebugContext(ctx, "session cookie rejected", "reason", err.Error())
		a.cookie.Clear(w, r)
		return auth.Principal{}, nil, auth.ErrNoCredentials
	}
	if err := auth.CheckCSRF(r, s.CSRF, a.cop); err != nil {
		return auth.Principal{}, nil, err
	}
	v, changed, err := a.store.Touch(ctx, &s)
	switch {
	case err == nil && changed:
		a.cookie.Set(w, r, v, s.ExpiresAt.Sub(a.now()))
	case err != nil && !errors.Is(err, auth.ErrUnsupported):
		a.log.WarnContext(ctx, "session touch failed", "reason", err.Error(), "sid", s.ID)
	}
	p := s.Principal
	p.AuthMethod = auth.MethodCookie
	p.Grants = append([]string(nil), s.RolesSnapshot...)
	p.GrantsIssuedAt = s.IssuedAt
	return p, r.WithContext(auth.WithSession(ctx, s)), nil
}
