package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	internalkinds "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/auth/adminsock"
	"github.com/robertherbaugh/kudurru/internal/auth/keys"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/auth/stack"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/bootstrap"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/cache/persist"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/controller"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/proxyproto"
	"github.com/robertherbaugh/kudurru/internal/edge/dnsdist"
	"github.com/robertherbaugh/kudurru/internal/health"
	"github.com/robertherbaugh/kudurru/internal/integration/externaldns"
	"github.com/robertherbaugh/kudurru/internal/listfetcher"
	"github.com/robertherbaugh/kudurru/internal/metrics"
	"github.com/robertherbaugh/kudurru/internal/notifier"
	"github.com/robertherbaugh/kudurru/internal/objects"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
	cachestage "github.com/robertherbaugh/kudurru/internal/stages/cache"
	"github.com/robertherbaugh/kudurru/internal/stages/control"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	logstage "github.com/robertherbaugh/kudurru/internal/stages/log"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	"github.com/robertherbaugh/kudurru/internal/upstream"
	"github.com/robertherbaugh/kudurru/internal/version"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	"github.com/robertherbaugh/kudurru/internal/zoneview"
	"github.com/robertherbaugh/kudurru/web"

	"k8s.io/client-go/rest"
)

func newServeCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the DNS server",
		Long: `Run the kudurru node: the store (sqlite or kube), first-boot bootstrap,
file-managed objects, zonecache, the query pipeline (ingress, authoritative,
cache, upstream) on every configured transport, the Lease-elected controller,
the ops and metrics listeners. The API and UI land in milestone M3.`,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := loadConfig(c, *configPath)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(c.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runServe(ctx, cfg)
		},
	}
}

// logLevel is the level of the serve logger; log.level reloads set it.
var logLevel = new(slog.LevelVar)

func newLogger(cfg config.Log) *slog.Logger { return newLoggerAt(cfg, new(slog.LevelVar)) }

// newLoggerAt builds the logger on level, set from cfg.Level.
func newLoggerAt(cfg config.Log, level *slog.LevelVar) *slog.Logger {
	level.Set(parseLevel(cfg.Level))
	opts := &slog.HandlerOptions{Level: level}
	if cfg.Format == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

// openStore opens the configured driver (docs/architecture.md §4.3, §4.4).
// The returned snapshot store is non-nil only for the kube driver.
// openStore opens the configured driver (docs/architecture.md §4.3, §4.4)
// with the public and internal kudurru.io kinds. The rest config is non-nil
// only for the kube driver.
func openStore(ctx context.Context, cfg *config.Config, v *validator.Validator, reg prometheus.Registerer, logger *slog.Logger) (store.Store, *localdb.SnapshotStore, *rest.Config, error) {
	scheme, err := internalkinds.NewScheme()
	if err != nil {
		return nil, nil, nil, err
	}
	var st store.Store
	var restCfg *rest.Config
	switch cfg.Store.Driver {
	case "sqlite":
		s, err := sqlite.Open(filepath.Join(cfg.Node.DataDir, sqlite.FileName), sqlite.Options{
			Validator: v, Scheme: scheme, InternalKinds: stack.InternalKinds(),
			Identity: cfg.Node.Name, Logger: logger.With("component", "store"), Metrics: sqlite.NewMetrics(reg),
		})
		switch {
		case errors.Is(err, sqlite.ErrSchemaTooNew):
			return nil, nil, nil, fmt.Errorf("state.db was written by a newer kudurru: %w", err)
		case errors.Is(err, sqlite.ErrLocked):
			return nil, nil, nil, fmt.Errorf("another kudurru process owns state.db: %w", err)
		case err != nil:
			return nil, nil, nil, err
		}
		st = s
	case "kube":
		restCfg, err = ctrlconfig.GetConfig()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("store.driver kube: %w", err)
		}
		s, err := kube.New(restCfg, kube.Options{
			Scheme: scheme, InternalKinds: stack.InternalKinds(),
			Identity: cfg.Node.Name, StartupTimeout: cfg.Store.Kube.StartupTimeout.Std(), Logger: logger.With("component", "store"),
		})
		if err != nil {
			return nil, nil, nil, err
		}
		st = s
	default:
		return nil, nil, nil, fmt.Errorf("store.driver %q is not supported", cfg.Store.Driver)
	}
	db, err := localdb.OpenSnapshotStore(ctx, filepath.Join(cfg.Node.DataDir, localdb.FileName))
	if err != nil {
		_ = st.Close()
		if errors.Is(err, localdb.ErrSchemaTooNew) {
			return nil, nil, nil, fmt.Errorf("local.db was written by a newer kudurru: %w", err)
		}
		return nil, nil, nil, err
	}
	return st, db, restCfg, nil
}

func runServe(ctx context.Context, cfg *config.Config) error {
	logger := newLoggerAt(cfg.Log, logLevel)
	slog.SetDefault(logger)
	// Settings hot reload (D25): every consumer of a cluster-scope key that
	// is not restartRequired subscribes to reload (reload.go).
	reload := config.NewReloadable(cfg)
	info := version.Get()
	logger.Info("kudurru starting", "version", info.Version, "commit", info.Commit, "node", cfg.Node.Name, "config", cfg.Source, "store", cfg.Store.Driver)

	if err := os.MkdirAll(cfg.Node.DataDir, 0o750); err != nil {
		return fmt.Errorf("node.dataDir: %w", err)
	}

	// Metrics registry: build info, Go runtime, process, DNS.
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	build := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kudurru_build_info", Help: "Build information."}, []string{"version", "revision", "goversion"})
	build.WithLabelValues(info.Version, info.Commit, info.GoVersion).Set(1)
	reg.MustRegister(build)
	dnsMetrics := metrics.NewDNS(reg)

	checker := health.New(map[string]any{"version": info.Version, "node": cfg.Node.Name})

	// Store: one validator per process, shared by the driver and the loaders.
	v, err := validator.New()
	if err != nil {
		return err
	}
	st, snapDB, restCfg, err := openStore(ctx, cfg, v, reg, logger)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	// First boot: root forward zone, default client group and policy (§6.4).
	seed, err := bootstrap.Seed(ctx, st, bootstrap.Options{Validator: v, Logger: logger, StarterBlockList: bootstrap.EmbeddedStarterBlockList})
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if seed.FirstBoot {
		logger.Info("first boot: seeded defaults", "created", len(seed.Created))
	}

	// File-managed objects (§6.2): inline objects and objectsDir.
	inlineSource := "kudurru.yaml"
	if cfg.Source != "" {
		inlineSource = filepath.Base(cfg.Source)
	}
	loader, err := objects.New(objects.Options{
		Store: st, Validator: v, Logger: logger, Registerer: reg,
		Inline: cfg.Objects, InlineSource: inlineSource, Dir: cfg.ObjectsDir,
	})
	if err != nil {
		return err
	}
	if report := loader.Load(ctx); len(report.Diagnoses) > 0 {
		for _, d := range report.Diagnoses {
			logger.Warn("object file diagnosis", "diagnosis", d)
		}
	}
	checker.Register("objects", loader.Ready)

	// zonecache: store watches → serving snapshots, local.db cold start (§4.4).
	var zcLocal *localdb.SnapshotStore
	if cfg.Store.Driver == "kube" {
		zcLocal = snapDB
	}
	zc, err := zonecache.New(st, zonecache.Options{
		LocalDB: zcLocal, StartupTimeout: cfg.Store.Kube.StartupTimeout.Std(), MaxAge: cfg.Store.Kube.Snapshot.MaxAge.Std(),
		Registerer: reg, Logger: logger,
	})
	if err != nil {
		return err
	}
	if err := zc.Register(zoneview.Builder(logger)); err != nil {
		return err
	}
	if err := zc.Register(policy.Builder(policy.Options{DefaultNamespace: bootstrap.DefaultNamespace}, logger)); err != nil {
		return err
	}
	checker.Register("zonecache", zc.Ready)
	reg.MustRegister(zoneview.NewCollector(zc)) // kudurru_zone_serial, kudurru_zone_records (§11.6)

	// Blocklists (§7.3, D15): the leader fetches, every node pulls bodies by
	// digest over the internal listener (D47) and compiles locally.
	lists, err := listfetcher.OpenDiskCache(cfg.BlockingListsDir(), logger)
	if err != nil {
		return err
	}
	listObjs := listfetcher.NewZoneCacheObjects(zc)
	downloader := listfetcher.NewDownloader(lists, listfetcher.DownloadOptions{AllowLocalAddresses: cfg.Blocking.AllowLocalListURLs})
	initStrategy, err := listfetcher.ParseInitStrategy(cfg.Blocking.InitStrategy)
	if err != nil {
		return err
	}
	fetcher, err := listfetcher.New(listfetcher.Options{
		Store: st, Objects: listObjs, Cache: lists, Downloader: downloader,
		RefreshInterval: cfg.Blocking.RefreshInterval.Std(), Registerer: reg, Logger: logger,
	})
	if err != nil {
		return err
	}
	var membership cluster.Membership = cluster.Self{Name: cfg.Node.Name}
	if cfg.Store.Driver == "kube" {
		headless := os.Getenv("KUDURRU_HEADLESS_SERVICE")
		_, port, _ := net.SplitHostPort(cfg.Listeners.Internal.Address)
		if port == "" {
			port = "8082"
		}
		byName := func(_ context.Context, pod string) (string, error) {
			host := pod
			if headless != "" {
				host = pod + "." + headless
			}
			return net.JoinHostPort(host, port), nil
		}
		resolve, err := leaderResolver(restCfg, headless, port, byName)
		if err != nil {
			return err
		}
		membership = &cluster.ElectorMembership{
			SelfName: cfg.Node.Name,
			Elector:  st.Elector(cluster.ElectionController),
			Resolve:  resolve,
			CacheTTL: 30 * time.Second,
		}
	}
	encKeys, err := keys.Load(keys.Options{Driver: cfg.Store.Driver, DataDir: cfg.Node.DataDir})
	if err != nil {
		return err
	}
	encKey, encPrev := encKeys.Current, encKeys.Previous
	if encKeys.Generated {
		logger.Info("generated the encryption key", "path", encKeys.Path)
	}
	var (
		internalClient *internalapi.Client
		internalSrv    *internalapi.Server
		internalCreds  *internalapi.Credentials
		listResolver   *listfetcher.Resolver
	)
	if encKey != nil || cfg.Listeners.Internal.TokenFile != "" {
		creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{
			Key: encKey, PreviousKey: encPrev, NodeName: cfg.Node.Name,
			TokenFile: cfg.Listeners.Internal.TokenFile,
			CertFile:  cfg.Listeners.Internal.TLS.CertFile, KeyFile: cfg.Listeners.Internal.TLS.KeyFile,
			CAFile: cfg.Listeners.Internal.TLS.CAFile, ServerName: cfg.Listeners.Internal.TLS.ServerName,
		})
		if err != nil {
			return err
		}
		internalCreds = creds
		internalClient = internalapi.NewClient(creds, internalapi.ClientOptions{})
		internalSrv, err = internalapi.NewServer(internalapi.ServerOptions{
			Address: cfg.Listeners.Internal.Address, Credentials: creds, Lists: lists,
			Status: func(ctx context.Context) internalapi.Status {
				leader, _ := membership.Leader(ctx)
				var ls []internalapi.ListStatus
				if listResolver != nil {
					ls = listResolver.Lists()
				}
				return internalapi.Status{Node: cfg.Node.Name, Version: info.Version, Leader: leader.Name, State: zc.State().String(), Lists: ls}
			},
			// Zones and serials for the edge supervisors (§3.6 step 5).
			Zones: func(context.Context) internalapi.ZoneList {
				zl := internalapi.ZoneList{Node: cfg.Node.Name}
				for _, z := range zoneview.Served(zc) {
					zl.Zones = append(zl.Zones, internalapi.Zone{Name: z.Name(), Serial: z.Serial(), Type: string(z.Type())})
				}
				return zl
			},
			Registerer: reg, Logger: logger,
		})
		if err != nil {
			return err
		}
	} else if cfg.Store.Driver == "kube" {
		return errors.New("KUDURRU_ENCRYPTION_KEY (or listeners.internal.tokenFile) is required for the internal listener in Kubernetes")
	}
	// A dnsdist next to this node (sidecar, compose single-edge, HA nodes)
	// caches blocked and unblocked answers up to their TTL: flush its
	// resolver pool on every compiled list snapshot and on blocking toggles
	// (docs/m3/edge.md). Without a local edge the flusher is a no-op.
	edgeFlush := dnsdist.NewLocalCacheFlusher("", "", logger.With("component", "edgeflush"))
	defer edgeFlush.Stop()
	listResolver, err = listfetcher.NewResolver(listfetcher.ResolverOptions{
		Objects: listObjs, Cache: lists, Sink: flushingListSink{next: zc, flush: edgeFlush}, Membership: membership, Client: internalClient,
		Downloader: downloader, RefreshInterval: cfg.Blocking.RefreshInterval.Std(),
		InitStrategy: initStrategy, AllowDegraded: cfg.Blocking.AllowDegraded,
		Registerer: reg, Logger: logger,
	})
	if err != nil {
		return err
	}
	checker.Register("blocklists", listResolver.Ready)

	// Query log (§11.2): privacy once at ingest, live tail, sqlite or slog sink.
	var (
		collector querylog.Collector // nil interface when disabled
		qlAsync   *querylog.Async
		qlWriter  *sqlitelog.Writer
		qlTail    *querylog.Tail
		qlReader  *sqlitelog.Reader
	)
	if cfg.Querylog.Enabled {
		qlMetrics := querylog.NewMetrics(reg)
		privacy, err := querylog.PrivacyFrom(cfg.Querylog)
		if err != nil {
			return err
		}
		tail := querylog.NewTail(0, 0, qlMetrics)
		qlTail = tail
		var sink querylog.Sink
		var firstID uint64
		switch cfg.Querylog.Sink {
		case "sqlite":
			opts, err := sqlitelog.OptionsFrom(cfg.Querylog, cfg.Node.DataDir, cfg.Node.Name)
			if err != nil {
				return err
			}
			opts.Metrics, opts.Tail, opts.Logger = qlMetrics, tail, logger
			opts.DomainsOnLists = func() int64 {
				if sn := zc.Snapshot(); sn != nil {
					return domainsOnLists(sn.Objects.Lists())
				}
				return 0
			}
			if qlWriter, err = sqlitelog.Open(ctx, opts); err != nil {
				if errors.Is(err, sqlitelog.ErrSchemaTooNew) {
					return fmt.Errorf("querylog.db was written by a newer kudurru: %w", err)
				}
				return err
			}
			sink, firstID = qlWriter, qlWriter.NextID()
			qlReader = qlWriter.Reader()
		default:
			s := &querylog.SlogSink{Logger: logger, Level: slog.LevelInfo}
			if err := s.Open(ctx, nil, reg); err != nil {
				return err
			}
			sink = s
		}
		qlAsync = querylog.NewAsyncWith(querylog.AsyncOptions{Sink: sink, Privacy: privacy, Tail: tail, Metrics: qlMetrics, FirstID: firstID, Logger: logger})
		collector = qlAsync
	}

	// Pipeline: ingress → authoritative → cache → upstream, with the log observer.
	upOpts := upstream.Options{Logger: logger, Metrics: upstream.NewMetrics(reg)}
	upSet, err := upstream.New(upstream.ConfigFrom(cfg.Upstreams), upOpts)
	if err != nil {
		return err
	}
	fwd := upstreamstage.NewStage(upSet)
	hostname, _ := os.Hostname()

	// Ingress rate limiting (§7.1 step 2).
	// The limiter always exists so ratelimit.enabled can change at runtime.
	rlc, err := ingress.RateLimitFromConfig(cfg.RateLimit)
	if err != nil {
		return err
	}
	limiter, err := ingress.NewRateLimiter(ingress.RateLimitConfig{})
	if err != nil {
		return err
	}
	if err := limiter.Reconfigure(rlc); err != nil {
		return err
	}
	limiter.SetHook(func(v ingress.Verdict) {
		if v == ingress.VerdictDrop {
			dnsMetrics.Dropped.WithLabelValues(ingress.DropReason).Inc()
		}
	})
	reg.MustRegister(limiter.Collector())

	// Policies and blocking (§7.1 steps 3, 4, 7).
	polSrc := policy.Source{Cache: zc}
	bopts := blockingstage.OptionsFrom(cfg.Blocking)
	bopts.Index, bopts.Registerer = polSrc, reg
	bs, err := blockingstage.New(bopts)
	if err != nil {
		return err
	}
	checker.Register("blocking", bs.Ready)
	bs.Toggle().SetOnChange(edgeFlush.BlockingChanged)

	auth := authoritative.New(zoneview.Source{Cache: zc})
	auth.Metrics = authoritative.NewMetrics(reg)

	stages := []pipeline.Stage{
		&ingress.Stage{NodeID: cfg.Node.Name, Hostname: hostname, Version: info.Version, Limiter: limiter, Lameduck: checker.IsLameduck},
		identify.New(polSrc, nil),
		control.New(control.Options{}),
		auth,
		bs,
	}
	// The cache stage always exists; cache.maxEntries 0 makes it pass
	// queries through, so the key can change at runtime.
	rc := cache.New(cache.OptionsFrom(cfg.Cache, cache.NewMetrics(reg)))
	cs := cachestage.New(rc, cachestage.SettingsFrom(cfg.Cache), fwd)
	cs.SetDisabled(cfg.Cache.MaxEntries == 0)
	stages = append(stages, cs)
	stages = append(stages, fwd)
	chain, err := pipeline.New(stages...)
	if err != nil {
		return err
	}
	chain.Observe(&logstage.Observer{Node: cfg.Node.Name, Collector: collector, Metrics: dnsMetrics})
	logger.Info("pipeline ready", "stages", chain.Stages(), "upstreamGroups", upSet.Groups())

	// TSIG keys for transfers and NOTIFY (RFC 8945).
	keyring, err := dnsserver.KeyringFromConfig(cfg.TSIGKeys)
	if err != nil {
		return err
	}
	tsigFailures := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kudurru_dns_tsig_failures_total", Help: "Requests rejected for their TSIG (RFC 8945), by error."}, []string{"error"})
	reg.MustRegister(tsigFailures)

	// DNS listeners: udp/tcp/dot/doh/doq/doh3 with the shared certificate
	// store and the authenticated PROXYv2 policy (D45, D56, D67).
	proxyPolicy, err := dnsserver.PolicyFromConfig(cfg.Listeners)
	if err != nil {
		return err
	}
	if proxyPolicy != nil {
		proxyPolicy.SetDropHook(func(r proxyproto.Reason) { dnsMetrics.Dropped.WithLabelValues(string(r)).Inc() })
	}
	registry, err := dnsserver.NewRegistry(dnsserver.Options{
		Handler:       &dnsserver.Handler{Chain: chain, Logger: logger},
		Logger:        logger,
		Proxy:         proxyPolicy,
		TSIG:          keyring,
		OnTSIGFailure: func(reason string) { tsigFailures.WithLabelValues(reason).Inc() },
		Sockets:       cfg.Listeners.Sockets,
	})
	if err != nil {
		return err
	}
	if err := registry.Add(dnsserver.SpecsFromConfig(cfg.Listeners)...); err != nil {
		return err
	}
	if err := registry.Bind(); err != nil { // fail fast on port conflicts
		return err
	}
	checker.Register("dns", registry.Ready)

	// NOTIFY out (RFC 1996): leader-only, triggered by every published snapshot.
	var notifySrc netip.Addr
	if cfg.Notify.Source != "" {
		notifySrc = netip.MustParseAddr(cfg.Notify.Source)
	}
	nt, err := notifier.New(notifier.Options{Source: zoneview.Source{Cache: zc}, Keyring: keyring, SourceAddr: notifySrc, Registerer: reg, Logger: logger})
	if err != nil {
		return err
	}
	zc.OnPublish(func(*zonecache.Snapshot) { nt.Trigger() })

	// Response cache persistence across restarts (local.db blob).
	var cachePersister *persist.Persister
	if cfg.Cache.MaxEntries > 0 && snapDB != nil {
		if cachePersister, err = persist.New(snapDB, persist.Options{}); err != nil {
			return err
		}
		if pst, err := cachePersister.Restore(ctx, rc); err != nil {
			logger.Warn("cache dump not restored", "err", err)
		} else if pst.Entries > 0 {
			logger.Info("cache restored", "entries", pst.Entries, "age", time.Since(pst.WrittenAt).Round(time.Second))
		}
	}

	// Identity (§8): users, tokens, sessions, OIDC, Kubernetes identities,
	// the RBAC engine and the admin socket.
	stackOpts := identityOptions(cfg, st, encKeys, logger)
	stackOpts.Kube = restCfg
	if cfg.Store.Driver == "kube" {
		stackOpts.Namespace = kube.PodNamespace()
	}
	if internalCreds != nil {
		stackOpts.InternalToken = internalCreds.ValidToken
	}
	ids, err := stack.New(ctx, stackOpts)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	authMux := http.NewServeMux()
	ids.Register(authMux)

	// REST API, dashboard and the external-dns webhook (§10, §5.6).
	recorders := audit.Multi{audit.Log{Logger: logger}}
	if ss, ok := st.(*sqlite.Store); ok {
		recorders = audit.Multi{audit.SQLite{Store: ss}, audit.Log{Logger: logger}}
	}
	if !web.Built() {
		logger.Warn("dashboard bundle not built into this binary; serving a placeholder page (make web-build)")
	}
	apiSrv, err := api.New(api.Options{
		Store: st, Validator: v, Cache: zc, Policy: polSrc, Blocking: bs,
		Querylog: qlReader, Tail: qlTail,
		Upstreams:     api.UpstreamStatusFunc(func() []upstream.ServerStatus { return fwd.Set().Status() }),
		ResponseCache: rc, Chain: chain,
		Config: cfg, Reload: reload, Settings: settingsWriter(cfg), Objects: loader, Health: checker, Membership: membership,
		Lists: listResolver, Notifier: api.NotifyTrigger(nt.Trigger),
		Refresher:    listRefresher(membership, fetcher),
		Purger:       querylogPurger(qlWriter),
		Audit:        recorders,
		Authenticate: ids.Middleware(), Authorizer: ids.Authorizer(),
		AuthHandler: authMux,
		Roles:       ids.Roles,
		UIHandler:   web.Handler(web.Options{BasePath: cfg.Listeners.API.PathPrefix}),
		Logger:      logger, Registerer: reg, Version: info, NodeName: cfg.Node.Name,
		DefaultNamespace: bootstrap.DefaultNamespace,
	})
	if err != nil {
		return err
	}
	var edns *externaldns.Server
	if cfg.ExternalDNS.Webhook.Enabled {
		eo := externaldns.OptionsFrom(cfg.ExternalDNS.Webhook)
		eo.Store, eo.Validator, eo.Cache, eo.Registerer, eo.Logger = st, v, zc, reg, logger
		if edns, err = externaldns.New(eo); err != nil {
			return err
		}
	}

	// Controller: leader-only reconcilers (status, serials, Events).
	ctrl, err := controller.New(st, controller.Options{Events: controller.LogSink{Logger: logger}, Registerer: reg, Logger: logger})
	if err != nil {
		return err
	}

	// Runner: always-on runnables on every replica, leader-only ones under
	// the election (D44, D78). Leadership loss never ends Run.
	runner, err := cluster.New(cluster.Options{
		Elector:          st.Elector(cluster.ElectionController),
		Lameduck:         func(context.Context) { checker.Lameduck() },
		LameduckDuration: cfg.Node.Lameduck.Std(),
		Registerer:       reg, Logger: logger,
	})
	if err != nil {
		return err
	}
	add := func(name string, leaderOnly bool, fn func(context.Context) error) error {
		return runner.Add(name, leaderOnly, fn)
	}
	for _, r := range []struct {
		name       string
		leaderOnly bool
		fn         func(context.Context) error
	}{
		{"zonecache", false, zc.Start},
		{"settings", false, func(ctx context.Context) error {
			return config.WatchSettings(ctx, reload, config.WatchOptions{Environ: os.Environ(), Logger: logger})
		}},
		{"objects", false, loader.Watch},
		{"dns", false, registry.Run},
		{"ops", false, func(ctx context.Context) error {
			return serveHTTP(ctx, cfg.Listeners.Ops.Address, checker.Handler(), logger, "ops")
		}},
		{"metrics", false, func(ctx context.Context) error {
			mux := http.NewServeMux()
			mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{EnableOpenMetrics: true}))
			return serveHTTP(ctx, cfg.Listeners.Metrics.Address, mux, logger, "metrics")
		}},
		{"selfcheck", false, func(ctx context.Context) error {
			select {
			case <-zc.Initialized():
				checker.SetStarted()
			case <-ctx.Done():
				return nil
			}
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-t.C:
					checker.Tick()
				}
			}
		}},
		{"controller", true, ctrl.Run},
		{"notifier", true, nt.Run},
		{"listfetcher", true, fetcher.Run},
		{"listresolver", false, listResolver.Run},
		{"identity", false, ids.Run},
		{"api", false, apiSrv.Run},
	} {
		if err := add(r.name, r.leaderOnly, r.fn); err != nil {
			return err
		}
	}
	subscribeReload(ctx, reload, reloadTargets{
		logger: logger, level: logLevel, upstreams: fwd, upOpts: upOpts, cache: rc, cacheStg: cs,
		blocking: bs, limiter: limiter, qlAsync: qlAsync, qlWriter: qlWriter,
	})
	if rc != nil {
		if err := add("cache-sweep", false, func(ctx context.Context) error { return rc.Run(ctx, time.Minute) }); err != nil {
			return err
		}
	}
	if cachePersister != nil {
		if err := add("cache-persist", false, func(ctx context.Context) error {
			return cache.RunPersister(ctx, rc, cachePersister, 10*time.Minute, 5*time.Second, logger)
		}); err != nil {
			return err
		}
	}
	if internalSrv != nil {
		if err := add("internal", false, internalSrv.Run); err != nil {
			return err
		}
	}
	if p := cfg.Auth.AdminSocket.Path; p != "" {
		if err := add("adminsock", false, func(ctx context.Context) error {
			err := ids.AdminSocket().ListenAndServe(ctx, p)
			if errors.Is(err, adminsock.ErrBusy) {
				// Another kudurru on this host owns the default socket: keep
				// serving without `kudurru admin` instead of failing startup.
				logger.Warn("admin socket is in use by another process; `kudurru admin` will not reach this node (set auth.adminSocket.path)", "path", p)
				<-ctx.Done()
				return nil
			}
			return err
		}); err != nil {
			return err
		}
	}
	if edns != nil {
		if err := add("externaldns", false, edns.Run); err != nil {
			return err
		}
	}
	if qlWriter != nil {
		if err := add("querylog", false, qlWriter.Run); err != nil {
			return err
		}
	}
	// Audit retention (§11.4): the leader prunes state.db audit rows older
	// than 400 days once a day.
	if ss, ok := st.(*sqlite.Store); ok {
		if err := add("audit-prune", true, (&sqlite.AuditPruner{Store: ss, Logger: logger}).Run); err != nil {
			return err
		}
	}

	runErr := runner.Run(ctx)
	if runErr != nil {
		logger.Error("component failed, shutting down", "err", runErr)
	}

	if cs != nil {
		cs.Wait()
	}
	_ = fwd.Set().Close()
	shutdownCtx, c2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer c2()
	if qlAsync != nil {
		if err := qlAsync.Close(shutdownCtx); err != nil {
			logger.Warn("query log flush incomplete", "err", err, "dropped", qlAsync.Dropped())
		}
	}
	if snapDB != nil {
		_ = snapDB.Close()
	}
	logger.Info("kudurru stopped")
	return runErr
}

func serveHTTP(ctx context.Context, addr string, h http.Handler, logger *slog.Logger, name string) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	logger.Info(name+" listener started", "address", addr)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// readEncryptionKeys reads KUDURRU_ENCRYPTION_KEY[_FILE] and the _PREVIOUS
// variants from the environment only, never generating one: the edge and
// VIP supervisors run in their own images and must share the node's key.
func readEncryptionKeys() (key, previous []byte, err error) {
	read := func(name string) ([]byte, error) {
		v := os.Getenv(name)
		if f := os.Getenv(name + "_FILE"); f != "" {
			b, err := os.ReadFile(f) //nolint:gosec // the operator names the file (KUDURRU_*_FILE)
			if err != nil {
				return nil, fmt.Errorf("%s_FILE: %w", name, err)
			}
			v = strings.TrimSpace(string(b))
		}
		if v == "" {
			return nil, nil
		}
		k, err := session.ParseKey(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return k, nil
	}
	if key, err = read(keys.EnvKey); err != nil {
		return nil, nil, err
	}
	if previous, err = read(keys.EnvPreviousKey); err != nil {
		return nil, nil, err
	}
	return key, previous, nil
}

// identityOptions are the stack.Options that do not depend on the
// deployment shape (the Kubernetes config, namespace and internal token
// are added by the caller).
func identityOptions(cfg *config.Config, st store.Store, k keys.Keys, logger *slog.Logger) stack.Options {
	return stack.Options{
		Config: cfg, Store: st, Keys: k,
		TrustForwardedProto: len(cfg.Listeners.API.TrustedProxies) > 0,
		APIPrefix:           strings.TrimSuffix(cfg.Listeners.API.PathPrefix, "/") + "/api/v1",
		ClientIP:            apiClientIP,
		Logger:              logger,
	}
}

// apiClientIP is stack.Options.ClientIP: the client address the API
// server's RealIP step derived (the right-most untrusted X-Forwarded-For
// entry behind listeners.api.trustedProxies), so login lockouts count
// clients rather than the reverse proxy every request arrives from.
// Requests that did not pass the API server (the admin socket) fall back
// to the transport peer.
func apiClientIP(r *http.Request) string {
	if a := api.ClientIP(r.Context()); a.IsValid() {
		return a.String()
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// flushingListSink hands every compiled list snapshot to the zone cache and
// then flushes the local edge's resolver cache.
type flushingListSink struct {
	next  listfetcher.ListSink
	flush *dnsdist.CacheFlusher
}

func (s flushingListSink) SetLists(sn *blocking.Snapshot) {
	s.next.SetLists(sn)
	s.flush.ListsChanged()
}

// domainsOnLists is the dashboard's "domains on lists" tile: the compiled
// block entries of every block-mode list, without the lists' own @@
// exceptions. Allow lists do not count. nil (no lists compiled yet) is 0.
func domainsOnLists(sn *blocking.Snapshot) int64 {
	if sn == nil {
		return 0
	}
	var n int64
	for _, l := range sn.Lists() {
		if l.Mode() != blocking.ModeBlock {
			continue
		}
		st := l.Stats()
		n += int64(st.Entries - st.Exceptions)
	}
	return n
}
