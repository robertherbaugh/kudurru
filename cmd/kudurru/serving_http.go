package main

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
)

type servingHTTPOptions struct {
	Runtime     *servingRuntime
	Cookie      session.CookieConfig
	CrossOrigin *http.CrossOriginProtection
	ClientIP    func(*http.Request) string
	Hasher      *users.Hasher
	RoutePrefix string
	MaxDuration time.Duration
	// ProviderHandler is an explicitly captured provider/browser adapter, never
	// the legacy live Stack. Missing capability is reported as unavailable.
	ProviderHandler http.Handler
}

type servingHTTP struct {
	factorMu           sync.Mutex
	permissions        *rbac.Permissions
	factorLimit        *users.Limiter
	opts               servingHTTPOptions
	adapter            *servinghttp.Adapter
	mux                *http.ServeMux
	ipLimit, userLimit *users.Limiter
}

func newServingHTTP(o servingHTTPOptions) (*servingHTTP, error) {
	if o.Runtime == nil || o.Runtime.publisher == nil || o.Hasher == nil {
		return nil, serving.ErrComponents
	}
	if o.RoutePrefix == "" {
		o.RoutePrefix = api.BasePath
	}
	if !strings.HasPrefix(o.RoutePrefix, "/") || strings.HasSuffix(o.RoutePrefix, "/") {
		return nil, serving.ErrComponents
	}
	if err := o.Cookie.Validate(); err != nil {
		return nil, err
	}
	if o.CrossOrigin == nil {
		o.CrossOrigin = new(http.CrossOriginProtection)
	}
	a, err := servinghttp.New(servinghttp.Options{Publisher: o.Runtime.publisher, Cookie: o.Cookie, CrossOriginProtection: o.CrossOrigin, MaxDuration: o.MaxDuration, CaptureWait: 2 * time.Second, Now: o.Runtime.opts.Now})
	if err != nil {
		return nil, err
	}
	h := &servingHTTP{opts: o, adapter: a, mux: http.NewServeMux(), ipLimit: users.NewLimiter(users.DefaultIPLimit, o.Runtime.opts.Now), userLimit: users.NewLimiter(users.DefaultUserLimit, o.Runtime.opts.Now)}
	h.permissions, err = rbac.LoadPermissions()
	if err != nil {
		return nil, err
	}
	h.factorLimit = users.NewLimiter(users.LimitPolicy{Failures: 1, Window: time.Second, Lockout: time.Second}, o.Runtime.opts.Now)
	h.register()
	h.registerTOTP()
	h.registerLocalUsers()
	return h, nil
}
func (h *servingHTTP) Middleware(next http.Handler) http.Handler { return h.adapter.Middleware(next) }
func (h *servingHTTP) Handler() http.Handler                     { return h.mux }
func (h *servingHTTP) Authorizer() rbac.Authorizer               { return h.adapter }
func (h *servingHTTP) Roles(ctx context.Context, _ auth.Principal) []string {
	v, p, err := h.readPrincipal(ctx)
	if err != nil {
		return []string{}
	}
	r, err := v.Roles(p, h.opts.Runtime.opts.Now())
	if err != nil {
		return []string{}
	}
	return r
}
func (h *servingHTTP) readPrincipal(ctx context.Context) (*servingauth.View, auth.Principal, error) {
	v, err := servinghttp.Component[*servingauth.View](ctx, "auth")
	if err != nil {
		return nil, auth.Principal{}, err
	}
	proof, err := servinghttp.Credential(ctx)
	if err != nil {
		return nil, auth.Principal{}, err
	}
	p, err := v.Authenticate(ctx, proof, h.opts.Runtime.opts.Now())
	return v, p, err
}
