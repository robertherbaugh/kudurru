package pipeline

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/tracing"
)

var (
	edgeTrace = tracing.TraceID{0xaa, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	edgeSpan  = tracing.SpanID{0xbb, 1, 2, 3, 4, 5, 6, 7}
)

func tracedQuery(flags byte) *dns.Msg {
	q := new(dns.Msg)
	q.SetQuestion("www.example.org.", dns.TypeA)
	q.SetEdns0(1232, false)
	opt := q.IsEdns0()
	opt.Option = append(opt.Option, &dns.EDNS0_LOCAL{Code: tracing.DefaultOptionCode, Data: tracing.AppendOption(nil, edgeTrace, edgeSpan, flags)})
	return q
}

type seen struct {
	trace    *tracing.Trace
	option   bool
	carried  bool
	observed tracing.TraceID
}

// observerFunc records what the log observer sees.
type observerFunc func(*Request, *Response, error, time.Duration)

func (f observerFunc) Observe(r *Request, resp *Response, err error, d time.Duration) {
	f(r, resp, err, d)
}

func tracedChain(t *testing.T, tracer *tracing.Tracer, terminal Status) (*Chain, *seen) {
	t.Helper()
	s := &seen{}
	pass := func(name string) Stage {
		return named{name: name, fn: func(r *Request, next Next) (*Response, error) { return next(r) }}
	}
	upstream := named{name: "upstream", fn: func(r *Request, _ Next) (*Response, error) {
		s.trace = r.Trace
		s.carried = r.Traceparent
		for _, o := range r.Msg.IsEdns0().Option {
			if o.Option() == tracing.DefaultOptionCode {
				s.option = true
			}
		}
		// An attempt span under the stage's span, as internal/upstream adds.
		tr, parent := tracing.FromContext(tracing.NewContext(context.Background(), r.Trace, r.TraceSpan))
		a := tr.Start(parent, "kudurru.upstream.exchange", tracing.KindClient, time.Now())
		tr.End(a, time.Now(), "")
		rcode := dns.RcodeSuccess
		if terminal == StatusServfailUpstream {
			rcode = dns.RcodeServerFailure
		}
		return Reply(r, rcode, terminal), nil
	}}
	chain := MustNew(pass("ingress"), pass("postprocess"), pass("cache"), upstream)
	chain.SetTracer(tracer, func() (bool, bool) { return true, false })
	chain.Observe(observerFunc(func(r *Request, _ *Response, _ error, _ time.Duration) {
		if r.Trace.Kept() {
			s.observed = r.Trace.ID()
		}
	}))
	return chain, s
}

func newTracer(t *testing.T, ratio float64) (*tracing.Tracer, *tracing.Memory) {
	t.Helper()
	mem := &tracing.Memory{}
	tr, err := tracing.New(tracing.Config{Ratio: ratio, AcceptIncoming: true, Exporter: mem, FlushInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); tr.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return tr, mem
}

func waitTraces(t *testing.T, mem *tracing.Memory, n int) []*tracing.Trace {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(mem.Traces()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("exported %d traces, want %d", len(mem.Traces()), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return mem.Traces()
}

// TestChainTracesTrustedEdgeQuery: a TRACEPARENT from an authenticated
// PROXYv2 peer joins the edge's trace; every stage is a span of a tree
// rooted at the server span, and the option never reaches a stage.
func TestChainTracesTrustedEdgeQuery(t *testing.T) {
	tracer, mem := newTracer(t, 0)
	chain, s := tracedChain(t, tracer, StatusForwarded)
	req := &Request{Msg: tracedQuery(tracing.FlagSampled), Client: Client{Addr: netip.MustParseAddrPort("192.0.2.7:5353"), ProxiedFrom: netip.MustParseAddr("10.0.0.2")}}
	if _, err := chain.Serve(req); err != nil {
		t.Fatal(err)
	}
	if s.option || !s.carried || s.trace.ID() != edgeTrace || s.observed != edgeTrace {
		t.Fatalf("stage saw option %v carried %v trace %s observer %s", s.option, s.carried, s.trace.ID(), s.observed)
	}
	if req.Trace != nil {
		t.Fatal("the request kept its trace after Serve")
	}
	spans := waitTraces(t, mem, 1)[0].Spans()
	names := []string{tracing.RootName, "kudurru.ingress", "kudurru.postprocess", "kudurru.cache", "kudurru.upstream", "kudurru.upstream.exchange"}
	if len(spans) != len(names) {
		t.Fatalf("%d spans: %+v", len(spans), spans)
	}
	parent := edgeSpan
	for i, sp := range spans {
		if sp.Name != names[i] || sp.Parent != parent || sp.End.Before(sp.Start) {
			t.Fatalf("span %d %s parent %s, want %s under %s", i, sp.Name, sp.Parent, names[i], parent)
		}
		parent = sp.ID
	}
	attrs := map[string]string{}
	for _, a := range spans[0].Attrs {
		attrs[a.Key] = a.Str
	}
	if attrs["dns.question.name"] != "www.example.org." || attrs["kudurru.status"] != "forwarded" || attrs["dns.response.code"] != "NOERROR" {
		t.Fatalf("root attributes %v", attrs)
	}
	if _, ok := attrs["client.address"]; ok {
		t.Fatal("client address exported although privacy forbids it")
	}
}

// TestChainIgnoresUntrustedTraceparent: the same option from a client
// without an authenticated PROXYv2 header (or a synthetic dig) is removed
// and ignored; with ratio 0 nothing is exported for a successful query.
func TestChainIgnoresUntrustedTraceparent(t *testing.T) {
	tracer, mem := newTracer(t, 0)
	chain, s := tracedChain(t, tracer, StatusForwarded)
	for _, client := range []Client{
		{Addr: netip.MustParseAddrPort("192.0.2.7:5353")},
		{Addr: netip.MustParseAddrPort("192.0.2.7:5353"), ProxiedFrom: netip.MustParseAddr("10.0.0.2"), Synthetic: true},
	} {
		if _, err := chain.Serve(&Request{Msg: tracedQuery(tracing.FlagSampled), Client: client}); err != nil {
			t.Fatal(err)
		}
		if s.option || !s.carried || s.trace.ID() == edgeTrace || s.trace.FromPeer() || s.observed.IsValid() {
			t.Fatalf("untrusted option honored: option %v trace %s", s.option, s.trace.ID())
		}
	}
	// A failed query is kept by the tail rule, under its own trace id.
	chain, s = tracedChain(t, tracer, StatusServfailUpstream)
	if _, err := chain.Serve(&Request{Msg: tracedQuery(tracing.FlagSampled), Client: Client{Addr: netip.MustParseAddrPort("192.0.2.7:5353")}}); err != nil {
		t.Fatal(err)
	}
	kept := waitTraces(t, mem, 1)
	if len(kept) != 1 || kept[0].ID() == edgeTrace || kept[0].ID() != s.observed {
		t.Fatalf("kept %d traces", len(kept))
	}
	root := kept[0].Spans()[0]
	if !root.Error || root.Parent.IsValid() {
		t.Fatalf("root %+v", root)
	}
}

// TestChainWithoutTracerLeavesTheQuery: tracing off reads and removes
// nothing, and an untraced query gets no trace.
func TestChainWithoutTracerLeavesTheQuery(t *testing.T) {
	chain, s := tracedChain(t, nil, StatusForwarded)
	if _, err := chain.Serve(&Request{Msg: tracedQuery(tracing.FlagSampled), Client: Client{ProxiedFrom: netip.MustParseAddr("10.0.0.2")}}); err != nil {
		t.Fatal(err)
	}
	if !s.option || s.carried || s.trace != nil {
		t.Fatal("tracing off changed the query")
	}
}
