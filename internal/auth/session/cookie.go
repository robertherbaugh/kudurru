package session

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// SecureMode controls the cookie Secure attribute (auth.session.secure).
type SecureMode string

// Secure modes. Auto marks the cookie Secure when TLS terminates in kudurru
// or, if TrustForwardedProto is set, when a trusted proxy reports https.
// Plain-HTTP labs still get HttpOnly, SameSite=Lax and the CSRF header.
const (
	SecureAuto   SecureMode = "auto"
	SecureAlways SecureMode = "always"
	SecureNever  SecureMode = "never"
)

// CookieConfig describes how a cookie is written. Every cookie kudurru
// writes is HttpOnly and SameSite=Lax: Lax (not Strict) is required so the
// browser sends the pre-auth cookie on the IdP's top-level redirect back to
// the callback.
type CookieConfig struct {
	// Name of the cookie. Default kudurru_session.
	Name string
	// Path of the cookie. Default "/".
	Path string
	// Secure selects the Secure attribute. Default auto.
	Secure SecureMode
	// TrustForwardedProto lets auto mode honour X-Forwarded-Proto: https.
	// Only enable it when the RealIP(trustedProxies) step guarantees the
	// header comes from a trusted proxy.
	TrustForwardedProto bool
}

// Validate checks the mode and fills defaults.
func (c *CookieConfig) Validate() error {
	if c.Name == "" {
		c.Name = DefaultCookieName
	}
	if c.Path == "" {
		c.Path = "/"
	}
	switch c.Secure {
	case "":
		c.Secure = SecureAuto
	case SecureAuto, SecureAlways, SecureNever:
	default:
		return fmt.Errorf("session: cookie secure mode %q is not auto|always|never", c.Secure)
	}
	return nil
}

// IsSecure reports whether a cookie written in response to r gets the
// Secure attribute.
func (c CookieConfig) IsSecure(r *http.Request) bool {
	switch c.Secure {
	case SecureAlways:
		return true
	case SecureNever:
		return false
	}
	if r.TLS != nil {
		return true
	}
	return c.TrustForwardedProto && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// Set writes the cookie with value and a Max-Age of maxAge (rounded up to
// whole seconds). A zero maxAge makes it a browser-session cookie. Max-Age
// rather than Expires keeps the lifetime independent of the browser's
// clock.
func (c CookieConfig) Set(w http.ResponseWriter, r *http.Request, value string, maxAge time.Duration) {
	// Secure follows auth.session.secure (auto|always|never); HttpOnly and
	// SameSite=Lax are unconditional.
	ck := &http.Cookie{ //nolint:gosec // G124: Secure is computed by IsSecure, see above
		Name:     c.name(),
		Value:    value,
		Path:     c.path(),
		HttpOnly: true,
		Secure:   c.IsSecure(r),
		SameSite: http.SameSiteLaxMode,
	}
	if maxAge > 0 {
		ck.MaxAge = int((maxAge + time.Second - 1) / time.Second)
	}
	http.SetCookie(w, ck)
}

// Clear deletes the cookie.
func (c CookieConfig) Clear(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is computed by IsSecure
		Name:     c.name(),
		Value:    "",
		Path:     c.path(),
		MaxAge:   -1,
		Expires:  time.Unix(0, 0).UTC(),
		HttpOnly: true,
		Secure:   c.IsSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func (c CookieConfig) name() string {
	if c.Name == "" {
		return DefaultCookieName
	}
	return c.Name
}

func (c CookieConfig) path() string {
	if c.Path == "" {
		return "/"
	}
	return c.Path
}
