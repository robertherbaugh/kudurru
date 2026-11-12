package pipeline

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/tracing"
)

// fullTracedChain is every stage of Order as a pass-through, ending at an
// upstream stage that records two raced attempts the way internal/upstream
// does (a trace context, a client span with three attributes each).
func fullTracedChain(tracer *tracing.Tracer) *Chain {
	resp := &Response{Msg: new(dns.Msg), Status: StatusForwarded}
	var stages []Stage
	for _, name := range Order {
		if name == "upstream" {
			stages = append(stages, named{name: name, fn: func(r *Request, _ Next) (*Response, error) {
				ctx := tracing.NewContext(r.Ctx, r.Trace, r.TraceSpan)
				tr, under := tracing.FromContext(ctx)
				for range 2 {
					a := tr.Start(under, "kudurru.upstream.exchange", tracing.KindClient, time.Now())
					tr.SetStr(a, "kudurru.upstream", "default/one")
					tr.SetStr(a, "network.protocol.name", "udp")
					tr.SetStr(a, "dns.response.code", "NOERROR")
					tr.End(a, time.Now(), "")
				}
				return resp, nil
			}})
			continue
		}
		stages = append(stages, named{name: name, fn: func(r *Request, next Next) (*Response, error) { return next(r) }})
	}
	chain := MustNew(stages...)
	if tracer != nil {
		chain.SetTracer(tracer, func() (bool, bool) { return true, true })
	}
	return chain
}

// TestTracedChainAllocations bounds what tracing costs a query that is not
// kept (the 99.9 % case at the default ratio), measured over the full
// stage order rather than one bare span: the Trace (spans and attributes
// live in its inline arrays) and the trace context the upstream stage
// hands to its attempts. Anything more is a per-query regression.
func TestTracedChainAllocations(t *testing.T) {
	tracer, err := tracing.New(tracing.Config{Ratio: 0, Exporter: &tracing.Memory{}})
	if err != nil {
		t.Fatal(err)
	}
	q := new(dns.Msg)
	q.SetQuestion("www.example.org.", dns.TypeA)
	q.SetEdns0(1232, false)
	client := Client{Addr: netip.MustParseAddrPort("192.0.2.7:5353")}
	measure := func(chain *Chain) float64 {
		return testing.AllocsPerRun(500, func() {
			req := &Request{Msg: q, Client: client, Ctx: context.Background(), Received: time.Now()}
			if _, err := chain.Serve(req); err != nil {
				t.Fatal(err)
			}
		})
	}
	off, on := measure(fullTracedChain(nil)), measure(fullTracedChain(tracer))
	if extra := on - off; extra > 2 {
		t.Fatalf("an unkept traced query allocates %v times more than an untraced one (%v against %v), want at most 2", extra, on, off)
	}
}

// BenchmarkTracedChain compares a query through the full stage order with
// tracing off and with tracing on but the query not kept.
func BenchmarkTracedChain(b *testing.B) {
	tracer, err := tracing.New(tracing.Config{Ratio: 0, Exporter: &tracing.Memory{}})
	if err != nil {
		b.Fatal(err)
	}
	q := new(dns.Msg)
	q.SetQuestion("www.example.org.", dns.TypeA)
	q.SetEdns0(1232, false)
	for _, c := range []struct {
		name   string
		tracer *tracing.Tracer
	}{{"off", nil}, {"unkept", tracer}} {
		chain := fullTracedChain(c.tracer)
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				req := &Request{Msg: q, Ctx: context.Background(), Received: time.Now()}
				if _, err := chain.Serve(req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
