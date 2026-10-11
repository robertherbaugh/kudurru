// Package stack assembles the identity layer of docs/architecture.md §8 from
// the configuration and the store: the authenticator chain of §8.8 (bearer
// kdr_ → bearer JWT routed by iss → session cookie + CSRF → unix peer →
// internal token), LoadPrincipal, the authorizer (rbac.Engine over SAR for
// Kubernetes identities, config and RoleBinding-object bindings, and the
// implicit local admin), local users and the first-boot admin, API tokens
// and the bootstrap token, sealed sessions, OIDC providers from the config
// file and IdentityProvider objects, the /api/v1/auth/* and /api/v1/iam/*
// routes and the admin socket.
//
// The API server builds one Stack at startup, installs Middleware in its
// chain, authorizes its own routes with Authorizer (auth.Authorize), mounts
// Routes, and runs Run for the lifetime of the process (every replica: the
// caches are per process).
package stack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	authorizationv1client "k8s.io/client-go/kubernetes/typed/authorization/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/adminsock"
	"github.com/robertherbaugh/kudurru/internal/auth/k8s"
	"github.com/robertherbaugh/kudurru/internal/auth/keys"
	"github.com/robertherbaugh/kudurru/internal/auth/objcache"
	"github.com/robertherbaugh/kudurru/internal/auth/oidc"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// DefaultAPIPrefix is the API base path.
const DefaultAPIPrefix = "/api/v1"

// InternalKinds returns the internal kinds as store kinds, the value of
// sqlite.Options.InternalKinds and kube.Options.InternalKinds. The store
// scheme must come from internalkinds.NewScheme().
func InternalKinds() []store.Kind {
	out := make([]store.Kind, 0, len(internalv1.Kinds))
	for _, k := range internalv1.Kinds {
		out = append(out, store.Kind(k))
	}
	return out
}

// Options configures New.
type Options struct {
	// Config is the loaded configuration: auth.*, rbac.bindings,
	// node.name. Required.
	Config *config.Config
	// Store serves the internal kinds (InternalKinds). Required.
	Store store.Store
	// Keys are the encryption keys (keys.Load). Current is required.
	Keys keys.Keys
	// Kube is the pod's apiserver configuration with the kube driver: it
	// enables Kubernetes ServiceAccount tokens, SubjectAccessReview and
	// IdentityProvider clientSecretRef. nil in Docker mode.
	Kube *rest.Config
	// Namespace holds kudurru's own service accounts (bootstrap,
	// break-glass) and, with Kube, the Secrets of clientSecretRef: the
	// release namespace. Default tokens.DefaultNamespace.
	Namespace string
	// InternalToken validates the internal-listener token
	// (internalapi.Credentials.ValidToken); nil disables that step.
	InternalToken func(string) bool
	// CrossOrigin is the API server's CrossOriginProtection (with its
	// trusted origins); nil uses a default instance.
	CrossOrigin *http.CrossOriginProtection
	// TrustForwardedProto lets auth.session.secure=auto honour
	// X-Forwarded-Proto from a trusted proxy (only when the API server's
	// RealIP step guarantees the header's origin).
	TrustForwardedProto bool
	// APIPrefix is the API base path; default /api/v1.
	APIPrefix string
	// ClientIP returns the client address of a request after the
	// RealIP(trustedProxies) step; default the RemoteAddr host.
	ClientIP func(*http.Request) string
	// HTTPClient talks to identity providers; default 15 s timeout.
	HTTPClient *http.Client
	// Getenv reads the environment; default os.Getenv.
	Getenv func(string) string
	// Out receives the generated first-boot admin password; default
	// os.Stderr.
	Out io.Writer
	// Logger; default slog.Default().
	Logger *slog.Logger
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// Stack is the assembled identity layer.
type Stack struct {
	opts     Options
	cfg      *config.Config
	log      *slog.Logger
	perms    *rbac.Permissions
	bindings *rbac.BindingSource
	engine   *rbac.Engine
	users    *users.Users
	tokens   *tokens.Tokens
	sealed   *session.Sealed
	cookie   session.CookieConfig
	sessAuth *session.Authenticator
	k8s      *k8s.Authenticator
	secrets  oidc.SecretResolver
	cop      *http.CrossOriginProtection
	admin    *adminsock.Server

	roleBindings *objcache.Cache[*internalv1.RoleBinding]
	idps         *objcache.Cache[*internalv1.IdentityProvider]
	configIdPs   []*internalv1.IdentityProvider

	registry atomic.Pointer[oidc.Registry]
	regMu    sync.Mutex
	regDiag  []string

	middleware func(http.Handler) http.Handler
}

// New builds the stack: it validates the configuration, primes the
// identity caches from the store, creates the first-boot admin and the
// bootstrap token when configured, and builds the OIDC providers. It does
// not start watches; call Run.
func New(ctx context.Context, opts Options) (*Stack, error) {
	if opts.Config == nil || opts.Store == nil {
		return nil, errors.New("stack: Config and Store are required")
	}
	if len(opts.Keys.Current) == 0 {
		return nil, errors.New("stack: an encryption key is required (keys.Load)")
	}
	if opts.APIPrefix == "" {
		opts.APIPrefix = DefaultAPIPrefix
	}
	if opts.Namespace == "" {
		opts.Namespace = tokens.DefaultNamespace
	}
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ClientIP == nil {
		opts.ClientIP = remoteHost
	}
	if opts.CrossOrigin == nil {
		opts.CrossOrigin = http.NewCrossOriginProtection()
	}
	cfg := opts.Config
	log := opts.Logger.With("component", "auth")
	if m := cfg.Auth.Kubernetes.Mode; m != "" && m != "sar" {
		return nil, fmt.Errorf("auth.kubernetes.mode %q: v0.1 supports only sar (impersonate is v0.2, D55)", m)
	}
	s := &Stack{opts: opts, cfg: cfg, log: log, cop: opts.CrossOrigin}

	var err error
	if s.perms, err = rbac.LoadPermissions(); err != nil {
		return nil, err
	}
	static, err := rbac.ParseBindings(cfg.RBAC.Bindings)
	if err != nil {
		return nil, err
	}
	if s.bindings, err = rbac.NewBindingSource(s.perms, static, rbac.SourceOptions{Logger: log, Now: opts.Now}); err != nil {
		return nil, err
	}

	// Kubernetes identities (kube driver only).
	var sar rbac.Authorizer
	if opts.Kube != nil {
		if s.k8s, err = k8s.New(k8s.Options{Config: opts.Kube, Logger: log, Now: opts.Now}); err != nil {
			return nil, err
		}
		hc, err := rest.HTTPClientFor(opts.Kube)
		if err != nil {
			return nil, err
		}
		az, err := authorizationv1client.NewForConfigAndClient(opts.Kube, hc)
		if err != nil {
			return nil, err
		}
		if sar, err = rbac.NewSARAuthorizer(rbac.SAROptions{Client: az, Logger: log, Now: opts.Now}); err != nil {
			return nil, err
		}
		core, err := corev1client.NewForConfigAndClient(opts.Kube, hc)
		if err != nil {
			return nil, err
		}
		s.secrets = k8s.SecretResolver(core, opts.Namespace)
	}
	if s.engine, err = rbac.NewEngine(rbac.EngineOptions{Bindings: s.bindings, SAR: sar}); err != nil {
		return nil, err
	}

	// Stores and caches.
	if s.users, err = users.New(users.Options{Store: opts.Store, Logger: log, Now: opts.Now}); err != nil {
		return nil, err
	}
	if s.tokens, err = tokens.New(tokens.Options{Store: opts.Store, MaxLifetime: cfg.Auth.Tokens.MaxLifetime.Std(), Namespace: opts.Namespace, Logger: log, Now: opts.Now}); err != nil {
		return nil, err
	}
	s.roleBindings = objcache.New[*internalv1.RoleBinding](opts.Store, internalv1.KindRoleBinding, objcache.Options{Logger: log})
	s.roleBindings.OnChange(s.syncRoleBindings)
	s.idps = objcache.New[*internalv1.IdentityProvider](opts.Store, internalv1.KindIdentityProvider, objcache.Options{Logger: log})
	for _, prime := range []func(context.Context) error{s.users.Prime, s.tokens.Prime, s.roleBindings.Prime, s.idps.Prime} {
		if err := prime(ctx); err != nil {
			return nil, fmt.Errorf("stack: loading identity objects: %w", err)
		}
	}

	// First boot.
	if _, err := s.users.Bootstrap(ctx, users.BootstrapOptions{Getenv: opts.Getenv, PasswordFile: cfg.Auth.Bootstrap.AdminPasswordFile, Out: opts.Out, Identity: cfg.Node.Name}); err != nil {
		return nil, err
	}
	if _, err := s.tokens.Bootstrap(ctx, opts.Getenv); err != nil {
		return nil, err
	}

	// Sessions.
	s.cookie = session.CookieConfig{Secure: session.SecureMode(cfg.Auth.Session.Secure), TrustForwardedProto: opts.TrustForwardedProto, HostPrefix: true}
	if err := s.cookie.Validate(); err != nil {
		return nil, err
	}
	if s.sealed, err = session.NewSealed(session.Options{
		Key: opts.Keys.Current, PreviousKey: opts.Keys.Previous,
		CookieName:  session.HostCookiePrefix + s.cookie.Name, // the 4 KiB guard counts the longer name
		IdleTimeout: cfg.Auth.Session.IdleTimeout.Std(), Lifetime: cfg.Auth.Session.Lifetime.Std(),
		ValidAfter: s.users.ValidAfter, Now: opts.Now,
	}); err != nil {
		return nil, err
	}
	if s.sessAuth, err = session.NewAuthenticator(session.AuthenticatorOptions{Store: s.sealed, Cookie: s.cookie, CrossOrigin: s.cop, Logger: log, Now: opts.Now}); err != nil {
		return nil, err
	}

	// OIDC providers: config entries now, objects on every change.
	for i, m := range cfg.Auth.IdentityProviders {
		idp, err := oidc.ParseConfigEntry(m)
		if err != nil {
			return nil, fmt.Errorf("auth.identityProviders[%d]: %w", i, err)
		}
		s.configIdPs = append(s.configIdPs, idp)
	}
	if err := s.rebuildRegistry(ctx); err != nil {
		return nil, err
	}
	// Later IdentityProvider changes (the watch started by Run) rebuild the
	// registry; failures then only produce diagnoses.
	bg := context.WithoutCancel(ctx)
	s.idps.OnChange(func() { _ = s.rebuildRegistry(bg) })

	if s.admin, err = adminsock.New(adminsock.Options{
		Policy: auth.PeerPolicy{GID: cfg.Auth.AdminSocket.Group}, Users: s.users, Tokens: s.tokens,
		MaxTokenLifetime: cfg.Auth.Tokens.MaxLifetime.Std(), Logger: log,
	}); err != nil {
		return nil, err
	}
	s.middleware = auth.Middleware(auth.Options{Steps: s.Steps(), Load: s.load, OnError: s.onAuthError})
	return s, nil
}

// Steps returns the authenticator chain in §8.8 order.
func (s *Stack) Steps() []auth.Authenticator {
	jwt := auth.BearerJWT{Verifiers: []auth.JWTVerifier{registryVerifier{s}}}
	if s.k8s != nil {
		jwt.Verifiers = append(jwt.Verifiers, s.k8s)
		jwt.Fallback = s.k8s.Fallback()
	}
	steps := []auth.Authenticator{s.tokens, jwt, s.sessAuth, auth.UnixPeer{Policy: auth.PeerPolicy{GID: s.cfg.Auth.AdminSocket.Group}}}
	if s.opts.InternalToken != nil {
		steps = append(steps, auth.InternalToken{Valid: s.opts.InternalToken})
	}
	return steps
}

// load is the LoadPrincipal step: local users are re-read from the user
// cache (deleted or disabled → 401; live groups and flags).
func (s *Stack) load(ctx context.Context, p auth.Principal) (auth.Principal, error) {
	return s.users.LoadPrincipal(ctx, p)
}

func (s *Stack) onAuthError(r *http.Request, e *auth.Error) {
	s.log.DebugContext(r.Context(), "request credentials rejected", "key", e.Key, "status", e.Status, "path", r.URL.Path,
		"client", s.opts.ClientIP(r), "cause", errString(e.Err))
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Middleware returns the Authenticate + LoadPrincipal steps for the API
// server's chain (after CrossOriginProtection and the auth-route rate
// limit, before Authorize).
func (s *Stack) Middleware() func(http.Handler) http.Handler { return s.middleware }

// Authorizer returns the Authorize step's authorizer: token scopes, then
// the implicit local admin, then SAR (Kubernetes identities) and bindings;
// Decision.Path names the path that granted (D22).
func (s *Stack) Authorizer() rbac.Authorizer { return s.engine }

// Permissions returns the permissions table.
func (s *Stack) Permissions() *rbac.Permissions { return s.perms }

// Bindings returns the live binding source.
func (s *Stack) Bindings() *rbac.BindingSource { return s.bindings }

// Users returns the local user store.
func (s *Stack) Users() *users.Users { return s.users }

// Tokens returns the token store.
func (s *Stack) Tokens() *tokens.Tokens { return s.tokens }

// Sessions returns the sealed session store.
func (s *Stack) Sessions() *session.Sealed { return s.sealed }

// Cookie returns the session cookie configuration.
func (s *Stack) Cookie() session.CookieConfig { return s.cookie }

// AdminSocket returns the admin socket server (serve it with
// ListenAndServe on auth.adminSocket.path).
func (s *Stack) AdminSocket() *adminsock.Server { return s.admin }

// Registry returns the current OIDC provider registry.
func (s *Stack) Registry() *oidc.Registry { return s.registry.Load() }

// Run keeps the identity caches current until ctx is done.
func (s *Stack) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Go(func() { _ = s.users.Run(ctx) })
	wg.Go(func() { _ = s.tokens.Run(ctx) })
	wg.Go(func() { _ = s.roleBindings.Run(ctx) })
	wg.Go(func() { _ = s.idps.Run(ctx) })
	wg.Wait()
	return nil
}

// Diagnoses lists configuration problems for GET /info/messages: ignored
// RoleBinding objects and identity providers that could not be built.
func (s *Stack) Diagnoses() []string {
	out := s.bindings.Diagnoses()
	s.regMu.Lock()
	out = append(out, s.regDiag...)
	s.regMu.Unlock()
	sort.Strings(out)
	return out
}

func (s *Stack) syncRoleBindings() {
	s.bindings.SetObjects(rbac.RoleBindingObjects(s.roleBindings.List()))
}

// rebuildRegistry builds the OIDC registry from the config entries and the
// IdentityProvider objects. Config entries win over objects with the same
// id; providers that fail to build are skipped and reported by Diagnoses.
// At startup (first build) a broken config entry is an error.
func (s *Stack) rebuildRegistry(ctx context.Context) error {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	first := s.registry.Load() == nil
	var (
		providers []*oidc.Provider
		diag      []string
		seen      = map[string]bool{}
	)
	objs := s.idps.List()
	slices.SortFunc(objs, func(a, b *internalv1.IdentityProvider) int { return compareStrings(a.Name, b.Name) })
	type src struct {
		idp    *internalv1.IdentityProvider
		origin string
	}
	var all []src
	for _, idp := range s.configIdPs {
		all = append(all, src{idp, "auth.identityProviders"})
	}
	for _, idp := range objs {
		all = append(all, src{idp, "IdentityProvider/" + idp.Name})
	}
	for _, e := range all {
		if seen[e.idp.Name] {
			diag = append(diag, fmt.Sprintf("%s: id %q is already defined in the config file (ignored)", e.origin, e.idp.Name))
			continue
		}
		p, err := s.buildProvider(ctx, e.idp)
		if err != nil {
			if first && e.origin == "auth.identityProviders" {
				return err
			}
			diag = append(diag, fmt.Sprintf("%s: %v (ignored)", e.origin, err))
			continue
		}
		seen[e.idp.Name] = true
		providers = append(providers, p)
	}
	reg, err := oidc.NewRegistry(providers...)
	if err != nil {
		if first {
			return err
		}
		diag = append(diag, err.Error()+" (identity providers unchanged)")
		s.regDiag = diag
		return nil
	}
	if err := reg.CheckRedirects(s.opts.APIPrefix); err != nil {
		if first {
			return err
		}
		diag = append(diag, err.Error()+" (identity providers unchanged)")
		s.regDiag = diag
		return nil
	}
	s.registry.Store(reg)
	s.regDiag = diag
	return nil
}

func (s *Stack) buildProvider(ctx context.Context, idp *internalv1.IdentityProvider) (*oidc.Provider, error) {
	pc, err := oidc.FromIdentityProvider(ctx, idp, s.secrets)
	if err != nil {
		return nil, err
	}
	if pc.Provisioning.AutoProvision == oidc.AutoProvisionNever {
		return nil, fmt.Errorf("oidc provider %q: autoProvision: never needs the v0.2 user store; use mapped-only", pc.ID)
	}
	p, err := oidc.NewProvider(pc, oidc.Options{
		Key: s.opts.Keys.Current, PreviousKey: s.opts.Keys.Previous,
		Sessions: s.sealed, SessionLifetime: s.sealed.Lifetime(), Cookie: s.cookie,
		Resolver:    oidc.BindingResolver{Bindings: s.bindings.Bindings},
		Permissions: s.perms, HTTPClient: s.opts.HTTPClient, Logger: s.log, Now: s.opts.Now,
	})
	if err != nil {
		return nil, err
	}
	if dr := p.Config().Provisioning.DefaultRole; dr != "" {
		s.bindings.AllowGrant(rbac.Grant{Role: dr, Scope: rbac.Scope{Kind: rbac.ScopeCluster}})
	}
	return p, nil
}

// registryVerifier routes bearer ID tokens to the current registry's
// providers by issuer.
type registryVerifier struct{ s *Stack }

func (v registryVerifier) provider(iss string) *oidc.Provider {
	reg := v.s.registry.Load()
	if reg == nil {
		return nil
	}
	return reg.ByIssuer(iss)
}

func (v registryVerifier) AcceptsIssuer(_ context.Context, iss string) bool { return v.provider(iss) != nil }

func (v registryVerifier) VerifyJWT(ctx context.Context, raw string) (auth.Principal, error) {
	iss, _ := auth.UnverifiedIssuer(raw)
	p := v.provider(iss)
	if p == nil {
		return auth.Principal{}, auth.Unauthenticated(auth.ErrUnknownIssuer)
	}
	return p.VerifyJWT(ctx, raw)
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
