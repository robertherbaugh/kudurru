package blocking

import (
	"errors"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
)

// recorder is a pipeline.Observer keeping every final outcome.
type recorder struct {
	mu  sync.Mutex
	got []observed
}

type observed struct {
	name   string
	status pipeline.Status
	err    error
	groups []string
	policy string
}

func (r *recorder) Observe(req *pipeline.Request, resp *pipeline.Response, err error, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o := observed{name: req.Question().Name, err: err, groups: req.Client.GroupIDs}
	if resp != nil {
		o.status = resp.Status
	} else if err != nil {
		o.status = pipeline.StatusDropped
	}
	if d := policy.FromRequest(req); d != nil {
		o.policy = d.Policy
	}
	r.got = append(r.got, o)
}

// TestEndToEnd runs ingress → identify → control → blocking → upstream
// (fake) and checks what the query log observer sees for each outcome.
func TestEndToEnd(t *testing.T) {
	c := newChain(t, fixture(t), Options{})
	if got, want := c.chain.Stages(), []string{"ingress", "identify", "control", "blocking", "upstream"}; !slices.Equal(got, want) {
		t.Fatalf("stages %v", got)
	}
	rec := &recorder{}
	c.chain.Observe(rec)

	type step struct {
		from   string
		name   string
		qtype  uint16
		mutate func(*dns.Msg)
		rcode  int
		status pipeline.Status
		drop   bool
	}
	steps := []step{
		{from: "10.0.0.5", name: "www.example.com.", qtype: dns.TypeA, rcode: dns.RcodeSuccess, status: pipeline.StatusForwarded},
		{from: "10.0.0.5", name: "x.ads.example.", qtype: dns.TypeA, rcode: dns.RcodeSuccess, status: pipeline.StatusBlockedList},
		{from: "10.0.0.5", name: "cname.example.com.", qtype: dns.TypeA, rcode: dns.RcodeSuccess, status: pipeline.StatusBlockedCNAME},
		{from: "10.0.20.5", name: "www.example.com.", qtype: dns.TypeA, rcode: dns.RcodeSuccess, status: pipeline.StatusBlockedList},
		{from: "203.0.113.1", name: "www.example.com.", qtype: dns.TypeA, rcode: dns.RcodeRefused, status: pipeline.StatusRefusedACL},
		{from: "10.0.0.5", name: "www.example.com.", qtype: dns.TypeHINFO, rcode: dns.RcodeRefused, status: pipeline.StatusRefusedACL},
		{from: "10.0.0.5", name: "www.example.com.", qtype: dns.TypeA, mutate: func(m *dns.Msg) { m.Response = true }, drop: true, status: pipeline.StatusDropped},
		{from: "10.0.0.5", name: "www.example.com.", qtype: dns.TypeANY, rcode: dns.RcodeRefused, status: pipeline.StatusRefusedACL},
		{from: "10.0.0.5", name: "health.kudurru.internal.", qtype: dns.TypeTXT, rcode: dns.RcodeSuccess, status: pipeline.StatusSpecialDomain},
	}
	for _, s := range steps {
		req := &pipeline.Request{Msg: query(s.name, s.qtype, true), Client: pipeline.Client{Addr: addrPort(s.from)}}
		if s.mutate != nil {
			s.mutate(req.Msg)
		}
		resp, err := c.chain.Serve(req)
		if s.drop {
			if resp != nil || !errors.Is(err, pipeline.ErrDrop) {
				t.Errorf("%s from %s: expected a silent drop, got %v %v", s.name, s.from, resp, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if resp.Msg.Rcode != s.rcode || resp.Status != s.status {
			t.Errorf("%s %s from %s: %s/%s, want %s/%s", s.name, dns.TypeToString[s.qtype], s.from,
				dns.RcodeToString[resp.Msg.Rcode], resp.Status, dns.RcodeToString[s.rcode], s.status)
		}
		if resp.Msg.Id != req.Msg.Id {
			t.Errorf("%s: response ID %d for query %d", s.name, resp.Msg.Id, req.Msg.Id)
		}
	}

	if len(rec.got) != len(steps) {
		t.Fatalf("observer saw %d queries, want %d", len(rec.got), len(steps))
	}
	for i, s := range steps {
		if rec.got[i].status != s.status {
			t.Errorf("observer %d (%s): status %s, want %s", i, s.name, rec.got[i].status, s.status)
		}
	}
	// Identification reaches the observer (query log): groups and policy.
	if o := rec.got[3]; !slices.Equal(o.groups, []string{"default/kids"}) || o.policy != "default/kids" {
		t.Errorf("kids query observed as %v %q", o.groups, o.policy)
	}
	// The upstream saw only what passed: the plain query and the CNAME
	// query (blocked on the way back).
	if want := []string{"www.example.com. A", "cname.example.com. A"}; !slices.Equal(c.up.seen, want) {
		t.Errorf("upstream saw %q, want %q", c.up.seen, want)
	}
}

func TestPipelineOrderIncludesNewStages(t *testing.T) {
	st, _ := New(Options{})
	if _, err := pipeline.New(st, &fakeUpstream{}); err != nil {
		t.Fatal(err)
	}
	// blocking must come after authoritative and before cache/upstream.
	if _, err := pipeline.New(&fakeUpstream{}, st); err == nil {
		t.Fatal("blocking after upstream must be rejected")
	}
}

func addrPort(s string) netip.AddrPort { return netip.AddrPortFrom(netip.MustParseAddr(s), 40000) }
