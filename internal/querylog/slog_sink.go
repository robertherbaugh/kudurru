package querylog

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
)

// SlogSink writes one structured log line per event. The SQLite writer
// (internal/querylog/sqlitelog) is the default sink; SlogSink stays
// available as querylog.sink: slog for deployments that ship logs instead
// of storing them.
type SlogSink struct {
	Logger *slog.Logger
	Level  slog.Level
}

// Name implements Sink.
func (s *SlogSink) Name() string { return "slog" }

// Open implements Sink.
func (s *SlogSink) Open(_ context.Context, _ json.RawMessage, _ prometheus.Registerer) error {
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	return nil
}

// Write implements Sink.
func (s *SlogSink) Write(batch []*Event) error {
	return s.WriteBatch(context.Background(), batch, PrivacyFull)
}

// WriteBatch uses the original batch cancellation. Events already carry their
// captured privacy transform; the sink never reads current settings.
func (s *SlogSink) WriteBatch(ctx context.Context, batch []*Event, _ Mode) error {
	for _, ev := range batch {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.Logger.Log(ctx, s.Level, "query",
			"log", "query",
			"client", ev.Client.String(),
			"proto", ev.Proto.String(),
			"name", ev.Name,
			"qtype", dns.TypeToString[ev.QType],
			"status", ev.Status.String(),
			"rcode", dns.RcodeToString[ev.Rcode],
			"upstream", ev.Upstream,
			"answers", ev.Answers,
			"latency_us", ev.Latency.Microseconds(),
		)
	}
	return nil
}

// Flush implements Sink.
func (s *SlogSink) Flush(context.Context) error { return nil }

// Close implements Sink.
func (s *SlogSink) Close() error { return nil }
