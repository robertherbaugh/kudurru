package servinghttp

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
)

// CookiePolicy combines immutable host cookie names/path/proxy trust with the
// ORIGINAL request View's secure mode. Missing policy is a refusal, never a
// fallback to startup or a fresh capture. It cannot authorize a command.
func CookiePolicy(ctx context.Context, host session.CookieConfig) (session.CookieConfig, error) {
	v, err := Component[*servingauth.View](ctx, "auth")
	if err != nil {
		return session.CookieConfig{}, err
	}
	return CookiePolicyForView(v, host)
}

// CookiePolicyForView also supports the existing postcommit sealing check. A
// caller must compare this with the original policy, never adopt a successor.
func CookiePolicyForView(v *servingauth.View, host session.CookieConfig) (session.CookieConfig, error) {
	controls, err := v.AuthControls()
	if err != nil {
		return session.CookieConfig{}, err
	}
	host.Secure = session.SecureMode(controls.SessionSecure)
	if err := host.Validate(); err != nil {
		return session.CookieConfig{}, servingauth.ErrInvalid
	}
	return host, nil
}
