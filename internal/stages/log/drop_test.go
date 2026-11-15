package log

import (
	"errors"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/robertherbaugh/kudurru/internal/metrics"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
)

// TestDropsCountRcodeNone: rate-limit and control drops send nothing, so
// kudurru_dns_responses_total labels them rcode="none"; an ordinary
// pipeline error is answered SERVFAIL and counted as such.
func TestDropsCountRcodeNone(t *testing.T) {
	m := metrics.NewDNS(prometheus.NewRegistry())
	o := &Observer{Node: "n", Metrics: m}
	req := request("a.example.", dns.TypeA)
	o.Observe(req, &pipeline.Response{Status: pipeline.StatusBlockedRateLimit}, nil, time.Millisecond)
	o.Observe(req, nil, pipeline.ErrDrop, time.Millisecond)
	o.Observe(req, nil, errors.New("boom"), time.Millisecond)
	tc := pipeline.Reply(req, dns.RcodeSuccess, pipeline.StatusBlockedRateLimit) // the slip: TC=1 is sent
	o.Observe(req, tc, nil, time.Millisecond)

	for _, c := range []struct {
		rcode, status string
		want          float64
	}{
		{RcodeNone, "blocked_rate_limit", 1},
		{RcodeNone, "dropped", 1},
		{"SERVFAIL", "dropped", 1},
		{"NOERROR", "blocked_rate_limit", 1},
		{"SERVFAIL", "blocked_rate_limit", 0},
	} {
		if got := testutil.ToFloat64(m.Responses.WithLabelValues("dot", c.rcode, c.status)); got != c.want {
			t.Errorf("responses{rcode=%s,status=%s} = %v, want %v", c.rcode, c.status, got, c.want)
		}
	}
}

func TestRPZDropRetainsPolicyDiagnostic(t *testing.T) {
	m := metrics.NewDNS(prometheus.NewRegistry())
	rec := &recorder{}
	o := &Observer{Node: "n", Metrics: m, Collector: rec}
	req := request("a.example.", dns.TypeA)
	out := &pipeline.Response{Status: pipeline.StatusBlockedRPZ, List: "dns/feed", Rule: "a.policy.test. drop"}
	o.Observe(req, out, pipeline.ErrDrop, time.Millisecond)
	if len(rec.events) != 1 {
		t.Fatal("missing drop event")
	}
	ev := rec.events[0]
	if ev.Status != pipeline.StatusBlockedRPZ || ev.List != out.List || ev.Rule != out.Rule || ev.ReplyType != querylog.ReplyNone || ev.Answers != 0 {
		t.Fatalf("lost drop diagnostic: %+v", ev)
	}
	if got := testutil.ToFloat64(m.Responses.WithLabelValues("dot", RcodeNone, "blocked_rpz")); got != 1 {
		t.Fatalf("drop metric=%v", got)
	}
}
