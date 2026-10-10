package querylog

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
)

// SlogSink writes one structured log line per event. It is the built-in
// sink of the scaffold; the SQLite writer replaces it as the default in M1.
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
	for _, ev := range batch {
		s.Logger.Log(context.Background(), s.Level, "query",
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
