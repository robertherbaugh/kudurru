package api

import (
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
)

func TestBlockingToggle(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	bs, err := blockingstage.New(blockingstage.Options{Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(o *Options) { o.Blocking = bs; o.Now = clock })
	var st BlockingState
	h.do(http.MethodGet, "/api/v1/dns/blocking", "viewer", nil).want(http.StatusOK).json(&st)
	if !st.Blocking || st.Timer != nil || st.Scope != "node" || st.Node != "node-a" {
		t.Fatalf("initial %+v", st)
	}
	h.do(http.MethodPost, "/api/v1/dns/blocking", "operator", map[string]any{"blocking": false, "timer": 300}).want(http.StatusOK).json(&st)
	if st.Blocking || st.Timer == nil || *st.Timer != 300 || st.Until == nil || !st.Until.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("disabled for 5m: %+v", st)
	}
	if bs.Enabled() {
		t.Fatal("stage still enabled")
	}
	st = BlockingState{}
	h.do(http.MethodPost, "/api/v1/dns/blocking", "operator", map[string]any{"blocking": false}).want(http.StatusOK).json(&st)
	if st.Blocking || st.Timer != nil {
		t.Fatalf("disabled indefinitely: %+v", st)
	}
	until := now.Add(time.Hour)
	h.do(http.MethodPost, "/api/v1/dns/blocking", "operator", map[string]any{"blocking": false, "until": until.Format(time.RFC3339)}).want(http.StatusOK).json(&st)
	if st.Until == nil || !st.Until.Equal(until) {
		t.Fatalf("until: %+v", st)
	}
	h.do(http.MethodPost, "/api/v1/dns/blocking", "operator", map[string]any{"blocking": false, "until": now.Add(-time.Hour).Format(time.RFC3339)}).want(http.StatusBadRequest)
	st = BlockingState{}
	h.do(http.MethodPost, "/api/v1/dns/blocking", "operator", map[string]any{"blocking": true}).want(http.StatusOK).json(&st)
	if !st.Blocking || !bs.Enabled() {
		t.Fatalf("enabled: %+v", st)
	}
	// Every toggle is audited with before and after.
	var toggles int
	for _, ev := range h.audit.Events() {
		if ev.Action == "blocking.toggle" && ev.Result == "success" {
			toggles++
			if len(ev.Before) == 0 || len(ev.After) == 0 || ev.Actor.Name != "operator" {
				t.Errorf("audit event %+v", ev)
			}
		}
	}
	if toggles != 4 {
		t.Fatalf("%d toggle audit events", toggles)
	}
}

func TestCacheFlush(t *testing.T) {
	rc := cache.New(cache.Options{MaxEntries: 1000})
	put := func(name string, qtype uint16) {
		m := new(dns.Msg)
		m.SetQuestion(name, qtype)
		wire, err := m.Pack()
		if err != nil {
			t.Fatal(err)
		}
		rc.Set(cache.MakeKey(name, qtype, dns.ClassINET, false, false, netip.Prefix{}), wire, cache.Positive, time.Minute)
	}
	put("a.example.", dns.TypeA)
	put("a.example.", dns.TypeAAAA)
	put("b.example.", dns.TypeA)
	h := newHarness(t, func(o *Options) { o.ResponseCache = rc })
	var res FlushResult
	h.do(http.MethodPost, "/api/v1/cache/flush", "operator", map[string]any{"name": "A.example"}).want(http.StatusOK).json(&res)
	if res.Flushed != 2 || rc.Len() != 1 {
		t.Fatalf("flush by name: %+v, %d left", res, rc.Len())
	}
	h.do(http.MethodPost, "/api/v1/cache/flush", "operator", nil).want(http.StatusOK).json(&res)
	if res.Flushed != 1 || rc.Len() != 0 {
		t.Fatalf("flush all: %+v", res)
	}
	h.do(http.MethodPost, "/api/v1/cache/flush", "viewer", nil).want(http.StatusForbidden)
}

// answerStage answers every query from a fixed A record, as an upstream
// would.
type answerStage struct{}

func (answerStage) Name() string { return "upstream" }

func (answerStage) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	m := new(dns.Msg)
	m.SetReply(req.Msg)
	m.RecursionAvailable = true
	rr, _ := dns.NewRR(req.Question().Name + " 60 IN A 192.0.2.1")
	m.Answer = append(m.Answer, rr)
	return &pipeline.Response{Msg: m, Status: pipeline.StatusForwarded, Upstream: "default"}, nil
}

// clientStage records the client the pipeline saw.
type clientStage struct{ seen *netip.AddrPort }

func (clientStage) Name() string { return "ingress" }

func (c clientStage) Serve(req *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	*c.seen = req.Client.Addr
	return next(req)
}

func TestDNSQuery(t *testing.T) {
	var seen netip.AddrPort
	chain := pipeline.MustNew(clientStage{&seen}, answerStage{})
	h := newHarness(t, func(o *Options) { o.Chain = chain })
	var res DNSQueryResult
	h.do(http.MethodPost, "/api/v1/dns/query", "viewer", map[string]any{"name": "www.example.com", "type": "a", "client": "192.168.1.20"}).
		want(http.StatusOK).json(&res)
	if res.Rcode != "NOERROR" || res.Status != "forwarded" || res.StatusClass != "permitted" || res.Upstream != "default" ||
		len(res.Answer) != 1 || res.Name != "www.example.com." || res.Node != "node-a" || !res.Flags.RA {
		t.Fatalf("query %+v", res)
	}
	if seen.Addr() != netip.MustParseAddr("192.168.1.20") {
		t.Fatalf("pipeline saw client %v", seen)
	}
	h.do(http.MethodPost, "/api/v1/dns/query", "viewer", map[string]any{"name": "www.example.com"}).want(http.StatusOK).json(&res)
	if seen.Addr() != netip.MustParseAddr("127.0.0.1") || res.Type != "A" {
		t.Fatalf("default client %v type %s", seen, res.Type)
	}
	h.do(http.MethodPost, "/api/v1/dns/query", "viewer", map[string]any{"name": "x", "type": "NOPE"}).want(http.StatusBadRequest)
	h.do(http.MethodPost, "/api/v1/dns/query", "viewer", map[string]any{"name": "x", "client": "nope"}).want(http.StatusBadRequest)
}
