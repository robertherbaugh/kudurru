// Package api is the kudurru REST API server (docs/architecture.md §10):
// huma operations under /api/v1 on the Go net/http mux, the uniform
// envelope ({error:{key,message,hint}, took_ms}, list envelopes with
// keyset "continue" tokens), the §8.8 middleware chain, the route
// permission declarations every operation carries (decision D31), the
// SPA mount at "/", the OpenAPI document at /api/v1/openapi.json and the
// Scalar reference at /api/v1/docs.
//
// The server consumes, and never implements, authentication: the auth
// package hands it an http middleware that puts an *auth.Principal in the
// request context (Options.Authenticate), an rbac.Authorizer and a CSRF
// check for cookie sessions. Login, OIDC and token issuance are served by
// Options.AuthHandler; the API documents those routes and forwards them.
//
// Object writes follow decision D70: the body is validated unstructured
// (strict unknown fields, schema defaults) before it is converted to the
// typed object the store persists, exactly as objects.d files are.
package api

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/central"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
	"github.com/robertherbaugh/kudurru/internal/health"
	"github.com/robertherbaugh/kudurru/internal/objects"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	"github.com/robertherbaugh/kudurru/internal/unblock"
	"github.com/robertherbaugh/kudurru/internal/upstream"
	"github.com/robertherbaugh/kudurru/internal/version"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// BasePath is the prefix of every API route.
const BasePath = "/api/v1"

// APIVersion is the OpenAPI info.version of the document; the build
// version is served by GET /version so the committed document does not
// change with every build.
const APIVersion = "v1"

// DefaultNamespace is used for namespaced objects that name none.
const DefaultNamespace = "default"

// maxBodyBytes caps request bodies (the 1.5 MiB etcd object cap plus
// headroom for multi-document apply and zone imports).
const maxBodyBytes = 32 << 20

// PolicySource returns the current policy index (policy.Source).
type PolicySource interface {
	Index() *policy.Index
}

// ListStatusSource reports the blocklist resolver state on this node
// (listfetcher.Resolver).
type ListStatusSource interface {
	Ready() (bool, []string)
}

// ListBodies opens the body of a URL or ConfigMap source as this node's
// list cache holds it (listfetcher.DiskCache): the leader fetches it,
// replicas pull it by digest. It returns an error wrapping
// fs.ErrNotExist when the node has no body for the source.
type ListBodies interface {
	OpenSourceBody(namespace string, src v1alpha1.ListSource) (io.ReadCloser, SourceBody, error)
}

// SourceBody describes a cached source body.
type SourceBody struct {
	Digest  string
	Fetched time.Time
	Size    int64
}

// ListRefresher forces a refresh of one BlockList (leader-side fetch).
// The list fetcher has no such trigger yet; nil answers 501.
type ListRefresher interface {
	RefreshList(ctx context.Context, namespace, name string) error
}

// ZoneNotifier sends NOTIFY for one zone now and returns the targets.
// nil answers 501.
type ZoneNotifier interface {
	NotifyZone(ctx context.Context, zone string) ([]string, error)
}

// QuerylogPurger deletes query-log rows older than before (zero: all) and
// returns how many were deleted. sqlitelog.Writer has the batched delete
// machinery but no exported purge yet; nil answers 501.
type QuerylogPurger interface {
	Purge(ctx context.Context, before time.Time) (int64, error)
}

// SettingsWriter persists a validated settings change. next is the full
// configuration after the change; changed lists the dotted key paths the
// patch touched. Until the Kudurru singleton exists the default (nil)
// keeps changes in memory, which GET /settings then reports.
type SettingsWriter interface {
	WriteSettings(ctx context.Context, next *config.Config, changed []string) error
}

// UpstreamStatus reports the upstream servers (*upstream.Set).
type UpstreamStatus interface {
	Status() []upstream.ServerStatus
}

// UpstreamStatusFunc adapts a function to UpstreamStatus.
type UpstreamStatusFunc func() []upstream.ServerStatus

// Status implements UpstreamStatus.
func (f UpstreamStatusFunc) Status() []upstream.ServerStatus { return f() }

// CentralState reports the central database's state (*central.DB).
type CentralState interface {
	State() central.State
}

// Options configures a Server. Only Store and Validator are required;
// every other dependency switches its routes to 501/503 when absent, so
// tests and partial wirings work.
type Options struct {
	// Store is the object store; Validator checks every object write
	// (ValidateUnstructured first, D70).
	Store     store.Store
	Validator *validator.Validator
	// Cache is the zonecache; list and zone reads come from its snapshot
	// when it has synced.
	Cache *zonecache.Cache
	// Policy is the policy index source (policy.Source{Cache}).
	Policy PolicySource
	// Blocking is the blocking stage; its Toggle backs /dns/blocking.
	Blocking *blockingstage.Stage
	// Querylog reads the node's query log; Tail is the live tail (SSE
	// resume via Last-Event-ID); Purger backs DELETE /querylog.
	Querylog *sqlitelog.Reader
	Tail     *querylog.Tail
	Purger   QuerylogPurger
	// CentralQuerylog answers scope=cluster from the central database
	// (sqlitelog.NewCentralReader, D161) and CentralPurger purges it with
	// this node's rows; nil without a central database.
	CentralQuerylog *sqlitelog.Reader
	CentralPurger   QuerylogPurger
	// Upstreams and ResponseCache back upstream status and /cache/flush.
	// Upstreams is an *upstream.Set, or UpstreamStatusFunc over the
	// upstream stage when the set is swapped on settings reloads.
	Upstreams     UpstreamStatus
	ResponseCache *cache.Cache
	// Chain runs POST /dns/query in-process.
	Chain *pipeline.Chain
	// Config is the effective configuration (settings routes, listener);
	// Settings persists PATCH /settings (cluster scope; PATCH /node/config
	// changes stay in memory). Reload, when set, is the live configuration:
	// reads use Reload.Current() and PATCHes are applied with Reload.Set,
	// which notifies the hot-reload subscribers; Config is then ignored.
	Config   *config.Config
	Settings SettingsWriter
	Reload   *config.Reloadable
	// Objects is the file-managed objects loader (CheckAPIWrite for the
	// ha-static shape, diagnoses for /info/messages).
	Objects *objects.Loader
	// Health is the node's readiness checker; Membership lists nodes.
	Health     *health.Checker
	Membership cluster.Membership
	// InternalClient fetches peer status over the authenticated internal listener.
	InternalClient *internalapi.Client
	// Lists is the list resolver; Refresher and Notifier are optional
	// leader-side triggers.
	Lists     ListStatusSource
	Refresher ListRefresher
	// ListBodies opens this node's cached body of a URL or ConfigMap
	// source for the entries browser; nil answers 503 for those sources.
	ListBodies ListBodies
	Notifier   ZoneNotifier
	// Central is the optional central database (internal/central); its
	// state is reported by /info/messages. nil: none configured.
	Central CentralState
	// Audit records denied attempts and non-store mutations. AuditReader
	// backs GET /audit and /audit/export; when nil, the reader behind
	// Audit is used (audit.ReaderOf), and the routes answer 503 when no
	// recorder stores events. Reading the audit trail is a licensed
	// feature (license.FeatureAudit), answered 402 without it.
	Audit       audit.Recorder
	AuditReader audit.Reader
	// License is the node's license (*license.Manager). It backs GET
	// /license and the 402 for configuration writes while the cluster is
	// larger than the license allows; nil turns both gates off.
	License LicenseSource
	// Authenticate puts the principal in the request context or answers
	// 401 for bad credentials (anonymous requests pass through and are
	// refused by Authorize on non-public routes).
	Authenticate func(http.Handler) http.Handler
	// Authorizer decides route permissions; nil denies everything that
	// is not public.
	Authorizer rbac.Authorizer
	// CSRF checks cookie-authenticated unsafe requests (the
	// X-Kudurru-CSRF header); nil skips the check here (the session
	// authenticator enforces it too).
	CSRF func(*http.Request) error
	// RateLimit optionally wraps /api/v1/auth/login before authentication.
	// serve leaves it nil; the users store enforces login lockouts.
	RateLimit func(http.Handler) http.Handler
	// AuthHandler serves /api/v1/auth/{status,login,logout,password,
	// oidc/...,tokens...} and the read-only /api/v1/iam/* lists (the auth
	// stack's mux, documented by the mirrors of authtypes.go); GET
	// /auth/me is served by the API itself.
	AuthHandler http.Handler
	// Roles lists the effective grants of a principal for GET /auth/me
	// (role or role@kind:name). nil reports a cookie session's roles
	// snapshot, else none.
	Roles func(ctx context.Context, p auth.Principal) []string
	// Catalogue backs /catalogue; a nil Holder serves the embedded one.
	Catalogue CatalogueSource
	// Unblock is the unblock request book (internal/unblock) behind
	// /unblockrequests; nil serves the store without an index.
	Unblock *unblock.Service
	// BlockPage renders GET /blockpage/preview (internal/blockpage); nil
	// answers 503.
	BlockPage BlockPagePreviewer
	// UI is the single-page app bundle (web.FS); nil serves a built-in
	// placeholder page. UIHandler, when set, replaces the built-in SPA
	// handler (web.Handler with the same path prefix); it receives paths
	// with the prefix stripped.
	UI        fs.FS
	UIHandler http.Handler
	// Logger, Registerer: logging and metrics (nil Registerer skips
	// registration).
	Logger     *slog.Logger
	Registerer prometheus.Registerer
	// Version is the build information; NodeName is node.name.
	Version  version.Info
	NodeName string
	// DefaultNamespace for namespaced objects without one ("default").
	DefaultNamespace string
	// Now is the clock (tests).
	Now func() time.Time
}

// Server is the API server. Build it with New; it is an http.Handler.
type Server struct {
	opts    Options
	log     *slog.Logger
	api     huma.API
	mux     *http.ServeMux
	handler http.Handler
	perms   *rbac.Permissions
	metrics *metrics
	trusted *trustedProxies
	prefix  string
	ui      http.Handler

	// routes are the declarations of every registered operation, for the
	// route-permission lint and /auth/me.
	routes []rbac.Route

	cfgMu sync.RWMutex
	cfg   *config.Config

	// streams is the semaphore of the live-tail streams (maxQueryStreams).
	streams chan struct{}
	// bodyScans is the semaphore of the parses of cached list bodies
	// (maxBodyScans).
	bodyScans chan struct{}

	// unblockFallback serves /unblockrequests without Options.Unblock.
	unblockOnce     sync.Once
	unblockFallback *unblock.Service
	// nodes caches the cluster size for the license gate.
	nodes nodeCounter
	// policyMu serializes the policy limit check with the create it guards.
	policyMu sync.Mutex
	// domainSvc is the Allowlist and Denylist over the store.
	domainOnce sync.Once
	domainSvc  *domainlist.Service
}

// New builds the server and registers every route.
func New(opts Options) (*Server, error) {
	if opts.Store == nil {
		return nil, errors.New("api: Store is required")
	}
	if opts.Validator == nil {
		return nil, errors.New("api: Validator is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.DefaultNamespace == "" {
		opts.DefaultNamespace = DefaultNamespace
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Version.Version == "" {
		opts.Version = version.Get()
	}
	if opts.Reload != nil {
		opts.Config = opts.Reload.Current()
	}
	if opts.Config == nil {
		cfg, err := config.Load("", nil)
		if err != nil {
			return nil, err
		}
		opts.Config = cfg
	}
	if opts.NodeName == "" {
		opts.NodeName = opts.Config.Node.Name
	}
	if opts.Membership == nil {
		opts.Membership = cluster.Self{Name: opts.NodeName}
	}
	perms, err := rbac.LoadPermissions()
	if err != nil {
		return nil, err
	}
	trusted := &trustedProxies{}
	for _, e := range opts.Config.Listeners.API.TrustedProxies {
		p, err := config.ParsePrefixOrAddr(e)
		if err != nil {
			return nil, fmt.Errorf("api: listeners.api.trustedProxies: %w", err)
		}
		trusted.prefixes = append(trusted.prefixes, p)
	}
	installHumaErrors()

	s := &Server{
		opts:      opts,
		log:       opts.Logger.With("component", "api"),
		mux:       http.NewServeMux(),
		perms:     perms,
		metrics:   newMetrics(opts.Registerer),
		trusted:   trusted,
		prefix:    strings.TrimSuffix(opts.Config.Listeners.API.PathPrefix, "/"),
		cfg:       opts.Config,
		streams:   make(chan struct{}, max(maxQueryStreams, 1)),
		bodyScans: make(chan struct{}, max(maxBodyScans, 1)),
	}
	s.api = newHumaAPI(s.mux, s.prefix)
	s.ui = opts.UIHandler
	if s.ui == nil {
		s.ui = newUIHandler(opts.UI, s.prefix)
	}
	s.registerRoutes()
	s.mux.Handle(BasePath+"/", http.HandlerFunc(s.apiNotFound))
	s.mux.Handle("/", s.ui)

	cop := http.NewCrossOriginProtection()
	inner := chain(s.mux,
		onPrefix(BasePath+"/auth/login", opts.RateLimit),
		onPrefix(BasePath+"/", opts.Authenticate),
		s.actorMW,
		s.licenseMW,
	)
	h := chain(inner,
		recoverMW(s.log),
		requestIDMW(trusted),
		realIPMW(trusted),
		securityHeadersMW(trusted),
		accessLogMW(s.log, s.metrics),
		crossOriginMW(cop),
	)
	if s.prefix != "" {
		h = stripPrefix(s.prefix, h)
	}
	s.handler = h
	return s, nil
}

// newHumaAPI configures huma: JSON only, no $schema links, the kudurru
// error envelope, the took_ms transformer and OpenAPI at
// /api/v1/openapi.json (the Scalar reference at /api/v1/docs is docs.go).
func newHumaAPI(mux *http.ServeMux, prefix string) huma.API {
	cfg := huma.DefaultConfig("kudurru API", APIVersion)
	cfg.CreateHooks = nil
	cfg.SchemasPath = ""
	cfg.OpenAPIPath = "/openapi"
	// The Scalar docs page is served by registerDocs from an embedded
	// bundle (huma's renderer loads it from unpkg.com).
	cfg.DocsPath = ""
	cfg.Info.Description = "The kudurru REST API (docs/architecture.md §10). Errors are {error:{key,message,hint}, took_ms}; " +
		"lists are {items, continue, took_ms} with opaque keyset continue tokens."
	cfg.Info.License = &huma.License{Name: "PolyForm-Noncommercial-1.0.0", URL: "https://polyformproject.org/licenses/noncommercial/1.0.0"}
	cfg.Servers = []*huma.Server{{URL: prefix + BasePath}}
	cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"bearer": {Type: "http", Scheme: "bearer", Description: "kdr_<id>_<secret> API token, OIDC ID token or Kubernetes ServiceAccount token"},
		"cookie": {Type: "apiKey", In: "cookie", Name: "kudurru_session", Description: "Session cookie; unsafe methods also need X-Kudurru-CSRF"},
	}
	cfg.Security = []map[string][]string{{"bearer": {}}, {"cookie": {}}}
	cfg.Transformers = append(cfg.Transformers, tookTransformer)
	api := humago.NewWithPrefix(mux, BasePath, cfg)
	registerTypeAliases(api.OpenAPI().Components.Schemas)
	return api
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// Handler returns the full handler (middleware chain, API and UI).
func (s *Server) Handler() http.Handler { return s.handler }

// OpenAPI returns the OpenAPI document of every registered route.
func (s *Server) OpenAPI() *huma.OpenAPI { return s.api.OpenAPI() }

// Routes returns the permission declaration of every route, for the
// route-permission lint (rbac.LintRoutes).
func (s *Server) Routes() []rbac.Route {
	out := make([]rbac.Route, len(s.routes))
	copy(out, s.routes)
	return out
}

// config returns the current configuration (PATCH /settings replaces it).
func (s *Server) config() *config.Config {
	if s.opts.Reload != nil {
		return s.opts.Reload.Current()
	}
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

// Run serves the API on listeners.api.address until ctx ends. TLS uses
// listeners.api.tls.certFile/keyFile, or with tls.auto=selfsigned a
// certificate generated once into <node.dataDir>/keys/ (§6.2).
func (s *Server) Run(ctx context.Context) error {
	cfg := s.config()
	ln, err := net.Listen("tcp", cfg.Listeners.API.Address)
	if err != nil {
		return fmt.Errorf("api: listen %s: %w", cfg.Listeners.API.Address, err)
	}
	return s.Serve(ctx, ln)
}

// Serve serves on ln (Run without the listen step; tests).
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	cfg := s.config()
	tlsCfg, err := s.tlsConfig(cfg)
	if err != nil {
		_ = ln.Close()
		return err
	}
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	if tlsCfg != nil {
		srv.TLSConfig = tlsCfg
		ln = tls.NewListener(ln, tlsCfg)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	s.log.Info("api listener started", "address", ln.Addr().String(), "tls", tlsCfg != nil, "pathPrefix", s.prefix)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := srv.Shutdown(shutdownCtx)
		<-errCh
		return err
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// stripPrefix serves h below prefix: "/kudurru/api/v1/zones" reaches h as
// "/api/v1/zones"; "/kudurru" redirects to "/kudurru/"; anything outside
// the prefix is 404.
func stripPrefix(prefix string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == prefix:
			http.Redirect(w, r, prefix+"/", http.StatusMovedPermanently)
			return
		case strings.HasPrefix(p, prefix+"/"):
		default:
			http.NotFound(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = strings.TrimPrefix(p, prefix)
		if r.URL.RawPath != "" {
			r2.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, prefix)
		}
		h.ServeHTTP(w, r2)
	})
}

// apiNotFound answers unknown /api/v1 paths with the envelope.
func (s *Server) apiNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, newError(http.StatusNotFound, KeyNotFound, "no route "+r.Method+" "+r.URL.Path, "see "+s.prefix+BasePath+"/docs"))
}
