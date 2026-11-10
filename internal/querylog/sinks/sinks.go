// Package sinks names the exporter types of querylog.sinks that send
// batches over HTTP (D371): clickhouse and loki. Both use the standard
// library client of the httpbatch package, run on the sink host of
// internal/querylog behind their own bounded queues, and export the flat
// record of the record package.
package sinks

import (
	"log/slog"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/clickhouse"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/httpbatch"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/loki"
)

// Options are the node's own inputs to every HTTP sink.
type Options struct {
	// Resolve reads the credential references of the settings with the
	// capabilities this node's operator delegated. A sink whose reference
	// it refuses does not open.
	Resolve httpbatch.Resolver
	Logger  *slog.Logger
	// UserAgent is sent with every request, "kudurru/<version>".
	UserAgent string
}

// Factories returns the factories of the HTTP sinks by querylog.sinks type.
// A factory validates the entry's settings and builds the sink without I/O;
// the host opens it from the sink's own goroutine.
func Factories(o Options) map[string]querylog.SinkFactory {
	return map[string]querylog.SinkFactory{
		config.QuerylogSinkClickHouse: func(spec querylog.SinkSpec) (querylog.Sink, error) {
			sink, err := clickhouse.New(spec, clickhouse.Options{Resolve: o.Resolve, Logger: o.Logger, UserAgent: o.UserAgent})
			if err != nil {
				return nil, err
			}
			return sink, nil
		},
		config.QuerylogSinkLoki: func(spec querylog.SinkSpec) (querylog.Sink, error) {
			sink, err := loki.New(spec, loki.Options{Resolve: o.Resolve, Logger: o.Logger, UserAgent: o.UserAgent})
			if err != nil {
				return nil, err
			}
			return sink, nil
		},
	}
}
