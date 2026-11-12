package pipeline

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/tracing"
)

// TestTailKeepFollowsStatusNotRcode pins the D382 tail rule: a trace is
// kept for failure by the resolution-failure statuses (and a pipeline
// error), not by a SERVFAIL rcode alone. The ingress stage answers the
// reserved health names SERVFAIL with StatusSpecialDomain while the node
// drains, and dnsdist and keepalived probe those every second; a cached
// SERVFAIL is a cache hit. Neither may become a kept, exported trace.
func TestTailKeepFollowsStatusNotRcode(t *testing.T) {
	for _, c := range []struct {
		status Status
		kept   bool
	}{
		{StatusSpecialDomain, false},
		{StatusCached, false},
		{StatusServfailUpstream, true},
		{StatusTimeoutUpstream, true},
		{StatusDNSSECBogus, true},
		{StatusServfailUnsynced, true},
	} {
		t.Run(c.status.String(), func(t *testing.T) {
			tracer, err := tracing.New(tracing.Config{Ratio: 0, Exporter: &tracing.Memory{}})
			if err != nil {
				t.Fatal(err)
			}
			chain := MustNew(named{name: "ingress", fn: func(r *Request, _ Next) (*Response, error) {
				return Reply(r, dns.RcodeServerFailure, c.status), nil
			}})
			chain.SetTracer(tracer, nil)
			var kept bool
			chain.Observe(observerFunc(func(r *Request, _ *Response, _ error, _ time.Duration) { kept = r.Trace.Kept() }))
			q := new(dns.Msg)
			q.SetQuestion("health.kudurru.internal.", dns.TypeA)
			req := &Request{Msg: q, Ctx: context.Background(), Client: Client{Addr: netip.MustParseAddrPort("127.0.0.1:5353")}}
			if _, err := chain.Serve(req); err != nil {
				t.Fatal(err)
			}
			if kept != c.kept {
				t.Fatalf("SERVFAIL with status %s kept = %v, want %v", c.status, kept, c.kept)
			}
		})
	}
}
