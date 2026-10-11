package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Authentication methods recorded in Principal.AuthMethod.
const (
	MethodToken    = "token"    // bearer kdr_ API token
	MethodJWT      = "jwt"      // bearer OIDC ID token
	MethodK8s      = "k8s"      // bearer Kubernetes ServiceAccount token
	MethodCookie   = "cookie"   // sealed session cookie
	MethodPassword = "password" // POST /auth/login (the session it issues says cookie afterwards)
	MethodUnix     = "unix"     // unix-socket peer credentials
	MethodInternal = "internal" // the internal-listener token of a peer node
)

// CSRFHeader is the header a cookie-authenticated unsafe request must carry,
// echoing the session's CSRF token from GET /api/v1/auth/me (§8.2).
const CSRFHeader = "X-Kudurru-CSRF"

// ErrNoCredentials is returned by an Authenticator when the request carries
// no credential of its kind; the chain then tries the next step.
var ErrNoCredentials = errors.New("auth: no credentials of this kind")

// Error is an authentication or authorization failure in the shape of the
// API contract (docs/architecture.md §10): HTTP status, a stable key, a
// user-safe message and an optional hint. Authenticators return it for a
// credential of their kind that is present but rejected.
type Error struct {
	Status  int
	Key     string
	Message string
	// Hint is a string or a JSON-encodable object (403 carries
	// {"missing":{"resource","verb","scope"}}).
	Hint any
	// RetryAfter sets the Retry-After header (429).
	RetryAfter time.Duration
	// Err is the internal cause; it is logged, never sent.
	Err error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Key + ": " + e.Message + ": " + e.Err.Error()
	}
	return e.Key + ": " + e.Message
}

// Unwrap returns the internal cause.
func (e *Error) Unwrap() error { return e.Err }

// Unauthenticated returns the 401 error every bearer authenticator uses for
// a rejected credential. The message is deliberately the same for an
// unknown token id, a wrong secret, an expired token and a bad signature
// (§8.6: no oracle).
func Unauthenticated(cause error) *Error {
	return &Error{Status: http.StatusUnauthorized, Key: "unauthenticated", Message: "invalid or expired credentials", Err: cause}
}

// AsError returns the *Error in err's chain, or a 401 wrapping err.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return Unauthenticated(err)
}

// errorBody is the contract's error envelope.
type errorBody struct {
	Error  errorDetail `json:"error"`
	TookMS int64       `json:"took_ms"`
}

type errorDetail struct {
	Key     string `json:"key"`
	Message string `json:"message"`
	Hint    any    `json:"hint,omitempty"`
}

// WriteError writes e as {error:{key,message,hint}, took_ms}. took_ms is
// measured from the start recorded by Middleware (0 without it). 401
// responses carry WWW-Authenticate, 429 responses Retry-After.
func WriteError(w http.ResponseWriter, r *http.Request, e *Error) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	switch e.Status {
	case http.StatusUnauthorized:
		h.Set("WWW-Authenticate", `Bearer realm="kudurru"`)
	case http.StatusTooManyRequests:
		secs := int64((e.RetryAfter + time.Second - 1) / time.Second)
		h.Set("Retry-After", strconv.FormatInt(max(secs, 1), 10))
	}
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: errorDetail{Key: e.Key, Message: e.Message, Hint: e.Hint}, TookMS: TookMS(r)})
}

// TookMS returns the milliseconds since Middleware first saw r (0 when it
// did not).
func TookMS(r *http.Request) int64 {
	if r == nil {
		return 0
	}
	if t, ok := r.Context().Value(startKey).(time.Time); ok {
		return time.Since(t).Milliseconds()
	}
	return 0
}

// Authenticator is one step of the §8.8 Authenticate chain.
//
// Authenticate returns ErrNoCredentials when r carries no credential of its
// kind, an error (usually *Error) when it carries one that is rejected,
// and otherwise the principal and the request to continue with (which may
// carry extra context, such as the cookie session, and may have written
// headers such as a re-sealed Set-Cookie on w). A nil request means r.
type Authenticator interface {
	Authenticate(w http.ResponseWriter, r *http.Request) (Principal, *http.Request, error)
}

// AuthenticatorFunc adapts a function to Authenticator.
type AuthenticatorFunc func(w http.ResponseWriter, r *http.Request) (Principal, *http.Request, error)

// Authenticate implements Authenticator.
func (f AuthenticatorFunc) Authenticate(w http.ResponseWriter, r *http.Request) (Principal, *http.Request, error) {
	return f(w, r)
}

// Loader is the LoadPrincipal step of §8.8: it re-checks an authenticated
// principal against current state (account disabled or deleted,
// mustChangePassword, live groups) and may replace fields. An error
// rejects the request (401 unless it is an *Error with another status).
type Loader func(ctx context.Context, p Principal) (Principal, error)

// Options configures Middleware.
type Options struct {
	// Steps are tried in order; the first that authenticates wins. The
	// production order (§8.8) is bearer kdr_ → bearer JWT routed by iss
	// (OIDC ID tokens, Kubernetes ServiceAccount tokens) → session cookie
	// (+CSRF on unsafe methods) → unix peer → internal token.
	Steps []Authenticator
	// Load runs after a successful step; nil keeps the principal as is.
	Load Loader
	// OnError, when set, observes every rejected credential (metrics,
	// sanitized logs) before the error response is written.
	OnError func(r *http.Request, e *Error)
}

type startCtxKey struct{}

var startKey = startCtxKey{}

// WithStart records the instant the API server started handling a request,
// the origin of took_ms. Middleware sets it when the API server has not.
func WithStart(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, startKey, t)
}

// Middleware returns the Authenticate + LoadPrincipal steps of §8.8. A
// request no step authenticates passes through anonymous (public routes
// such as /auth/status and /auth/login serve it; the Authorize step turns
// it into 401 elsewhere). A request whose credential is rejected is
// answered here with the contract's error body and never reaches next.
// The authenticated principal is stored with WithPrincipal.
func Middleware(opts Options) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := r.Context().Value(startKey).(time.Time); !ok {
				r = r.WithContext(WithStart(r.Context(), time.Now()))
			}
			for _, step := range opts.Steps {
				p, r2, err := step.Authenticate(w, r)
				if errors.Is(err, ErrNoCredentials) {
					continue
				}
				if err != nil {
					fail(w, r, opts, AsError(err))
					return
				}
				if r2 == nil {
					r2 = r
				}
				if opts.Load != nil {
					if p, err = opts.Load(r2.Context(), p); err != nil {
						fail(w, r, opts, AsError(err))
						return
					}
				}
				next.ServeHTTP(w, r2.WithContext(WithPrincipal(r2.Context(), p)))
				return
			}
			if BearerToken(r) != "" {
				// A bearer credential no step claimed is wrong, not absent:
				// never fall back to anonymous (or to a cookie) for it.
				fail(w, r, opts, Unauthenticated(errors.New("unrecognised bearer credential")))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func fail(w http.ResponseWriter, r *http.Request, opts Options, e *Error) {
	if opts.OnError != nil {
		opts.OnError(r, e)
	}
	WriteError(w, r, e)
}

// BearerToken returns the bearer credential of r, or "".
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}

// IsSafeMethod reports whether m never changes state (GET, HEAD, OPTIONS):
// such requests need no CSRF header.
func IsSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// defaultCOP is the cross-origin protection CSRF applies.
var defaultCOP = http.NewCrossOriginProtection()

// CSRF checks a request authenticated by the session cookie (§8.2): unsafe
// methods must pass Go's CrossOriginProtection (Sec-Fetch-Site / Origin)
// and carry the session's token in X-Kudurru-CSRF (constant-time compare).
// Safe methods and requests not authenticated by a cookie session (bearer
// tokens, unix peer, internal token) are exempt and return nil. The error
// is a 403 *Error with key "csrf".
func CSRF(r *http.Request) error { return CSRFWith(r, defaultCOP) }

// CSRFWith is CSRF with the API server's CrossOriginProtection (which knows
// its trusted origins); nil uses the default instance.
func CSRFWith(r *http.Request, cop *http.CrossOriginProtection) error {
	s, ok := SessionFrom(r.Context())
	if !ok {
		return nil
	}
	return CheckCSRF(r, s.CSRF, cop)
}

// CheckCSRF is the CSRF rule for a request about to be authenticated by a
// session whose token is want (the cookie step calls it before the session
// is in the context).
func CheckCSRF(r *http.Request, want string, cop *http.CrossOriginProtection) error {
	if IsSafeMethod(r.Method) {
		return nil
	}
	if cop == nil {
		cop = defaultCOP
	}
	if err := cop.Check(r); err != nil {
		return &Error{Status: http.StatusForbidden, Key: "csrf", Message: "cross-origin request rejected", Err: err}
	}
	got := r.Header.Get(CSRFHeader)
	if got == "" || want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return &Error{Status: http.StatusForbidden, Key: "csrf", Message: "missing or invalid " + CSRFHeader + " header",
			Hint: "send the csrf value of GET /api/v1/auth/me in the " + CSRFHeader + " header"}
	}
	return nil
}
