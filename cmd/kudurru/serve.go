package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/health"
	"github.com/robertherbaugh/kudurru/internal/metrics"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	logstage "github.com/robertherbaugh/kudurru/internal/stages/log"
	"github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/version"
)

func newServeCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the DNS server",
		Long: `Run the kudurru node. The scaffold serves a forwarding resolver with the
ingress and upstream stages, the ops listener (/healthz/*) and Prometheus
metrics. Zones, blocking, the store, the API and the UI land milestone by
milestone (docs/architecture.md §13).`,
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

func runServe(ctx context.Context, cfg *config.Config) error {
	logger := newLogger(cfg.Log)
	slog.SetDefault(logger)
	info := version.Get()
	logger.Info("kudurru starting", "version", info.Version, "commit", info.Commit, "node", cfg.Node.Name, "config", cfg.Source)

	// Metrics registry: build info, Go runtime, process, DNS.
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	build := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kudurru_build_info", Help: "Build information."}, []string{"version", "revision", "goversion"})
	build.WithLabelValues(info.Version, info.Commit, info.GoVersion).Set(1)
	reg.MustRegister(build)
	dnsMetrics := metrics.NewDNS(reg)

	// Query log: slog sink behind the non-blocking collector.
	sink := &querylog.SlogSink{Logger: logger, Level: slog.LevelInfo}
	if err := sink.Open(ctx, nil, reg); err != nil {
		return err
	}
	collector := querylog.NewAsync(sink, 10000, 1000, time.Second, logger)

	// Pipeline: ingress → upstream, with the log observer.
	group := cfg.Upstreams.Groups["default"]
	fwd, err := upstream.New("default", group.Servers, group.Timeout.Std())
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	chain, err := pipeline.New(
		&ingress.Stage{NodeID: cfg.Node.Name, Hostname: hostname, Version: info.Version},
		fwd,
	)
	if err != nil {
		return err
	}
	chain.Observe(&logstage.Observer{Node: cfg.Node.Name, Collector: collector, Metrics: dnsMetrics})
	logger.Info("pipeline ready", "stages", chain.Stages(), "upstreams", fwd.Servers)

	checker := health.New(map[string]any{"version": info.Version, "node": cfg.Node.Name})
	for _, l := range cfg.Listeners.DNS {
		checker.Set("dns:"+l.Address, false, "binding")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	run := func(name string, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s: %w", name, err)
				cancel()
			}
		}()
	}

	for _, l := range cfg.Listeners.DNS {
		srv := dnsserver.New(l.Address, l.Protocols, chain, logger)
		name := "dns:" + l.Address
		run(name, func(ctx context.Context) error {
			// Start returns a bind error immediately; readiness flips once bound.
			ready := make(chan struct{})
			go func() {
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					if len(srv.Addrs()) == len(l.Protocols) {
						checker.Set(name, true, "")
						close(ready)
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
			}()
			return srv.Start(ctx)
		})
	}
	run("ops", func(ctx context.Context) error {
		return serveHTTP(ctx, cfg.Listeners.Ops.Address, checker.Handler(), logger, "ops")
	})
	run("metrics", func(ctx context.Context) error {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{EnableOpenMetrics: true}))
		return serveHTTP(ctx, cfg.Listeners.Metrics.Address, mux, logger, "metrics")
	})
	run("selfcheck", func(ctx context.Context) error {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		checker.SetStarted()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
				checker.Tick()
			}
		}
	})

	var firstErr error
	select {
	case <-ctx.Done():
	case firstErr = <-errCh:
		logger.Error("component failed, shutting down", "err", firstErr)
	}
	checker.Lameduck()
	cancel()
	wg.Wait()
	shutdownCtx, c2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer c2()
	if err := collector.Close(shutdownCtx); err != nil {
		logger.Warn("query log flush incomplete", "err", err, "dropped", collector.Dropped())
	}
	logger.Info("kudurru stopped")
	return firstErr
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
