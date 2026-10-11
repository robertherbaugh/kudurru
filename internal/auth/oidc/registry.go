package oidc

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Registry holds the configured providers (auth.identityProviders[]) and
// mounts their routes.
type Registry struct {
	byID  map[string]*Provider
	order []*Provider
}

// NewRegistry rejects duplicate provider IDs and duplicate issuers: bearer
// ID tokens (v0.2 CLI) are routed by iss, so issuers must be unique.
func NewRegistry(providers ...*Provider) (*Registry, error) {
	r := &Registry{byID: map[string]*Provider{}}
	issuers := map[string]string{}
	for _, p := range providers {
		if _, dup := r.byID[p.ID()]; dup {
			return nil, fmt.Errorf("oidc: duplicate provider id %q", p.ID())
		}
		if other, dup := issuers[p.cfg.Issuer]; dup {
			return nil, fmt.Errorf("oidc: providers %q and %q share issuer %s", other, p.ID(), p.cfg.Issuer)
		}
		r.byID[p.ID()] = p
		issuers[p.cfg.Issuer] = p.ID()
		r.order = append(r.order, p)
	}
	return r, nil
}

// ProviderInfo is what the login page lists.
type ProviderInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

// Providers lists providers in configuration order.
func (r *Registry) Providers() []ProviderInfo {
	out := make([]ProviderInfo, 0, len(r.order))
	for _, p := range r.order {
		out = append(out, ProviderInfo{ID: p.ID(), DisplayName: p.DisplayName()})
	}
	return out
}

// Provider returns the provider with id.
func (r *Registry) Provider(id string) (*Provider, bool) {
	p, ok := r.byID[id]
	return p, ok
}

// Register mounts GET <prefix>/auth/oidc/{idp}/start and .../callback on
// mux (prefix is "/api/v1" in the API server). It fails if a provider's
// redirectURL does not point at its callback route, the most common
// misconfiguration.
func (r *Registry) Register(mux *http.ServeMux, prefix string) error {
	prefix = strings.TrimSuffix(prefix, "/")
	var errs []error
	for _, p := range r.order {
		u, _ := url.Parse(p.cfg.RedirectURL)
		if want := prefix + "/auth/oidc/" + p.ID() + "/callback"; u.Path != want {
			errs = append(errs, fmt.Errorf("oidc provider %q: redirectURL path %q must be %q", p.ID(), u.Path, want))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	mux.HandleFunc("GET "+prefix+"/auth/oidc/{idp}/start", r.route((*Provider).Start))
	mux.HandleFunc("GET "+prefix+"/auth/oidc/{idp}/callback", r.route((*Provider).Callback))
	return nil
}

func (r *Registry) route(h func(*Provider, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		p, ok := r.byID[req.PathValue("idp")]
		if !ok {
			http.NotFound(w, req)
			return
		}
		h(p, w, req)
	}
}
