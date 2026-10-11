package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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

	"github.com/robertherbaugh/kudurru/internal/bootstrap"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/controller"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/proxyproto"
	"github.com/robertherbaugh/kudurru/internal/health"
	"github.com/robertherbaugh/kudurru/internal/metrics"
	"github.com/robertherbaugh/kudurru/internal/objects"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	cachestage "github.com/robertherbaugh/kudurru/internal/stages/cache"
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

func newLogger(cfg config.Log) *slog.Logger {
	level := slog.LevelInfo
	switch strings.SplitN(cfg.Level, ",", 2)[0] {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.Format == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

// openStore opens the configured driver (docs/architecture.md §4.3, §4.4).
// The returned snapshot store is non-nil only for the kube driver.
func openStore(ctx context.Context, cfg *config.Config, v *validator.Validator, logger *slog.Logger) (store.Store, *localdb.SnapshotStore, error) {
	switch cfg.Store.Driver {
	case "sqlite":
		st, err := sqlite.Open(filepath.Join(cfg.Node.DataDir, sqlite.FileName), sqlite.Options{
			Validator: v, Identity: cfg.Node.Name, Logger: logger.With("component", "store"),
		})
		switch {
		case errors.Is(err, sqlite.ErrSchemaTooNew):
			return nil, nil, fmt.Errorf("state.db was written by a newer kudurru: %w", err)
		case errors.Is(err, sqlite.ErrLocked):
			return nil, nil, fmt.Errorf("another kudurru process owns state.db: %w", err)
		case err != nil:
			return nil, nil, err
		}
		return st, nil, nil
	case "kube":
		restCfg, err := ctrlconfig.GetConfig()
		if err != nil {
			return nil, nil, fmt.Errorf("store.driver kube: %w", err)
		}
		st, err := kube.New(restCfg, kube.Options{
			Identity: cfg.Node.Name, StartupTimeout: cfg.Store.Kube.StartupTimeout.Std(), Logger: logger.With("component", "store"),
		})
		if err != nil {
			return nil, nil, err
		}
		db, err := localdb.OpenSnapshotStore(ctx, filepath.Join(cfg.Node.DataDir, localdb.FileName))
		if err != nil {
			_ = st.Close()
			if errors.Is(err, localdb.ErrSchemaTooNew) {
				return nil, nil, fmt.Errorf("local.db was written by a newer kudurru: %w", err)
			}
			return nil, nil, err
		}
		return st, db, nil
	default:
		return nil, nil, fmt.Errorf("store.driver %q is not supported", cfg.Store.Driver)
	}
}

func runServe(ctx context.Context, cfg *config.Config) error {
	logger := newLogger(cfg.Log)
	slog.SetDefault(logger)
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
	st, snapDB, err := openStore(ctx, cfg, v, logger)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	// First boot: root forward zone, default client group and policy (§6.4).
	seed, err := bootstrap.Seed(ctx, st, bootstrap.Options{Validator: v, Logger: logger})
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
	zc, err := zonecache.New(st, zonecache.Options{
		LocalDB: snapDB, StartupTimeout: cfg.Store.Kube.StartupTimeout.Std(), MaxAge: cfg.Store.Kube.Snapshot.MaxAge.Std(),
		Registerer: reg, Logger: logger,
	})
	if err != nil {
		return err
	}
	if err := zc.Register(zoneview.Builder(logger)); err != nil {
		return err
	}
	checker.Register("zonecache", zc.Ready)

	// Query log: slog sink behind the non-blocking collector.
	sink := &querylog.SlogSink{Logger: logger, Level: slog.LevelInfo}
	if err := sink.Open(ctx, nil, reg); err != nil {
		return err
	}
	collector := querylog.NewAsync(sink, 10000, 1000, time.Second, logger)

	// Pipeline: ingress → authoritative → cache → upstream, with the log observer.
	upSet, err := upstream.New(upstream.ConfigFrom(cfg.Upstreams), upstream.Options{Logger: logger, Metrics: upstream.NewMetrics(reg)})
	if err != nil {
		return err
	}
	fwd := upstreamstage.NewStage(upSet)
	hostname, _ := os.Hostname()
	stages := []pipeline.Stage{
		&ingress.Stage{NodeID: cfg.Node.Name, Hostname: hostname, Version: info.Version},
		authoritative.New(zoneview.Source{Cache: zc}),
	}
	var cs *cachestage.Stage
	var rc *cache.Cache
	if cfg.Cache.MaxEntries > 0 {
		rc = cache.New(cache.OptionsFrom(cfg.Cache, cache.NewMetrics(reg)))
		cs = cachestage.New(rc, cachestage.SettingsFrom(cfg.Cache), fwd)
		stages = append(stages, cs)
	}
	stages = append(stages, fwd)
	chain, err := pipeline.New(stages...)
	if err != nil {
		return err
	}
	chain.Observe(&logstage.Observer{Node: cfg.Node.Name, Collector: collector, Metrics: dnsMetrics})
	logger.Info("pipeline ready", "stages", chain.Stages(), "upstreamGroups", upSet.Groups())

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
		Handler: &dnsserver.Handler{Chain: chain, Logger: logger},
		Logger:  logger,
		Proxy:   proxyPolicy,
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

	// Controller: leader-only reconcilers (status, serials, Events).
	ctrl, err := controller.New(st, controller.Options{Events: controller.LogSink{Logger: logger}, Registerer: reg, Logger: logger})
	if err != nil {
		return err
	}

	// Runner: always-on runnables on every replica, leader-only ones under
	// the election (D44, D78). Leadership loss never ends Run.
	runner, err := cluster.New(cluster.Options{
		Elector:    st.Elector(cluster.ElectionController),
		Lameduck:   func(context.Context) { checker.Lameduck() },
		Registerer: reg, Logger: logger,
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
	} {
		if err := add(r.name, r.leaderOnly, r.fn); err != nil {
			return err
		}
	}
	if rc != nil {
		if err := add("cache-sweep", false, func(ctx context.Context) error { return rc.Run(ctx, time.Minute) }); err != nil {
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
	_ = upSet.Close()
	shutdownCtx, c2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer c2()
	if err := collector.Close(shutdownCtx); err != nil {
		logger.Warn("query log flush incomplete", "err", err, "dropped", collector.Dropped())
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
