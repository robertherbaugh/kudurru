package tracing

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

var (
	peerTrace = TraceID{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36}
	peerSpan  = SpanID{0x00, 0xf0, 0x67, 0xaa, 0x0b, 0xa9, 0x02, 0xb7}
)

func query(options ...dns.EDNS0) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion("www.example.org.", dns.TypeA)
	m.SetEdns0(1232, false)
	opt := m.IsEdns0()
	opt.Option = append(opt.Option, options...)
	return m
}

func traceparent(code uint16, flags byte) *dns.EDNS0_LOCAL {
	return &dns.EDNS0_LOCAL{Code: code, Data: AppendOption(nil, peerTrace, peerSpan, flags)}
}

func newTestTracer(t *testing.T, c Config) (*Tracer, *Memory, *prometheus.Registry) {
	t.Helper()
	mem := &Memory{}
	if c.Exporter == nil {
		c.Exporter = mem
	}
	reg := prometheus.NewRegistry()
	c.Registerer = reg
	tr, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	return tr, mem, reg
}

func TestParseOptionVersions(t *testing.T) {
	full := AppendOption(nil, peerTrace, peerSpan, FlagSampled)
	if len(full) != 27 {
		t.Fatalf("version 0 option is %d octets, want 27", len(full))
	}
	r, ok := ParseOption(full)
	if !ok || r.TraceID != peerTrace || r.SpanID != peerSpan || !r.HasFlags || r.Flags != FlagSampled {
		t.Fatalf("full option parsed as %+v %v", r, ok)
	}
	// Earlier draft revisions: no flags, then no parent id.
	if r, ok := ParseOption(full[:26]); !ok || r.SpanID != peerSpan || r.HasFlags {
		t.Fatalf("26-octet option parsed as %+v %v", r, ok)
	}
	if r, ok := ParseOption(full[:18]); !ok || r.TraceID != peerTrace || r.SpanID.IsValid() {
		t.Fatalf("18-octet option parsed as %+v %v", r, ok)
	}
	bad := map[string][]byte{
		"empty":      nil,
		"short":      full[:17],
		"long":       append(bytes.Clone(full), 0),
		"version 1":  append([]byte{1}, full[1:]...),
		"zero trace": AppendOption(nil, TraceID{}, peerSpan, 1),
		"25 octets":  full[:25],
	}
	for name, b := range bad {
		if _, ok := ParseOption(b); ok {
			t.Errorf("%s: malformed option accepted", name)
		}
	}
}

func TestTakeOptionRemovesEveryCopy(t *testing.T) {
	ecs := &dns.EDNS0_SUBNET{Code: dns.EDNS0SUBNET, Family: 1, SourceNetmask: 24, Address: []byte{192, 0, 2, 0}}
	m := query(traceparent(DefaultOptionCode, 1), ecs, &dns.EDNS0_LOCAL{Code: 65400, Data: []byte{1}})
	ex := TakeOption(m, DefaultOptionCode)
	if !ex.Present || !ex.Valid || ex.Remote.TraceID != peerTrace {
		t.Fatalf("extracted %+v", ex)
	}
	opts := m.IsEdns0().Option
	if len(opts) != 2 || opts[0] != ecs || opts[1].Option() != 65400 {
		t.Fatalf("other options changed: %v", opts)
	}
	// Two copies: both removed, neither used.
	m = query(traceparent(DefaultOptionCode, 1), traceparent(DefaultOptionCode, 1))
	ex = TakeOption(m, DefaultOptionCode)
	if !ex.Present || ex.Valid || len(m.IsEdns0().Option) != 0 {
		t.Fatalf("duplicate options: %+v, left %v", ex, m.IsEdns0().Option)
	}
	// Another configured code leaves 65500 alone.
	m = query(traceparent(DefaultOptionCode, 1))
	if ex := TakeOption(m, 65001); ex.Present || len(m.IsEdns0().Option) != 1 {
		t.Fatalf("code 65001 took option 65500: %+v", ex)
	}
	// No OPT record, no message.
	plain := new(dns.Msg)
	plain.SetQuestion("a.example.", dns.TypeA)
	if ex := TakeOption(plain, DefaultOptionCode); ex.Present {
		t.Fatal("found an option without OPT")
	}
	if ex := TakeOption(nil, DefaultOptionCode); ex.Present {
		t.Fatal("found an option in nil")
	}
}

// TestBeginTrustRule pins D382: the option is removed from every query;
// only a trusted peer with acceptTraceparent continues the caller's trace,
// and its sampled flag decides (parent-based).
func TestBeginTrustRule(t *testing.T) {
	tr, _, reg := newTestTracer(t, Config{Ratio: 0, AcceptIncoming: true})
	now := time.Now()

	m := query(traceparent(DefaultOptionCode, FlagSampled))
	trace, carried := tr.Begin(m, true, now)
	if !carried || trace.ID() != peerTrace || trace.Remote() != peerSpan || !trace.Sampled() || !trace.FromPeer() {
		t.Fatalf("trusted sampled: id %s remote %s sampled %v", trace.ID(), trace.Remote(), trace.Sampled())
	}
	if len(m.IsEdns0().Option) != 0 {
		t.Fatal("trusted option not removed")
	}

	// dnsdist 2.1.3 sends TRACEFLAGS 00 for the queries it traces: the
	// option from a trusted peer is the sampling decision, not the flag.
	m = query(traceparent(DefaultOptionCode, 0))
	trace, _ = tr.Begin(m, true, now)
	if trace.ID() != peerTrace || !trace.Sampled() {
		t.Fatal("a trusted TRACEPARENT with flags 00 must be kept")
	}
	dnsdist, err := hex.DecodeString("00009af79095f1f29e1ca6455f2c2c6bf3426fd0a96ce9be898f00")
	if err != nil {
		t.Fatal(err)
	}
	m = query(&dns.EDNS0_LOCAL{Code: DefaultOptionCode, Data: dnsdist})
	trace, _ = tr.Begin(m, true, now)
	if trace.ID().String() != "9af79095f1f29e1ca6455f2c2c6bf342" || trace.Remote().String() != "6fd0a96ce9be898f" || !trace.Sampled() {
		t.Fatalf("the option dnsdist 2.1.3 sent parsed as %s/%s", trace.ID(), trace.Remote())
	}

	m = query(traceparent(DefaultOptionCode, FlagSampled))
	trace, carried = tr.Begin(m, false, now)
	if !carried || trace.ID() == peerTrace || trace.Remote().IsValid() || trace.Sampled() || trace.FromPeer() {
		t.Fatalf("untrusted option honored: id %s remote %s sampled %v", trace.ID(), trace.Remote(), trace.Sampled())
	}
	if len(m.IsEdns0().Option) != 0 {
		t.Fatal("untrusted option not removed")
	}

	m = query(&dns.EDNS0_LOCAL{Code: DefaultOptionCode, Data: []byte{0, 0, 1}})
	trace, _ = tr.Begin(m, true, now)
	if trace.ID() == peerTrace || !trace.ID().IsValid() {
		t.Fatal("malformed option not replaced by a new trace id")
	}

	off, _, offReg := newTestTracer(t, Config{Ratio: 1, AcceptIncoming: false})
	m = query(traceparent(DefaultOptionCode, 0))
	trace, _ = off.Begin(m, true, now)
	if trace.ID() == peerTrace || !trace.Sampled() || len(m.IsEdns0().Option) != 0 {
		t.Fatal("acceptTraceparent false still continued the peer trace")
	}

	for result, want := range map[string]float64{"accepted": 3, "untrusted": 1, "malformed": 1} {
		if got := testutil.ToFloat64(tr.metrics.parent[result]); got != want {
			t.Errorf("traceparent %s = %v, want %v", result, got, want)
		}
	}
	if got := testutil.ToFloat64(off.metrics.parent["ignored"]); got != 1 {
		t.Errorf("ignored = %v", got)
	}
	if n, err := testutil.GatherAndCount(reg, "kudurru_tracing_traceparent_total"); err != nil || n != 4 {
		t.Errorf("registered series %d %v", n, err)
	}
	_ = offReg
}

func TestRatioSamplerIsDeterministic(t *testing.T) {
	half, _, _ := newTestTracer(t, Config{Ratio: 0.5})
	low := TraceID{15: 1}
	high := TraceID{8: 0xff, 15: 1}
	if !half.ratioSampled(low) || half.ratioSampled(high) {
		t.Fatal("ratio 0.5 must keep the low half of the id space only")
	}
	none, _, _ := newTestTracer(t, Config{Ratio: 0})
	all, _, _ := newTestTracer(t, Config{Ratio: 1})
	if none.ratioSampled(low) || !all.ratioSampled(high) {
		t.Fatal("ratio bounds")
	}
	// A thousandth of 100000 random ids, within a wide margin.
	thousandth, _, _ := newTestTracer(t, Config{Ratio: 0.001})
	kept := 0
	for range 100000 {
		if thousandth.ratioSampled(newTraceID()) {
			kept++
		}
	}
	if kept < 40 || kept > 200 {
		t.Fatalf("ratio 0.001 kept %d of 100000", kept)
	}
}

// TestTailKeep pins the keep rule: sampled, failed or slow.
func TestTailKeep(t *testing.T) {
	tr, mem, _ := newTestTracer(t, Config{Ratio: 0, Slow: 250 * time.Millisecond, FlushInterval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go tr.Run(ctx)
	now := time.Now()
	cases := []struct {
		name    string
		latency time.Duration
		failed  bool
		keep    bool
	}{
		{"fast", time.Millisecond, false, false},
		{"failed", time.Millisecond, true, true},
		{"slow", 250 * time.Millisecond, false, true},
	}
	want := 0
	for _, c := range cases {
		trace, _ := tr.Begin(query(), false, now)
		if got := trace.Decide(c.latency, c.failed); got != c.keep || trace.Kept() != c.keep {
			t.Errorf("%s: kept %v, want %v", c.name, got, c.keep)
		}
		trace.Finish(now.Add(c.latency))
		if c.keep {
			want++
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(mem.Traces()) < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(mem.Traces()); got != want {
		t.Fatalf("exported %d traces, want %d", got, want)
	}
	if got := testutil.ToFloat64(tr.metrics.traces["not_kept"]); got != 1 {
		t.Fatalf("not_kept = %v", got)
	}
}

func TestSpanTreeAndBounds(t *testing.T) {
	tr, _, _ := newTestTracer(t, Config{Ratio: 1, AcceptIncoming: true})
	start := time.Now()
	trace, _ := tr.Begin(query(traceparent(DefaultOptionCode, 1)), true, start)
	stage := trace.Start(Root, "kudurru.cache", KindInternal, start)
	up := trace.Start(stage, "kudurru.upstream.exchange", KindClient, start)
	trace.SetStr(up, "kudurru.upstream", "default/one")
	trace.SetInt(up, "n", 7)
	trace.End(up, start.Add(time.Millisecond), "timeout")
	if trace.Spans() != nil {
		t.Fatal("spans visible before Finish")
	}
	for range MaxSpans {
		trace.Start(stage, "extra", KindInternal, start)
	}
	trace.Decide(time.Millisecond, false)
	trace.Finish(start.Add(2 * time.Millisecond))
	spans := trace.Spans()
	if len(spans) != MaxSpans || trace.DroppedSpans() != 3 {
		t.Fatalf("%d spans, %d dropped", len(spans), trace.DroppedSpans())
	}
	root, cache, exchange := spans[0], spans[1], spans[2]
	if root.Name != RootName || root.Kind != KindServer || root.Parent != peerSpan {
		t.Fatalf("root %+v", root)
	}
	if cache.Parent != root.ID || exchange.Parent != cache.ID || exchange.Kind != KindClient {
		t.Fatal("parent links")
	}
	if !exchange.Error || exchange.Message != "timeout" || len(exchange.Attrs) != 2 || !exchange.End.Equal(start.Add(time.Millisecond)) {
		t.Fatalf("exchange span %+v", exchange)
	}
	if !root.End.Equal(start.Add(2*time.Millisecond)) || !cache.End.Equal(root.End) {
		t.Fatal("open spans must end at Finish")
	}
	// A raced exchange that returns after the answer changes nothing.
	late := trace.Start(stage, "late", KindClient, start)
	trace.End(Root, start.Add(time.Hour), "late")
	trace.SetStr(Root, "late", "x")
	if late != 0 || spans[0].Error || len(spans[0].Attrs) != 0 {
		t.Fatal("a finished trace changed")
	}
}

func TestNilTraceIsInert(t *testing.T) {
	var trace *Trace
	ref := trace.Start(Root, "x", KindInternal, time.Now())
	trace.SetStr(ref, "k", "v")
	trace.End(ref, time.Now(), "")
	if trace.Decide(time.Hour, true) || trace.Kept() || trace.ID().IsValid() || trace.Spans() != nil {
		t.Fatal("nil trace did something")
	}
	trace.Finish(time.Now())
	var tracer *Tracer
	if tr, carried := tracer.Begin(query(traceparent(DefaultOptionCode, 1)), true, time.Now()); tr != nil || carried {
		t.Fatal("nil tracer began a trace")
	}
	if ctx := NewContext(context.Background(), nil, 1); ctx != context.Background() {
		t.Fatal("nil trace changed the context")
	}
}

// blocked is an exporter that waits until released.
type blocked struct{ release chan struct{} }

func (b *blocked) Export(ctx context.Context, _ []*Trace) error {
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestSlowExporterNeverBlocksFinish: with the exporter stuck, Finish
// returns at once and the overflow is counted.
func TestSlowExporterNeverBlocksFinish(t *testing.T) {
	stuck := &blocked{release: make(chan struct{})}
	tr, _, _ := newTestTracer(t, Config{Ratio: 1, QueueSize: 4, BatchSize: 1, Timeout: time.Minute, Exporter: stuck})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); tr.Run(ctx) }()
	start := time.Now()
	for range 100 {
		trace, _ := tr.Begin(query(), false, time.Now())
		trace.Decide(0, false)
		trace.Finish(time.Now())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("100 finishes took %v with a stuck exporter", elapsed)
	}
	if got := testutil.ToFloat64(tr.metrics.traces["queue_full"]); got < 90 {
		t.Fatalf("queue_full = %v", got)
	}
	cancel()
	<-done
}

func TestCloseDrainsAndFailuresAreCounted(t *testing.T) {
	mem := &Memory{}
	tr, _, _ := newTestTracer(t, Config{Ratio: 1, FlushInterval: time.Hour, Exporter: mem})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go tr.Run(ctx)
	for range 3 {
		trace, _ := tr.Begin(query(), false, time.Now())
		trace.Decide(0, false)
		trace.Finish(time.Now())
	}
	if err := tr.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := len(mem.Traces()); got != 3 {
		t.Fatalf("drained %d traces, want 3", got)
	}
	trace, _ := tr.Begin(query(), false, time.Now())
	trace.Decide(0, false)
	trace.Finish(time.Now())
	if got := testutil.ToFloat64(tr.metrics.traces["shutdown"]); got != 1 {
		t.Fatalf("shutdown = %v", got)
	}
	if err := tr.Close(t.Context()); err != nil {
		t.Fatal("second close", err)
	}

	failing := &Memory{Fail: errors.New("collector down")}
	tf, _, _ := newTestTracer(t, Config{Ratio: 1, BatchSize: 2, Exporter: failing})
	go tf.Run(ctx)
	for range 2 {
		trace, _ := tf.Begin(query(), false, time.Now())
		trace.Decide(0, false)
		trace.Finish(time.Now())
	}
	if err := tf.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(tf.metrics.traces["export_failed"]); got != 2 {
		t.Fatalf("export_failed = %v", got)
	}
}

func TestNewRefusesBadConfig(t *testing.T) {
	for name, c := range map[string]Config{
		"ratio":    {Ratio: 1.5, Exporter: &Memory{}},
		"slow":     {Slow: -1, Exporter: &Memory{}},
		"code":     {OptionCode: 8, Exporter: &Memory{}},
		"exporter": {},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if !strings.Contains(peerTrace.String(), "4bf92f35") {
		t.Fatal("hex form")
	}
}

// TestBeginAllocations bounds what a traced query costs: the Trace itself.
func TestBeginAllocations(t *testing.T) {
	tr, _, _ := newTestTracer(t, Config{Ratio: 0})
	m := query()
	now := time.Now()
	allocs := testing.AllocsPerRun(1000, func() {
		trace, _ := tr.Begin(m, false, now)
		s := trace.Start(Root, "kudurru.cache", KindInternal, now)
		trace.End(s, now, "")
		trace.Decide(0, false)
		trace.Finish(now)
	})
	if allocs > 1 {
		t.Fatalf("an unkept traced query allocates %v times, want 1", allocs)
	}
}
