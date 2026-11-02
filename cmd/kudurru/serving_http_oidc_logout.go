package main

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/browseroidc"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
)

func (h *servingHTTP) clearLogoutCookies(w http.ResponseWriter, r *http.Request) error {
	for _, suffix := range []string{"", "_oidc_hint", "_oidc_logout", "_oidc_logout_state", "_oidc"} {
		if err := h.clearCookie(w, r, suffix); err != nil {
			return err
		}
	}
	return nil
}

func browserLogoutBinding(i providerresources.BrowserInfo) browseroidc.LogoutBinding {
	return browseroidc.LogoutBinding{Provider: i.Provider.ID, ProviderUID: i.Provider.UID, Origin: string(i.Provider.Origin), SpecIdentity: i.SpecIdentity, ResourceIdentity: i.ResourceIdentity, Issuer: i.Issuer, ClientID: i.ClientID, RedirectURI: i.RedirectURI}
}
func (h *servingHTTP) openLogout(r *http.Request, kind, phase string) (*servingauth.View, browseroidc.Logout, error) {
	// Bound aggregate cookie input BEFORE net/http parses individual names.
	size := 0
	lines := r.Header.Values("Cookie")
	if len(lines) > 16 {
		return nil, browseroidc.Logout{}, servingBrowserInvalid()
	}
	for _, line := range lines {
		size += len(line)
		if size > 16<<10 {
			return nil, browseroidc.Logout{}, servingBrowserInvalid()
		}
	}
	cookie, err := h.cookiePolicy(r, "_oidc_"+kind)
	if err != nil {
		return nil, browseroidc.Logout{}, err
	}
	cookies := r.CookiesNamed(cookie.NameFor(r))
	if len(cookies) != 1 || len(cookies[0].Value) > browseroidc.MaxCookieBytes {
		return nil, browseroidc.Logout{}, servingBrowserInvalid()
	}
	v, err := servinghttp.Component[*servingauth.View](r.Context(), "auth")
	if err != nil {
		return nil, browseroidc.Logout{}, err
	}
	l, err := v.OpenBrowserLogout(cookies[0].Value, h.now())
	if err != nil || l.Phase() != phase {
		return nil, browseroidc.Logout{}, servingBrowserInvalid()
	}
	return v, l, nil
}

func (h *servingHTTP) logoutBrowser(w http.ResponseWriter, r *http.Request) error {
	// Remote planning grants no authority and must NEVER prevent an otherwise
	// authorized local revocation. No provider I/O, retries or successor capture.
	v, hint, hintErr := h.openLogout(r, "hint", "hint")
	var browser *providerresources.Browser
	var continuation browseroidc.Logout
	if hintErr == nil {
		proof, err := servinghttp.Credential(r.Context())
		original, ok := auth.SessionFrom(r.Context())
		if err == nil && proof.Kind == servingauth.Cookie && ok && original.ID == hint.SessionID() {
			_, candidate, err := h.capturedBrowser(r.Context(), hint.Binding().Provider)
			if err == nil && browserLogoutBinding(candidate.Info()) == hint.Binding() && candidate.Info().EndSessionURL != "" {
				continuation, err = hint.Continue(h.now())
				if err == nil {
					if _, err = continuation.RedirectURL(candidate.Info().EndSessionURL, h.now()); err == nil {
						browser = candidate
					}
				}
			}
		}
	}
	if err := h.commitLogout(r); err != nil {
		return err
	}
	if err := h.clearLogoutCookies(w, r); err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	out := api.BrowserLogoutBody{}
	if browser != nil && browser.Current() && continuation.ValidAt(h.now()) == nil {
		sealed, err := v.SealBrowserLogout(continuation, h.now())
		callback, _, targetErr := browseroidc.LogoutTargets(continuation.Binding())
		if err == nil && targetErr == nil && browser.Current() && continuation.ValidAt(h.now()) == nil {
			target, _ := url.Parse(callback)
			out.ContinuePath = strings.TrimSuffix(target.Path, "/callback")
			if err := h.setCookie(w, r, "_oidc_logout", sealed, continuation.ExpiresAt().Sub(h.now())); err != nil {
				return err
			}
		}
	}
	return servingHTTPJSON(w, 200, out)
}

func (b *servingBrowserHTTP) logout(w http.ResponseWriter, r *http.Request) error {
	cookie, err := b.h.cookiePolicy(r, "_oidc_logout")
	if err != nil {
		return err
	}
	cookie.Clear(w, r)
	if r.URL.RawQuery != "" {
		return servingBrowserInvalid()
	}
	v, l, err := b.h.openLogout(r, "logout", "continue")
	if err != nil || l.Binding().Provider != r.PathValue("idp") {
		return servingBrowserInvalid()
	}
	_, local, err := browseroidc.LogoutTargets(l.Binding())
	if err != nil {
		return servingBrowserInvalid()
	}
	// A provider cut that changed after revocation cannot redirect with the old
	// hint. Local logout already committed; returning to the app is always safe.
	_, browser, err := b.h.capturedBrowser(r.Context(), l.Binding().Provider)
	if err != nil || browserLogoutBinding(browser.Info()) != l.Binding() {
		w.Header().Set("Location", local)
		w.WriteHeader(302)
		return nil
	}
	target, err := browser.LogoutRedirect(r.Context(), l)
	if err != nil {
		w.Header().Set("Location", local)
		w.WriteHeader(302)
		return nil
	}
	state, err := l.Callback(b.h.now())
	if err != nil {
		return servingBrowserInvalid()
	}
	sealed, err := v.SealBrowserLogout(state, b.h.now())
	if err != nil || !browser.Current() || state.ValidAt(b.h.now()) != nil {
		w.Header().Set("Location", local)
		w.WriteHeader(302)
		return nil
	}
	if err := b.h.setCookie(w, r, "_oidc_logout_state", sealed, state.ExpiresAt().Sub(b.h.now())); err != nil {
		return err
	}
	w.Header().Set("Location", target)
	w.WriteHeader(302)
	return nil
}

func (b *servingBrowserHTTP) logoutCallback(w http.ResponseWriter, r *http.Request) error {
	if err := b.h.clearCookie(w, r, "_oidc_logout_state"); err != nil {
		return err
	}
	if len(r.URL.RawQuery) > 4096 {
		return servingBrowserInvalid()
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(q) > 3 {
		return servingBrowserInvalid()
	}
	for key, values := range q {
		if len(values) != 1 || len(values[0]) > 1024 || key != "state" && key != "error" && key != "error_description" {
			return servingBrowserInvalid()
		}
	}
	_, state, err := b.h.openLogout(r, "logout_state", "callback")
	if err != nil || state.Binding().Provider != r.PathValue("idp") || !state.MatchState(q.Get("state")) {
		return servingBrowserInvalid()
	}
	_, local, err := browseroidc.LogoutTargets(state.Binding())
	if err != nil {
		return servingBrowserInvalid()
	}
	// Even a valid provider error has no effect on local revocation. Ignore its
	// diagnostics; never follow provider-supplied URLs or issue a new session.
	w.Header().Set("Location", local)
	w.WriteHeader(302)
	return nil
}
