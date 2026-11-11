package main

import (
	"context"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/dnstap/dnstaptest"
)

// TestQuerylogRuntimeRawDnstapWaitsForItsAuditEvent runs a raw dnstap sink
// through the production factory on the real runtime (D372). Until the
// node's audit recorders are bound the sink does not open and contacts no
// collector; its events wait in its own bounded queue. Once they are, the
// node records querylog.raw.export first and then sends the queries as
// they were, although the privacy mode hides them everywhere else.
func TestQuerylogRuntimeRawDnstapWaitsForItsAuditEvent(t *testing.T) {
	collector := dnstaptest.Listen(t, "unix")
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.DataDir = t.TempDir()
	cfg.Node.Name = "dns1"
	cfg.Querylog.Exclude.SUDN = false
	cfg.Querylog.Privacy = "hide_domains_clients"
	cfg.Querylog.Sinks = []config.QuerylogSink{{Name: "raw-tap", Type: config.QuerylogSinkDnstap, BatchSize: 1, RetryBackoff: config.Duration(100 * time.Millisecond),
		Dnstap: &config.QuerylogDnstap{Target: collector.Target(), Raw: true, Types: []string{config.DnstapClientQuery}}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r, err := newQuerylogRuntime(t.Context(), cfg, prometheus.NewRegistry(), slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.close(context.Background()) })
	route := r.current.Load()
	if !route.RetainsWire() {
		t.Fatal("a route with a raw sink does not retain wire messages")
	}
	enqueue := func(name string) {
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeA)
		wire, err := q.Pack()
		if err != nil {
			t.Fatal(err)
		}
		if !route.Enqueue(&querylog.Event{Time: time.Now(), Name: name, Client: netip.MustParseAddr("192.0.2.9"), QType: dns.TypeA, QClass: dns.ClassINET,
			Status: pipeline.StatusForwarded, ReplyType: querylog.ReplyIP, Wire: &querylog.Wire{Query: wire, ClientPort: 40000}}) {
			t.Fatal("collector refused", name)
		}
	}
	enqueue("early.example.")
	deadline := time.Now().Add(10 * time.Second)
	for r.sinks.Status()[0].Errors == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the raw sink never tried to open: %+v", r.sinks.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := r.sinks.Status()[0]; !st.Raw || collector.Accepted() != 0 {
		t.Fatalf("a raw sink without its audit event contacted the collector: %+v, %d connections", st, collector.Accepted())
	}
	recorded := &audit.Memory{}
	r.rawAudit.bind(recorded)
	enqueue("audited.example.")
	frames := collector.WaitFrames(1, 10*time.Second)
	events := recorded.Events()
	if len(events) != 1 || events[0].Action != AuditQuerylogRawExport || events[0].Node != "dns1" || events[0].Resource.Name != "raw-tap" || events[0].Result != audit.ResultSuccess {
		t.Fatalf("audit events %+v", events)
	}
	m := new(dns.Msg)
	if err := m.Unpack(frames[0].Message.QueryMessage); err != nil || m.Question[0].Name == querylog.HiddenName+"." {
		t.Fatalf("raw frame %v %v", m, err)
	}
	if frames[0].Message.QueryAddress != netip.MustParseAddr("192.0.2.9") || frames[0].Message.QueryPort != 40000 {
		t.Fatalf("raw client %+v", frames[0].Message)
	}
	for _, f := range frames {
		if f.Identity != "dns1" {
			t.Fatal("identity", f.Identity)
		}
	}
}
