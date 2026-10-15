package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/browseroidc"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// servingBrowserHTTP has only bounded admission state. Durable pending attempts
// and current authorization belong to security, not this adapter's limiters.
type servingBrowserHTTP struct {
	h                           *servingHTTP
	mu                          sync.Mutex
	starts, callbacks, attempts *users.Limiter
	active                      chan struct{}
}

// CapturedBrowserHandler returns the explicit browser adapter for root to install
// as ProviderHandler BEFORE mounting. It uses only the original complete HTTP
// lease; absence of a ready browser capability never falls back to live Stack.
func (h *servingHTTP) CapturedBrowserHandler() http.Handler {
	b := &servingBrowserHTTP{h: h, active: make(chan struct{}, 4)}
	b.starts = users.NewLimiter(users.LimitPolicy{Failures: 5, Window: time.Minute, Lockout: time.Minute}, h.now)
	b.callbacks = users.NewLimiter(users.LimitPolicy{Failures: 10, Window: time.Minute, Lockout: time.Minute}, h.now)
	b.attempts = users.NewLimiter(users.LimitPolicy{Failures: 1, Window: browseroidc.Lifetime, Lockout: browseroidc.Lifetime}, h.now)
	return b
}

func (b *servingBrowserHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	var err error
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/start"):
		err = b.start(w, r)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/callback"):
		err = b.callback(w, r)
	default:
		err = servingHTTPMissing()
	}
	if err != nil {
		servingHTTPError(w, r, err)
	}
}

func (b *servingBrowserHTTP) admit(r *http.Request, callback bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	limit := b.starts
	if callback {
		limit = b.callbacks
	}
	key := b.h.clientKey(r)
	delay := limit.Locked(key)
	if delay > 0 {
		return &auth.Error{Status: 429, Key: "rate_limited", Message: "too many browser login attempts; start again later", RetryAfter: delay}
	}
	limit.Fail(key)
	return nil
}

func (b *servingBrowserHTTP) admitAttempt(attempt string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if delay := b.attempts.Locked(attempt); delay > 0 {
		return &auth.Error{Status: 429, Key: "rate_limited", Message: "browser login was already attempted; start again", RetryAfter: delay}
	}
	b.attempts.Fail(attempt)
	return nil
}

func (h *servingHTTP) browserCookie() session.CookieConfig {
	c := h.opts.Cookie
	// Keep the same secure/path/__Host policy, but never accept Stack's legacy
	// preauth name or confuse the enrolled-session cookie with pending state.
	c.Name += "_oidc"
	return c
}

func servingBrowserInvalid() error {
	return &auth.Error{Status: 401, Key: "login_state_invalid", Message: "browser login expired or was rejected; start again"}
}
func servingBrowserUnavailable() error {
	return &auth.Error{Status: 503, Key: "unavailable", Message: "captured browser provider login unavailable"}
}

func (h *servingHTTP) capturedBrowser(ctx context.Context, id string) (*servingauth.View, *providerresources.Browser, error) {
	v, err := servinghttp.Component[*servingauth.View](ctx, "auth")
	if err != nil {
		return nil, nil, err
	}
	p, err := v.ProviderInfo(id)
	if err != nil || p.Availability != servingauth.ProviderReady {
		return nil, nil, servingBrowserUnavailable()
	}
	owner, err := servinghttp.Component[*servingbuild.RuntimeOwner](ctx, "runtime")
	if err != nil {
		return nil, nil, servingBrowserUnavailable()
	}
	browser, err := owner.Browser(p.Descriptor.Ref, p.ResourceIdentity)
	if err != nil {
		return nil, nil, servingBrowserUnavailable()
	}
	i := browser.Info()
	if i.Provider != p.Descriptor.Ref || i.SpecIdentity != p.Descriptor.SpecIdentity || i.ResourceIdentity != p.ResourceIdentity || i.Issuer != p.Descriptor.Issuer || i.ClientID != p.Descriptor.Spec.ClientID || i.RedirectURI != p.Descriptor.Spec.RedirectURL || !browser.Current() {
		return nil, nil, servingBrowserUnavailable()
	}
	return v, browser, nil
}

func browserQuery(r *http.Request, callback bool) (url.Values, error) {
	if len(r.URL.RawQuery) > 16<<10 {
		return nil, servingHTTPInvalid("browser login query is too large")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(values) > 8 {
		return nil, servingHTTPInvalid("invalid browser login query")
	}
	for key, v := range values {
		if len(key) > 64 || len(v) != 1 || len(v[0]) > browseroidc.MaxCodeBytes {
			return nil, servingHTTPInvalid("invalid browser login query")
		}
		if !callback && key != "next" {
			return nil, servingHTTPInvalid("invalid browser login query")
		}
		if callback {
			switch key {
			case "code", "state", "iss", "session_state", "error", "error_description", "error_uri":
			default:
				return nil, servingHTTPInvalid("invalid browser login query")
			}
		}
	}
	return values, nil
}

func browserAttempt(s browseroidc.State, i providerresources.BrowserInfo) servingauth.BrowserAttempt {
	return servingauth.BrowserAttempt{FormatVersion: servingauth.FormatVersion, ID: s.ID(), Provider: i.Provider, SpecIdentity: i.SpecIdentity, ResourceIdentity: i.ResourceIdentity,
		ClientID: i.ClientID, RedirectURI: i.RedirectURI, IssuedAt: s.IssuedAt(), ExpiresAt: s.ExpiresAt(), PayloadHash: s.PayloadHash()}
}

func browserAttemptMatches(a servingauth.BrowserAttempt, s browseroidc.State, i providerresources.BrowserInfo) bool {
	expected := browserAttempt(s, i)
	return a.FormatVersion == expected.FormatVersion && a.ID == expected.ID && a.Provider == expected.Provider && a.Provider.ID == s.Provider() && a.SpecIdentity == expected.SpecIdentity && a.ResourceIdentity == expected.ResourceIdentity && a.ClientID == expected.ClientID && a.RedirectURI == expected.RedirectURI && a.IssuedAt.Equal(expected.IssuedAt) && a.ExpiresAt.Equal(expected.ExpiresAt) && subtle.ConstantTimeCompare([]byte(a.PayloadHash), []byte(expected.PayloadHash)) == 1
}

func browserAuthorizationURL(i providerresources.BrowserInfo, s browseroidc.State) (string, error) {
	u, err := url.Parse(i.AuthorizationURL)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.Scheme != "https" && u.Scheme != "http" {
		return "", servingBrowserUnavailable()
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", servingBrowserUnavailable()
	}
	for _, key := range []string{"response_type", "response_mode", "client_id", "redirect_uri", "scope", "state", "nonce", "code_challenge", "code_challenge_method", "request", "request_uri", "client_secret"} {
		if _, exists := q[key]; exists {
			return "", servingBrowserUnavailable()
		}
	}
	challenge, err := browseroidc.S256(s.Verifier())
	if err != nil {
		return "", err
	}
	for _, scope := range i.Scopes {
		if scope == "" {
			return "", servingBrowserUnavailable()
		}
		for _, c := range []byte(scope) {
			if c < 0x21 || c > 0x7e || c == '"' || c == '\\' {
				return "", servingBrowserUnavailable()
			}
		}
	}
	q.Set("response_type", "code")
	q.Set("client_id", i.ClientID)
	q.Set("redirect_uri", i.RedirectURI)
	q.Set("scope", strings.Join(i.Scopes, " "))
	q.Set("state", s.OAuthState())
	q.Set("nonce", s.Nonce())
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	if len(u.String()) > 16<<10 {
		return "", servingBrowserUnavailable()
	}
	return u.String(), nil
}

func (b *servingBrowserHTTP) start(w http.ResponseWriter, r *http.Request) error {
	if err := b.h.opts.CrossOrigin.Check(r); err != nil {
		return &auth.Error{Status: 403, Key: "csrf", Message: "cross-origin login rejected"}
	}
	if err := b.admit(r, false); err != nil {
		return err
	}
	query, err := browserQuery(r, false)
	if err != nil {
		return err
	}
	_, browser, err := b.h.capturedBrowser(r.Context(), r.PathValue("idp"))
	if err != nil {
		return err
	}
	state, err := browseroidc.New(r.PathValue("idp"), query.Get("next"), b.h.now())
	if err != nil {
		return servingHTTPInvalid("invalid browser login destination")
	}
	info := browser.Info()
	redirect, err := browserAuthorizationURL(info, state)
	if err != nil {
		return err
	}
	attempt := browserAttempt(state, info)
	err = b.h.opts.Runtime.currentAuthCommand(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		if !browser.Current() {
			return servingBrowserUnavailable()
		}
		return security.BeginBrowser(ctx, tx, deps, attempt, b.h.now)
	})
	if err != nil {
		return err
	}
	var raw string
	err = b.h.browserPublication(r.Context(), func(v *servingauth.View) (bool, error) {
		if !browser.Current() || state.ValidAt(b.h.now()) != nil {
			return false, servingBrowserUnavailable()
		}
		row, err := v.BrowserAttempt(state.ID(), b.h.now())
		if err != nil || !browserAttemptMatches(row, state, info) {
			return false, nil
		}
		raw, err = v.SealBrowserPreauth(state, b.h.now())
		return err == nil, err
	})
	if err != nil {
		return err
	}
	if !browser.Current() || state.ValidAt(b.h.now()) != nil {
		return servingBrowserUnavailable()
	}
	b.h.browserCookie().Set(w, r, raw, state.ExpiresAt().Sub(b.h.now()))
	w.Header().Set("Location", redirect)
	w.WriteHeader(http.StatusFound)
	return nil
}

func (b *servingBrowserHTTP) callback(w http.ResponseWriter, r *http.Request) error {
	cookie := b.h.browserCookie()
	cookie.Clear(w, r)
	if err := b.admit(r, true); err != nil {
		return err
	}
	query, err := browserQuery(r, true)
	if err != nil {
		return err
	}
	// Bound aggregate Cookie bytes before net/http allocates parsed cookies.
	bytes := 0
	lines := r.Header.Values("Cookie")
	if len(lines) > 16 {
		return servingBrowserInvalid()
	}
	for _, line := range lines {
		bytes += len(line)
		if bytes > 16<<10 {
			return servingBrowserInvalid()
		}
	}
	cookies := r.CookiesNamed(cookie.NameFor(r))
	if len(cookies) != 1 || len(cookies[0].Value) > browseroidc.MaxCookieBytes {
		return servingBrowserInvalid()
	}
	v, err := servinghttp.Component[*servingauth.View](r.Context(), "auth")
	if err != nil {
		return err
	}
	state, err := v.OpenBrowserPreauth(cookies[0].Value, b.h.now())
	if err != nil || state.Provider() != r.PathValue("idp") || !state.MatchState(query.Get("state")) {
		return servingBrowserInvalid()
	}
	if _, exists := query["error"]; exists {
		return servingBrowserInvalid()
	}
	if query.Get("code") == "" {
		return servingBrowserInvalid()
	}
	_, browser, err := b.h.capturedBrowser(r.Context(), state.Provider())
	if err != nil {
		return err
	}
	info := browser.Info()
	row, err := v.BrowserAttempt(state.ID(), b.h.now())
	if err != nil || !browserAttemptMatches(row, state, info) || query.Get("iss") != "" && query.Get("iss") != info.Issuer {
		return servingBrowserInvalid()
	}
	if err = b.admitAttempt(state.ID()); err != nil {
		return err
	}
	select {
	case b.active <- struct{}{}:
		defer func() { <-b.active }()
	default:
		return servingBrowserUnavailable()
	}
	// The only provider I/O is explicit and outside ALL command/owner gates.
	tokens, err := browser.Exchange(r.Context(), query.Get("code"), state.Verifier())
	if err != nil {
		return servingBrowserUnavailable()
	}
	var enrollment security.Enrollment
	err = b.h.opts.Runtime.currentAuthCommand(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		if !browser.Current() {
			return servingBrowserUnavailable()
		}
		var err error
		enrollment, err = security.IssueBrowser(ctx, tx, deps, row, tokens.LoginProof(state.Nonce()), b.h.now)
		return err
	})
	if err != nil {
		return err
	}
	var raw string
	var expires time.Time
	err = b.h.browserPublication(r.Context(), func(view *servingauth.View) (bool, error) {
		if !browser.Current() {
			return false, servingBrowserUnavailable()
		}
		sealed, sess, err := enrollment.Seal(view, b.h.now())
		if err != nil {
			return false, nil
		}
		raw, expires = sealed, sess.ExpiresAt
		return true, nil
	})
	if err != nil {
		return err
	}
	if !browser.Current() || !b.h.now().Before(expires) {
		return servingBrowserUnavailable()
	}
	b.h.opts.Cookie.Set(w, r, raw, expires.Sub(b.h.now()))
	w.Header().Set("Location", state.Next())
	w.WriteHeader(http.StatusFound)
	return nil
}

// Postcommit publication is the only permitted fresh capture in this flow. It
// seals an already committed exact row/enrollment; it cannot authorize commands,
// exchange codes, select successor resources, or replace the original HTTP lease.
func (h *servingHTTP) browserPublication(ctx context.Context, ready func(*servingauth.View) (bool, error)) error {
	h.opts.Runtime.publisher.Wake()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		changed := h.opts.Runtime.publisher.Subscribe()
		lease, err := h.opts.Runtime.publisher.CaptureHTTP(ctx)
		if err == nil {
			value, ok := lease.Bundle().Component("auth")
			v, typed := value.(*servingauth.View)
			if ok && typed {
				done, err := ready(v)
				lease.Release()
				if err != nil {
					return err
				}
				if done {
					return nil
				}
			} else {
				lease.Release()
				return serving.ErrComponents
			}
		} else if !errors.Is(err, serving.ErrPending) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
