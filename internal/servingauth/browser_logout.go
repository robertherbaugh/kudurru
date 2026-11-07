package servingauth

import (
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/browseroidc"
)

// SealBrowserLogout encrypts redirect evidence using this complete cut's keys.
// It grants no session, credential, pending row or current writer authority.
func (v *View) SealBrowserLogout(l browseroidc.Logout, now time.Time) (string, error) {
	if v == nil || v.released.Load() || v.logoutCodec == nil {
		return "", ErrCredentials
	}
	return v.logoutCodec.Seal(l, now)
}

// OpenBrowserLogout opens bounded private redirect evidence only. Callers must
// join its exact original enrollment/provider tuple to their captured request.
func (v *View) OpenBrowserLogout(raw string, now time.Time) (browseroidc.Logout, error) {
	if v == nil || v.released.Load() || v.logoutCodec == nil {
		return browseroidc.Logout{}, ErrCredentials
	}
	return v.logoutCodec.Open(raw, now)
}
