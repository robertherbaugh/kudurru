// Package servinghttp binds HTTP authentication and read authorization to one
// complete serving lease. Canonical mutations must separately reauthorize the
// retained raw credential within the admitted command transaction.
package servinghttp

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
)

// Options pins the host's cookie/CSRF policy. Provider and local-login endpoints
// use their own current command flows; Basic/password authentication is absent.
type Options struct {
	Publisher             *serving.Publisher
	Cookie                session.CookieConfig
	CrossOriginProtection *http.CrossOriginProtection
	MaxDuration           time.Duration
	Now                   func() time.Time
}

// Adapter owns no independent auth cache, Store reader or background watcher.
type Adapter struct{ options Options }

// New validates dependencies. Streams must reconnect at the finite request
// deadline; owners are released after the handler stops using the lease.
func New(options Options) (*Adapter, error) {
	if options.Publisher == nil {
		return nil, serving.ErrComponents
	}
	if err := options.Cookie.Validate(); err != nil {
		return nil, err
	}
	if options.MaxDuration == 0 {
		options.MaxDuration = 5 * time.Minute
	}
	if options.MaxDuration < time.Millisecond || options.MaxDuration > 24*time.Hour {
		return nil, serving.ErrComponents
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Adapter{options: options}, nil
}

type leaseKey struct{}
type proofKey struct{}

type proof struct {
	lease      *serving.Lease
	credential servingauth.Credential
}

func (proof) String() string     { return "private HTTP credential" }
func (p proof) GoString() string { return p.String() }

// Component returns a typed immutable owner from this request's held lease.
// Callers must not retain the result beyond the synchronous handler lifetime.
func Component[T any](ctx context.Context, name string) (T, error) {
	var zero T
	lease, ok := ctx.Value(leaseKey{}).(*serving.Lease)
	if !ok || lease.Bundle() == nil || ctx.Err() != nil {
		return zero, serving.ErrReleased
	}
	value, found := lease.Bundle().Component(name)
	typed, ok := value.(T)
	if !found || !ok || nilValue(typed) {
		return zero, serving.ErrComponents
	}
	return typed, nil
}

func nilValue(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}

// Credential returns the original untrusted proof for current command
// reauthorization. Possession of this value conveys no mutation authority.
// Passwords are accepted only by explicit login command endpoints.
func Credential(ctx context.Context) (servingauth.Credential, error) {
	p, ok := ctx.Value(proofKey{}).(proof)
	lease, _ := ctx.Value(leaseKey{}).(*serving.Lease)
	if !ok || p.lease == nil || p.lease != lease || lease.Bundle() == nil || ctx.Err() != nil {
		return servingauth.Credential{}, servingauth.ErrCredentials
	}
	return p.credential, nil
}

// Identity returns the actual served cut of this request, never an installed
// generation sampled independently from the credential or object readers.
func Identity(ctx context.Context) (serving.Identity, error) {
	lease, _ := ctx.Value(leaseKey{}).(*serving.Lease)
	if lease == nil || lease.Bundle() == nil || ctx.Err() != nil {
		return serving.Identity{}, serving.ErrReleased
	}
	return lease.Bundle().Identity(), nil
}

// CheckWriter checks this request's ORIGINAL complete lease against the
// publisher and carried writer target. It neither captures a replacement lease
// nor grants transaction authority. Root retains the writer and dependency pins,
// reauthorizes the raw Credential in CommandTx, and calls this memory-only check
// again at final commit, including for deduplication and no-op commands.
func CheckWriter(ctx context.Context, publisher *serving.Publisher, target serving.TermActivation) error {
	if ctx == nil || publisher == nil {
		return serving.ErrComponents
	}
	lease, _ := ctx.Value(leaseKey{}).(*serving.Lease)
	if lease == nil || lease.Bundle() == nil {
		return serving.ErrReleased
	}
	return publisher.CheckWriterLease(ctx, lease, target)
}

// Middleware captures BEFORE authentication and retains every owner through
// the entire handler, including streaming responses. An unavailable complete
// bundle returns 503 and never attempts independent live authentication.
func (a *Adapter) Middleware(next http.Handler) http.Handler {
	verify := auth.Middleware(auth.Options{Steps: []auth.Authenticator{auth.AuthenticatorFunc(a.authenticate)}})(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), a.options.MaxDuration)
		defer cancel()
		lease, err := a.options.Publisher.CaptureHTTP(ctx)
		if err != nil {
			auth.WriteError(w, r, &auth.Error{Status: http.StatusServiceUnavailable, Key: "generation_pending", Message: "complete serving generation unavailable", RetryAfter: time.Second})
			return
		}
		defer lease.Release()
		r = r.WithContext(context.WithValue(ctx, leaseKey{}, lease))
		if _, err := Component[*servingauth.View](r.Context(), "auth"); err != nil {
			auth.WriteError(w, r, &auth.Error{Status: http.StatusServiceUnavailable, Key: "generation_pending", Message: "authentication generation unavailable", RetryAfter: time.Second})
			return
		}
		verify.ServeHTTP(w, r)
	})
}

func (a *Adapter) selectCredential(r *http.Request) (servingauth.Credential, error) {
	headers := r.Header.Values("Authorization")
	if len(headers) != 0 {
		if len(headers) != 1 || len(headers[0]) > oidccore.MaxTokenBytes+len("Bearer ") {
			return servingauth.Credential{}, servingauth.ErrCredentials
		}
		parts := strings.Fields(headers[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return servingauth.Credential{}, servingauth.ErrUnsupported
		}
		if strings.HasPrefix(parts[1], "kdr_") {
			if len(headers[0]) > 1024 {
				return servingauth.Credential{}, servingauth.ErrCredentials
			}
			return servingauth.Credential{Kind: servingauth.APIToken, Value: parts[1]}, nil
		}
		if len(parts[1]) > oidccore.MaxTokenBytes || strings.Count(parts[1], ".") != 2 {
			return servingauth.Credential{}, servingauth.ErrUnsupported
		}
		// Shape selects a verifier, never an identity or authority. The exact
		// captured auth owner verifies signature, issuer, audience and policy.
		return servingauth.Credential{Kind: servingauth.OIDCJWT, Value: parts[1]}, nil
	}
	cookies := r.CookiesNamed(a.options.Cookie.NameFor(r))
	if len(cookies) == 0 {
		return servingauth.Credential{}, auth.ErrNoCredentials
	}
	if len(cookies) != 1 || cookies[0].Value == "" || len(cookies[0].Value) > session.MaxCookieBytes {
		return servingauth.Credential{}, servingauth.ErrCredentials
	}
	return servingauth.Credential{Kind: servingauth.Cookie, Value: cookies[0].Value}, nil
}

func (a *Adapter) authenticate(_ http.ResponseWriter, r *http.Request) (auth.Principal, *http.Request, error) {
	credential, err := a.selectCredential(r)
	if err != nil {
		return auth.Principal{}, nil, err
	}
	view, err := Component[*servingauth.View](r.Context(), "auth")
	if err != nil {
		return auth.Principal{}, nil, err
	}
	now := a.options.Now()
	var principal auth.Principal
	if credential.Kind == servingauth.Cookie {
		sess, err := view.CookieSession(credential.Value, now)
		if err != nil {
			return auth.Principal{}, nil, err
		}
		if err := auth.CheckCSRF(r, sess.CSRF, a.options.CrossOriginProtection); err != nil {
			return auth.Principal{}, nil, err
		}
		principal = sess.Principal
		r = r.WithContext(auth.WithSession(r.Context(), sess))
	} else {
		principal, err = view.Authenticate(r.Context(), credential, now)
		if err != nil {
			return auth.Principal{}, nil, err
		}
	}
	lease, _ := r.Context().Value(leaseKey{}).(*serving.Lease)
	r = r.WithContext(context.WithValue(r.Context(), proofKey{}, proof{lease, credential}))
	return principal, r, nil
}

// Authorize implements rbac.Authorizer for reads. It rechecks the retained raw
// credential against the SAME view and current time; the caller's Principal
// cannot replace the one derived from the private request proof. Mutations and
// dedup responses still require servingauth.ReauthorizeCurrent in CommandTx.
func (a *Adapter) Authorize(ctx context.Context, _ rbac.Principal, attrs rbac.Attributes) (rbac.Decision, error) {
	credential, err := Credential(ctx)
	if err != nil {
		return rbac.Decision{}, err
	}
	view, err := Component[*servingauth.View](ctx, "auth")
	if err != nil {
		return rbac.Decision{}, err
	}
	now := a.options.Now()
	principal, err := view.Authenticate(ctx, credential, now)
	if err != nil {
		return rbac.Decision{}, err
	}
	d, err := view.Authorize(principal, attrs, now)
	if errors.Is(err, servingauth.ErrDenied) {
		return d, nil // auth.Require renders the established 403 contract.
	}
	return d, err
}

var _ rbac.Authorizer = (*Adapter)(nil)
