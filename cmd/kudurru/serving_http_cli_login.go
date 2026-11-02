package main

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/clioidc"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
)

// cliPreparationAdmission counts requests, not authentication failures. Keys
// come only from clientKey's normalized IP/prefix; the existing limiter bounds
// retained keys. Serialize check+charge so concurrent requests share the cap.
type cliPreparationAdmission struct {
	mu    sync.Mutex
	limit *users.Limiter
}

func (a *cliPreparationAdmission) admit(key string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if d := a.limit.Locked(key); d > 0 {
		return &auth.Error{Status: 429, Key: "rate_limited", Message: "too many CLI preparations; try again later", RetryAfter: d}
	}
	a.limit.Fail(key)
	return nil
}
func (h *servingHTTP) registerCLILogin() {
	h.cliPrepareLimit = &cliPreparationAdmission{limit: users.NewLimiter(users.LimitPolicy{Failures: 5, Window: time.Minute, Lockout: time.Minute}, h.now)}
	h.route("GET", "/auth/cli/providers", h.cliProviders)
	h.route("POST", "/auth/cli/{idp}/prepare", h.cliPrepare)
	h.route("POST", "/auth/cli/{idp}/verify", h.cliVerify)
}
func (h *servingHTTP) capturedCLI(ctx context.Context, id string) (*servingauth.View, *providerresources.CLI, error) {
	v, e := servinghttp.Component[*servingauth.View](ctx, "auth")
	if e != nil {
		return nil, nil, e
	}
	p, e := v.ProviderInfo(id)
	if e != nil || p.Availability != servingauth.ProviderReady {
		return nil, nil, servingCLIUnavailable()
	}
	runtime, e := servinghttp.Component[*servingbuild.RuntimeOwner](ctx, "runtime")
	if e != nil {
		return nil, nil, e
	}
	c, e := runtime.CLI(p.Descriptor.Ref, p.ResourceIdentity)
	if e != nil {
		return nil, nil, servingCLIUnavailable()
	}
	return v, c, nil
}
func (h *servingHTTP) cliProviders(w http.ResponseWriter, r *http.Request) error {
	v, e := servinghttp.Component[*servingauth.View](r.Context(), "auth")
	if e != nil {
		return e
	}
	out := []api.CLILoginProviderBody{}
	for _, p := range h.providerInfos(v) {
		_, c, e := h.capturedCLI(r.Context(), p.ID)
		if e != nil {
			continue
		}
		i := c.Info()
		out = append(out, api.CLILoginProviderBody{ID: p.ID, DisplayName: p.DisplayName, Browser: i.Browser, Device: i.Device})
	}
	return servingHTTPList(w, r, out)
}
func (h *servingHTTP) cliPrepare(w http.ResponseWriter, r *http.Request) error {
	if e := h.opts.CrossOrigin.Check(r); e != nil {
		return servingCLIInvalid()
	}
	if e := h.cliPrepareLimit.admit(h.clientKey(r)); e != nil {
		return e
	}
	var q clioidc.PrepareRequest
	if e := servingHTTPDecode(w, r, &q); e != nil {
		return e
	}
	if q.Validate() != nil {
		return servingHTTPInvalid("invalid public CLI login request")
	}
	v, c, e := h.capturedCLI(r.Context(), r.PathValue("idp"))
	if e != nil {
		return e
	}
	if e = c.Check(r.Context()); e != nil {
		return servingCLIUnavailable()
	}
	i := c.Info()
	if q.Mode == "browser" && !i.Browser || q.Mode == "device" && !i.Device {
		return servingCLIUnavailable()
	}
	raw, expiry, e := v.SealCLIReceipt(i.Source, q, h.now())
	if e != nil {
		return servingCLIInvalid()
	}
	if !c.Current() || r.Context().Err() != nil {
		return servingCLIUnavailable()
	}
	return servingHTTPJSON(w, 200, clioidc.Preparation{Provider: i.Source.Provider, DisplayName: i.DisplayName, Issuer: i.Source.Issuer, ClientID: i.Source.ClientID, AuthorizationURL: i.AuthorizationURL, TokenURL: i.TokenURL, DeviceURL: i.DeviceURL, Scopes: i.Scopes, Receipt: raw, ExpiresAt: expiry})
}
func (h *servingHTTP) cliVerify(w http.ResponseWriter, r *http.Request) error {
	var q clioidc.VerifyRequest
	if e := servingHTTPDecode(w, r, &q); e != nil {
		return e
	}
	if len(q.AccessToken) > 32<<10 {
		return servingHTTPInvalid("CLI proof exceeds bounds")
	}
	v, c, e := h.capturedCLI(r.Context(), r.PathValue("idp"))
	if e != nil {
		return e
	}
	receipt, e := v.OpenCLIReceipt(q.Receipt, h.now())
	if e != nil || receipt.Source() != c.Info().Source {
		return servingCLIInvalid()
	}
	proof, e := servinghttp.Credential(r.Context())
	if e != nil {
		return e
	}
	if e = c.Check(r.Context()); e != nil {
		return servingCLIUnavailable()
	}
	observed := h.now()
	p, expiry, e := v.VerifyCLILogin(r.Context(), proof, receipt, q.AccessToken, observed)
	if e != nil {
		return servingCLIInvalid()
	}
	me, e := h.sessionInfo(v, p, nil)
	if e != nil {
		return e
	}
	now := h.now()
	if !c.Current() || now.Before(observed) || receipt.ValidAt(now) != nil || !now.Before(expiry) || r.Context().Err() != nil {
		return servingCLIInvalid()
	}
	return servingHTTPJSON(w, 200, api.CLILoginVerifiedBody{Me: me, ExpiresAt: expiry})
}

func servingCLIInvalid() error {
	return &auth.Error{Status: 401, Key: "cli_login_invalid", Message: "public CLI login proof expired or was rejected; start again"}
}
func servingCLIUnavailable() error {
	return &auth.Error{Status: 503, Key: "unavailable", Message: "public CLI provider unavailable"}
}
