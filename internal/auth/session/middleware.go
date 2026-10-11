package session

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
)

// CSRFHeader is the header a cookie-authenticated unsafe request must carry,
// echoing the session's CSRF token from GET /api/v1/auth/me.
const CSRFHeader = "X-Kudurru-CSRF"

// AuthMethodCookie is Principal.AuthMethod for cookie-authenticated requests.
const AuthMethodCookie = "cookie"

// Authenticator is the cookie step of Authenticate in the §8.8 chain:
//
//	… → CrossOriginProtection → Authenticate(… | cookie+CSRF | …) → LoadPrincipal → Authorize → …
//
// For a request carrying the session cookie and no Authorization header it
// loads the session (seal, idle and absolute expiry, sessionsValidAfter),
// enforces CSRF on unsafe methods, slides the idle timeout, and stores the
// session and principal in the request context. Requests without a valid
// cookie pass through anonymous; Authorize turns that into 401. Bearer
// requests are left to the bearer authenticators and are CSRF-exempt
// (browsers cannot attach Authorization cross-site without a preflight).
type Authenticator struct {
	store  auth.SessionStore
	cookie CookieConfig
	cop    *http.CrossOriginProtection
	log    *slog.Logger
	now    func() time.Time
}

// AuthenticatorOptions configures an Authenticator.
type AuthenticatorOptions struct {
	// Store opens and re-seals sessions. Required.
	Store auth.SessionStore
	// Cookie is the session cookie's configuration.
	Cookie CookieConfig
	// CrossOrigin is the Go 1.25 cross-origin protection applied to
	// cookie-authenticated unsafe requests. The API server should share the
	// instance it installs globally (with its trusted origins); nil uses
	// http.NewCrossOriginProtection().
	CrossOrigin *http.CrossOriginProtection
	// Logger receives debug lines for rejected cookies; default discards.
	Logger *slog.Logger
	// Now is the clock used for cookie Max-Age; default time.Now.
	Now func() time.Time
}

// NewAuthenticator validates opts.
func NewAuthenticator(opts AuthenticatorOptions) (*Authenticator, error) {
	if opts.Store == nil {
		return nil, errors.New("session: authenticator needs a store")
	}
	if err := opts.Cookie.Validate(); err != nil {
		return nil, err
	}
	if opts.CrossOrigin == nil {
		opts.CrossOrigin = http.NewCrossOriginProtection()
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Authenticator{store: opts.Store, cookie: opts.Cookie, cop: opts.CrossOrigin, log: opts.Logger, now: opts.Now}, nil
}

// Cookie returns the session cookie configuration (with defaults applied).
func (a *Authenticator) Cookie() CookieConfig { return a.cookie }

// Store returns the session store.
func (a *Authenticator) Store() auth.SessionStore { return a.store }

// Middleware wraps next with cookie authentication.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			next.ServeHTTP(w, r)
			return
		}
		ck, err := r.Cookie(a.cookie.NameFor(r))
		if err != nil || ck.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		s, err := a.store.Load(ctx, ck.Value)
		if err != nil {
			a.log.DebugContext(ctx, "session cookie rejected", "reason", err.Error())
			a.cookie.Clear(w, r)
			next.ServeHTTP(w, r)
			return
		}
		if !isSafeMethod(r.Method) {
			if err := a.cop.Check(r); err != nil {
				writeError(w, http.StatusForbidden, "csrf", "cross-origin request rejected")
				return
			}
			if !validCSRF(r.Header.Get(CSRFHeader), s.CSRF) {
				writeError(w, http.StatusForbidden, "csrf", "missing or invalid "+CSRFHeader+" header")
				return
			}
		}
		v, changed, err := a.store.Touch(ctx, &s)
		switch {
		case err == nil && changed:
			a.cookie.Set(w, r, v, s.ExpiresAt.Sub(a.now()))
		case err != nil && !errors.Is(err, auth.ErrUnsupported):
			// Expired between Load and Touch, or re-seal failed: the
			// request still carries a session that was valid at Load,
			// so serve it but do not extend it.
			a.log.WarnContext(ctx, "session touch failed", "reason", err.Error(), "sid", s.ID)
		}
		p := s.Principal
		p.AuthMethod = AuthMethodCookie
		ctx = auth.WithSession(auth.WithPrincipal(ctx, p), s)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func isSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

func validCSRF(got, want string) bool {
	return got != "" && want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// SecurityHeaders is the §8.8 SecurityHeaders step: CSP default-src 'self',
// framing denied, nosniff, a same-origin referrer policy, and HSTS when TLS
// terminates in kudurru. It lives here until the API server package lands.
func SecurityHeaders(hsts bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'self'")
			h.Set("X-Frame-Options", "DENY")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "same-origin")
			if hsts && r.TLS != nil {
				h.Set("Strict-Transport-Security", "max-age=31536000")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// apiError is the §8.7 error body.
type apiError struct {
	Error  string `json:"error"`
	Reason string `json:"reason,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, reason string) {
	writeJSON(w, status, apiError{Error: code, Reason: reason})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
