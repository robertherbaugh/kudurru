package oidc

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/rbac"
)

// PreAuthLifetime bounds the time between Start and Callback.
const PreAuthLifetime = 10 * time.Minute

// discoveryRetry is the minimum gap between failed discovery attempts, so a
// down IdP is not hammered by every login click.
const discoveryRetry = 10 * time.Second

// Options are the runtime dependencies of a Provider.
type Options struct {
	// Key and PreviousKey are KUDURRU_ENCRYPTION_KEY and
	// KUDURRU_ENCRYPTION_KEY_PREVIOUS; they seal the pre-auth cookie under
	// the oidc-preauth purpose.
	Key, PreviousKey []byte
	// Sessions issues the session cookie. Required.
	Sessions auth.SessionStore
	// SessionLifetime is the session cookie's Max-Age and the ExpiresAt
	// requested from Sessions. Default session.DefaultLifetime; match the
	// store's Lifetime.
	SessionLifetime time.Duration
	// Cookie is the session cookie configuration (shared with the
	// session.Authenticator). The pre-auth cookie copies its Secure mode.
	Cookie session.CookieConfig
	// Resolver maps the principal to grants. Required.
	Resolver RoleResolver
	// IdentityExists answers "is this identity known?" for
	// autoProvision: never. Required in that mode.
	IdentityExists func(ctx context.Context, key IdentityKey) (bool, error)
	// Permissions validates provisioning.defaultRole when set.
	Permissions *rbac.Permissions
	// HTTPClient talks to the IdP (discovery, JWKS, token). Default: a
	// client with a 15 s timeout.
	HTTPClient *http.Client
	// Logger receives login audit lines; default discards.
	Logger *slog.Logger
	// Now is the clock for pre-auth expiry, session times and ID token
	// expiry; default time.Now.
	Now func() time.Time
	// ErrorHandler renders refused logins; default DefaultErrorHandler.
	ErrorHandler ErrorHandler
}

// Provider is one configured OpenID Provider and its Start and Callback
// handlers. Discovery is lazy and retried, so kudurru starts (and serves
// DNS) while the IdP is down; logins fail with idp_unavailable until it
// answers. A Provider is safe for concurrent use.
type Provider struct {
	cfg           ProviderConfig
	opts          Options
	preauth       *session.Sealer
	preauthCookie session.CookieConfig

	mu       sync.Mutex
	md       *metadata
	lastFail time.Time
	lastErr  error

	bearer bearerCache
}

type metadata struct {
	oauth2   oauth2.Config
	verifier *gooidc.IDTokenVerifier
	// bearer verifies bearer ID tokens: same keys, audience checked by
	// checkBearerAudience.
	bearer *gooidc.IDTokenVerifier
}

// NewProvider validates cfg and opts. It does not contact the IdP; call
// Discover to fail fast at startup if desired.
func NewProvider(cfg ProviderConfig, opts Options) (*Provider, error) {
	if err := cfg.Validate(opts.Permissions); err != nil {
		return nil, err
	}
	if opts.Sessions == nil || opts.Resolver == nil {
		return nil, errors.New("oidc: Options.Sessions and Options.Resolver are required")
	}
	if cfg.Provisioning.AutoProvision == AutoProvisionNever && opts.IdentityExists == nil {
		return nil, fmt.Errorf("oidc provider %q: autoProvision: never needs Options.IdentityExists", cfg.ID)
	}
	if err := opts.Cookie.Validate(); err != nil {
		return nil, err
	}
	if opts.SessionLifetime <= 0 {
		opts.SessionLifetime = session.DefaultLifetime
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ErrorHandler == nil {
		opts.ErrorHandler = DefaultErrorHandler
	}
	sealer, err := session.NewSealer(session.PurposeOIDCPreAuth, opts.Key, opts.PreviousKey)
	if err != nil {
		return nil, err
	}
	redirect, _ := url.Parse(cfg.RedirectURL) // validated above
	return &Provider{
		cfg:     cfg,
		opts:    opts,
		preauth: sealer,
		preauthCookie: session.CookieConfig{
			Name: "kudurru_oidc_" + cfg.ID,
			// Scope the pre-auth cookie to the provider's callback
			// directory: it is never sent to the rest of the API.
			Path:                path.Dir(redirect.Path) + "/",
			Secure:              opts.Cookie.Secure,
			TrustForwardedProto: opts.Cookie.TrustForwardedProto,
		},
	}, nil
}

// ID returns the provider slug.
func (p *Provider) ID() string { return p.cfg.ID }

// DisplayName returns the login-page label.
func (p *Provider) DisplayName() string { return p.cfg.DisplayName }

// Config returns the validated configuration.
func (p *Provider) Config() ProviderConfig { return p.cfg }

// Discover performs OIDC discovery now (it otherwise happens on the first
// login) and caches the result.
func (p *Provider) Discover(ctx context.Context) error {
	_, err := p.metadata(ctx)
	return err
}

func (p *Provider) metadata(ctx context.Context) (*metadata, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.md != nil {
		return p.md, nil
	}
	now := p.opts.Now()
	if !p.lastFail.IsZero() && now.Sub(p.lastFail) < discoveryRetry {
		return nil, p.lastErr
	}
	dctx := gooidc.ClientContext(ctx, p.opts.HTTPClient)
	op, err := gooidc.NewProvider(dctx, p.cfg.Issuer)
	if err != nil {
		p.lastFail, p.lastErr = now, fmt.Errorf("oidc provider %q: discovery: %w", p.cfg.ID, err)
		return nil, p.lastErr
	}
	// JWKS refreshes outlive the request that triggered discovery.
	keyCtx := gooidc.ClientContext(context.WithoutCancel(ctx), p.opts.HTTPClient)
	p.md = &metadata{
		oauth2: oauth2.Config{
			ClientID:     p.cfg.ClientID,
			ClientSecret: p.cfg.ClientSecret,
			Endpoint:     op.Endpoint(),
			RedirectURL:  p.cfg.RedirectURL,
			Scopes:       p.cfg.Scopes,
		},
		verifier: op.VerifierContext(keyCtx, &gooidc.Config{ClientID: p.cfg.ClientID, Now: p.opts.Now}),
		bearer:   op.VerifierContext(keyCtx, &gooidc.Config{SkipClientIDCheck: true, Now: p.opts.Now}),
	}
	p.lastFail, p.lastErr = time.Time{}, nil
	return p.md, nil
}

// preAuthV1 is the sealed pre-auth cookie payload.
type preAuthV1 struct {
	V        int    `json:"v"`
	IDP      string `json:"idp"`
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	Next     string `json:"next"`
	Exp      int64  `json:"exp"`
}

// Start serves GET /api/v1/auth/oidc/{idp}/start?next=/path: it seals
// {state, nonce, PKCE verifier, next} into a 10-minute pre-auth cookie and
// redirects to the IdP's authorization endpoint with an S256 challenge and
// the nonce.
func (p *Provider) Start(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		p.fail(w, r, loginErr(http.StatusMethodNotAllowed, CodeInternal, "use GET", nil))
		return
	}
	md, err := p.metadata(r.Context())
	if err != nil {
		p.fail(w, r, loginErr(http.StatusServiceUnavailable, CodeIDPUnavailable, "the identity provider is unreachable; try again shortly", err))
		return
	}
	pa := preAuthV1{
		V:        1,
		IDP:      p.cfg.ID,
		State:    randomToken(32),
		Nonce:    randomToken(32),
		Verifier: oauth2.GenerateVerifier(),
		Next:     SafeNext(r.URL.Query().Get("next")),
		Exp:      p.opts.Now().Add(PreAuthLifetime).Unix(),
	}
	pt, err := json.Marshal(pa)
	if err != nil {
		p.fail(w, r, loginErr(http.StatusInternalServerError, CodeInternal, "could not start the login", err))
		return
	}
	p.preauthCookie.Set(w, r, p.preauth.Seal(pt), PreAuthLifetime)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, md.oauth2.AuthCodeURL(pa.State, gooidc.Nonce(pa.Nonce), oauth2.S256ChallengeOption(pa.Verifier)), http.StatusFound)
}

// Callback serves GET /api/v1/auth/oidc/{idp}/callback?code&state: it
// checks state against the pre-auth cookie, exchanges the code with the
// PKCE verifier, verifies the ID token (signature, iss, aud, exp via
// go-oidc; nonce, azp and at_hash here), runs the claim pipeline and the
// provisioning policy, resolves grants, issues the session cookie and
// redirects to the same-origin next path.
func (p *Provider) Callback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		p.fail(w, r, loginErr(http.StatusMethodNotAllowed, CodeInternal, "use GET", nil))
		return
	}
	s, next, lerr := p.callback(w, r)
	if lerr != nil {
		p.opts.Logger.WarnContext(r.Context(), "oidc login refused", "idp", p.cfg.ID, "code", lerr.Code, "reason", lerr.Error())
		p.fail(w, r, lerr)
		return
	}
	p.opts.Logger.InfoContext(r.Context(), "oidc login", "idp", p.cfg.ID, "principal", s.Principal.ID,
		"username", s.Principal.Name, "roles", s.RolesSnapshot)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, next, http.StatusFound) //nolint:gosec // G710: next comes from SafeNext (same-origin path only)
}

func (p *Provider) callback(w http.ResponseWriter, r *http.Request) (auth.Session, string, *LoginError) {
	ctx := r.Context()
	q := r.URL.Query()

	// The pre-auth cookie is single-use whatever happens next.
	ck, cookieErr := r.Cookie(p.preauthCookie.Name)
	p.preauthCookie.Clear(w, r)

	if e := q.Get("error"); e != "" {
		return auth.Session{}, "", loginErr(http.StatusUnauthorized, CodeIDPError,
			"the identity provider refused the login ("+sanitizeIDPError(e)+")", nil)
	}
	if cookieErr != nil {
		return auth.Session{}, "", loginErr(http.StatusBadRequest, CodeMissingPreAuth,
			"the login cookie is missing or expired; start the login again (the redirect URL must use the same host as the page you started from)", nil)
	}
	pt, _, err := p.preauth.Open(ck.Value)
	if err != nil {
		return auth.Session{}, "", loginErr(http.StatusBadRequest, CodeInvalidPreAuth, "the login cookie is invalid; start the login again", err)
	}
	var pa preAuthV1
	if err := json.Unmarshal(pt, &pa); err != nil || pa.V != 1 || pa.IDP != p.cfg.ID {
		return auth.Session{}, "", loginErr(http.StatusBadRequest, CodeInvalidPreAuth, "the login cookie is invalid; start the login again", err)
	}
	now := p.opts.Now()
	if now.Unix() >= pa.Exp {
		return auth.Session{}, "", loginErr(http.StatusBadRequest, CodeExpiredPreAuth, "the login took longer than 10 minutes; start it again", nil)
	}
	if !constantEqual(q.Get("state"), pa.State) {
		return auth.Session{}, "", loginErr(http.StatusBadRequest, CodeStateMismatch, "the login state does not match; start the login again", nil)
	}
	code := q.Get("code")
	if code == "" {
		return auth.Session{}, "", loginErr(http.StatusBadRequest, CodeMissingCode, "the identity provider returned no authorization code", nil)
	}

	md, err := p.metadata(ctx)
	if err != nil {
		return auth.Session{}, "", loginErr(http.StatusServiceUnavailable, CodeIDPUnavailable, "the identity provider is unreachable; try again shortly", err)
	}
	octx := gooidc.ClientContext(ctx, p.opts.HTTPClient)
	tok, err := md.oauth2.Exchange(octx, code, oauth2.VerifierOption(pa.Verifier))
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			return auth.Session{}, "", loginErr(http.StatusUnauthorized, CodeExchangeFailed, "the identity provider rejected the authorization code", err)
		}
		return auth.Session{}, "", loginErr(http.StatusBadGateway, CodeIDPUnavailable, "the identity provider's token endpoint is unreachable", err)
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		return auth.Session{}, "", loginErr(http.StatusUnauthorized, CodeMissingIDToken, "the identity provider returned no ID token (is the openid scope allowed for this client?)", nil)
	}
	idt, err := md.verifier.Verify(ctx, rawID)
	if err != nil {
		return auth.Session{}, "", loginErr(http.StatusUnauthorized, CodeInvalidIDToken, "the ID token failed verification", err)
	}
	// go-oidc leaves the nonce to the caller.
	if !constantEqual(idt.Nonce, pa.Nonce) {
		return auth.Session{}, "", loginErr(http.StatusUnauthorized, CodeNonceMismatch, "the ID token nonce does not match this login", nil)
	}
	if idt.AccessTokenHash != "" {
		if err := idt.VerifyAccessToken(tok.AccessToken); err != nil {
			return auth.Session{}, "", loginErr(http.StatusUnauthorized, CodeInvalidIDToken, "the ID token does not match the access token (at_hash)", err)
		}
	}
	if idt.Subject == "" {
		return auth.Session{}, "", loginErr(http.StatusUnauthorized, CodeInvalidIDToken, "the ID token has no subject", nil)
	}
	var raw json.RawMessage
	if err := idt.Claims(&raw); err != nil {
		return auth.Session{}, "", loginErr(http.StatusUnauthorized, CodeInvalidIDToken, "the ID token claims are unreadable", err)
	}
	claims, err := DecodeClaims(raw)
	if err != nil {
		return auth.Session{}, "", loginErr(http.StatusUnauthorized, CodeInvalidIDToken, "the ID token claims are unreadable", err)
	}
	if lerr := checkAudience(idt.Audience, claims, p.cfg.ClientID); lerr != nil {
		return auth.Session{}, "", lerr
	}
	id, lerr := p.cfg.Evaluate(idt.Issuer, idt.Subject, claims)
	if lerr != nil {
		return auth.Session{}, "", lerr
	}
	principal := id.Principal()

	if p.cfg.Provisioning.AutoProvision == AutoProvisionNever {
		known, err := p.opts.IdentityExists(ctx, id.Key)
		if err != nil {
			return auth.Session{}, "", loginErr(http.StatusInternalServerError, CodeInternal, "could not look up your account", err)
		}
		if !known {
			return auth.Session{}, "", loginErr(http.StatusForbidden, CodeNotProvisioned,
				fmt.Sprintf("signed in as %q, but this identity provider only admits accounts an administrator has created", id.Username), nil)
		}
	}
	grants, err := p.opts.Resolver.ResolveRoles(ctx, principal)
	if err != nil {
		return auth.Session{}, "", loginErr(http.StatusInternalServerError, CodeInternal, "could not resolve role bindings", err)
	}
	if len(grants) == 0 {
		if dr := p.cfg.Provisioning.DefaultRole; dr != "" && p.cfg.Provisioning.AutoProvision != AutoProvisionMappedOnly {
			grants = []auth.Grant{{Role: dr, ScopeKind: auth.ScopeCluster}}
		} else {
			// Strict by default (§8.4): never silently admin, never a
			// session with no permissions.
			return auth.Session{}, "", loginErr(http.StatusForbidden, CodeNoBindings,
				fmt.Sprintf("signed in as %q via %s, but no kudurru role binding matches your user or any of your %d groups; ask an administrator to bind a role to your user or one of your groups",
					id.Username, p.cfg.DisplayName, len(id.Groups)), nil)
		}
	}

	s := auth.Session{
		Principal:     principal,
		RolesSnapshot: auth.GrantStrings(grants),
		IDP:           p.cfg.ID,
		IssuedAt:      now,
		ExpiresAt:     now.Add(p.opts.SessionLifetime),
	}
	value, err := p.opts.Sessions.Issue(ctx, s)
	if err != nil {
		if errors.Is(err, session.ErrTooLarge) {
			return auth.Session{}, "", loginErr(http.StatusInternalServerError, CodeSessionTooLarge,
				fmt.Sprintf("your %d role bindings do not fit in a session cookie; ask an administrator to consolidate them", len(grants)), err)
		}
		return auth.Session{}, "", loginErr(http.StatusInternalServerError, CodeInternal, "could not create the session", err)
	}
	p.opts.Cookie.Set(w, r, value, p.opts.SessionLifetime)
	return s, SafeNext(pa.Next), nil
}

func (p *Provider) fail(w http.ResponseWriter, r *http.Request, e *LoginError) {
	p.opts.ErrorHandler(w, r, e)
}

func decodeSegment(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

func constantEqual(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// randomToken returns n random bytes, base64url without padding.
func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error (Go 1.24+)
	return base64.RawURLEncoding.EncodeToString(b)
}
