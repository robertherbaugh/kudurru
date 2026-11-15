package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/tracing"
	"github.com/robertherbaugh/kudurru/internal/tracing/otlp"
	"github.com/robertherbaugh/kudurru/internal/version"
)

// newTracer builds the query tracer of tracing.* (D382), or nil when
// tracing is off. The tracer is not running yet: the caller starts Run and
// closes it on shutdown. It contacts nothing; the collector credential is
// read on the first export.
func newTracer(cfg *config.Config, reg prometheus.Registerer, logger *slog.Logger) (*tracing.Tracer, error) {
	t := cfg.Tracing
	if !t.Enabled {
		return nil, nil
	}
	endpoint, err := config.ParseSinkURL(t.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("tracing.endpoint %w", err)
	}
	resolve, err := querylogSinkCredentials(cfg, nil)
	if err != nil {
		return nil, err
	}
	exporter, err := otlp.New(otlp.Options{
		URL:      endpoint,
		Resource: otlp.Resource{ServiceName: t.ServiceName, Version: version.Get().Version, Instance: cfg.Node.Name},
		Headers:  t.Headers,
		Bearer:   t.BearerTokenRef.Ref(),
		CA:       t.CARef.Ref(),
		Gzip:     t.Compression != config.QuerylogCompressionNone,
		Resolve:  resolve,
		Logger:   logger,
	})
	if err != nil {
		return nil, err
	}
	if t.Endpoint != "" && endpoint.Scheme == "http" && t.BearerTokenRef.Ref() != nil {
		logger.Warn("tracing sends its credential over plain HTTP", "url", endpoint.Scheme+"://"+endpoint.Host)
	}
	return tracing.New(tracing.Config{
		Ratio:          float64(t.SampleRate) / float64(config.MaxTracingSampleRate),
		Slow:           t.SlowThreshold.Std(),
		OptionCode:     uint16(t.EDNSOptionCode), //nolint:gosec // validated to 65001-65534
		AcceptIncoming: t.AcceptTraceparent,
		QueueSize:      t.QueueSize,
		Timeout:        t.Timeout.Std(),
		Exporter:       exporter,
		Registerer:     reg,
		Logger:         logger,
	})
}

// tracePrivacy lets a kept trace show the question name and the client
// address only as far as the live querylog.privacy lets the query log
// store them (D59): names under full and anonymize_client, the client
// address under full only (an anonymized address is not recomputed for a
// span; it is left out).
func tracePrivacy(reload *config.Reloadable) pipeline.TracePrivacy {
	privacy := querylogPrivacy(reload)
	return func() (names, clients bool) {
		mode, _ := privacy()
		return mode <= querylog.PrivacyAnonymizeClient, mode == querylog.PrivacyFull
	}
}

// startTracer runs the tracer and returns its bounded stop.
func startTracer(ctx context.Context, t *tracing.Tracer, logger *slog.Logger) func() {
	if t == nil {
		return func() {}
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() { defer close(done); t.Run(runCtx) }()
	return func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := t.Close(closeCtx); err != nil {
			logger.Warn("traces not exported before shutdown", "err", err)
		}
		cancel()
		<-done
	}
}
