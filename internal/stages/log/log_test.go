package log

import (
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
)

type recorder struct{ events []*querylog.Event }

func (r *recorder) Enqueue(ev *querylog.Event) bool { r.events = append(r.events, ev); return true }

type fakeZone struct{ name string }

func (z *fakeZone) Name() string { return z.name }

func request(name string, qtype uint16) *pipeline.Request {
	m := new(dns.Msg)
	m.SetQuestion(name, qtype)
	return &pipeline.Request{
		Msg:      m,
		Client:   pipeline.Client{Addr: netip.MustParseAddrPort("192.168.1.10:5353"), Proto: pipeline.ProtoDoT, ClientID: "laptop", GroupIDs: []string{"kids", "default"}},
		Received: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC),
	}
}

func reply(req *pipeline.Request, rcode int, rrs ...string) *dns.Msg {
	m := new(dns.Msg)
	m.SetRcode(req.Msg, rcode)
	for _, s := range rrs {
		rr, err := dns.NewRR(s)
		if err != nil {
			panic(err)
		}
		m.Answer = append(m.Answer, rr)
	}
	return m
}

func TestObserverEvent(t *testing.T) {
	rec := &recorder{}
	o := &Observer{Node: "node-a", Collector: rec}

	req := request("WWW.Example.COM.", dns.TypeA)
	opt := req.Msg.SetEdns0(1232, true).IsEdns0()
	opt.Option = append(opt.Option, &dns.EDNS0_SUBNET{Code: dns.EDNS0SUBNET, Family: 1, SourceNetmask: 24, Address: net.ParseIP("192.168.1.0").To4()})
	req.Zone = &fakeZone{name: "example.com."}
	resp := reply(req, dns.RcodeSuccess,
		"www.example.com. 60 IN CNAME edge.example.net.",
		"edge.example.net. 60 IN A 192.0.2.1",
		"edge.example.net. 60 IN A 192.0.2.2",
		"edge.example.net. 60 IN A 192.0.2.3",
		"edge.example.net. 60 IN A 192.0.2.4")
	resp.SetEdns0(1232, true)
	resp.IsEdns0().Option = append(resp.IsEdns0().Option, &dns.EDNS0_EDE{InfoCode: dns.ExtendedErrorCodeStaleAnswer})
	o.Observe(req, &pipeline.Response{Msg: resp, Status: pipeline.StatusCachedStale}, nil, 4*time.Millisecond)

	if len(rec.events) != 1 {
		t.Fatalf("%d events", len(rec.events))
	}
	ev := rec.events[0]
	if ev.Name != "www.example.com." || ev.ClientName != "laptop" || ev.ClientGroup != "kids" || ev.Zone != "example.com" ||
		ev.Proto != pipeline.ProtoDoT || ev.Client != netip.MustParseAddr("192.168.1.10") || ev.Latency != 4*time.Millisecond {
		t.Errorf("identity fields: %+v", ev)
	}
	if ev.ReplyType != querylog.ReplyIP || ev.Answers != 5 {
		t.Errorf("reply type %s, answers %d", ev.ReplyType, ev.Answers)
	}
	if want := "CNAME edge.example.net., 192.0.2.1, 192.0.2.2 (+2)"; ev.Answer != want {
		t.Errorf("answer summary %q, want %q", ev.Answer, want)
	}
	if !ev.HasEDE || ev.EDE != dns.ExtendedErrorCodeStaleAnswer {
		t.Errorf("EDE %v %d", ev.HasEDE, ev.EDE)
	}
	if ev.ECS != netip.MustParsePrefix("192.168.1.0/24") || !ev.Flags.ECS || !ev.Flags.DO || !ev.Flags.RD || !ev.Flags.Cached || !ev.Flags.Stale {
		t.Errorf("ecs %v flags %+v", ev.ECS, ev.Flags)
	}
}

// TestObserverPolicy: the deciding policy of the identify stage's
// decision reaches the event; no decision leaves it empty.
func TestObserverPolicy(t *testing.T) {
	rec := &recorder{}
	o := &Observer{Node: "n", Collector: rec}
	req := request("a.example.", dns.TypeA)
	req.Policy = &policy.Decision{Policy: "home/kids", Policies: []string{"home/kids", "default/default"}}
	o.Observe(req, &pipeline.Response{Msg: reply(req, dns.RcodeSuccess), Status: pipeline.StatusForwarded}, nil, time.Millisecond)
	req = request("b.example.", dns.TypeA)
	o.Observe(req, &pipeline.Response{Msg: reply(req, dns.RcodeSuccess), Status: pipeline.StatusForwarded}, nil, time.Millisecond)
	if got := rec.events[0].Policy; got != "home/kids" {
		t.Errorf("policy %q, want the deciding policy home/kids", got)
	}
	if got := rec.events[1].Policy; got != "" {
		t.Errorf("no decision: policy %q", got)
	}
}

func TestObserverReplyTypes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		qtype  uint16
		rcode  int
		rrs    []string
		resp   bool
		err    error
		want   querylog.ReplyType
		status pipeline.Status
	}{
		{"nxdomain", dns.TypeA, dns.RcodeNameError, nil, true, nil, querylog.ReplyNXDomain, pipeline.StatusForwarded},
		{"nodata", dns.TypeAAAA, dns.RcodeSuccess, nil, true, nil, querylog.ReplyNoData, pipeline.StatusForwarded},
		{"servfail", dns.TypeA, dns.RcodeServerFailure, nil, true, nil, querylog.ReplyServfail, pipeline.StatusServfailUpstream},
		{"refused", dns.TypeA, dns.RcodeRefused, nil, true, nil, querylog.ReplyRefused, pipeline.StatusRefusedACL},
		{"notimp", dns.TypeA, dns.RcodeNotImplemented, nil, true, nil, querylog.ReplyNotImp, pipeline.StatusForwarded},
		{"formerr", dns.TypeA, dns.RcodeFormatError, nil, true, nil, querylog.ReplyOther, pipeline.StatusForwarded},
		{"cname only", dns.TypeA, dns.RcodeSuccess, []string{"a.example. 60 IN CNAME b.example."}, true, nil, querylog.ReplyCNAME, pipeline.StatusForwarded},
		{"ptr", dns.TypePTR, dns.RcodeSuccess, []string{"1.2.0.192.in-addr.arpa. 60 IN PTR host.example."}, true, nil, querylog.ReplyDomain, pipeline.StatusForwarded},
		{"txt", dns.TypeTXT, dns.RcodeSuccess, []string{`a.example. 60 IN TXT "v=spf1 -all"`}, true, nil, querylog.ReplyBlob, pipeline.StatusForwarded},
		{"dropped", dns.TypeA, 0, nil, false, errors.New("timeout"), querylog.ReplyNone, pipeline.StatusDropped},
	} {
		rec := &recorder{}
		o := &Observer{Node: "n", Collector: rec}
		req := request("a.example.", tc.qtype)
		var resp *pipeline.Response
		if tc.resp {
			resp = &pipeline.Response{Msg: reply(req, tc.rcode, tc.rrs...), Status: tc.status}
		}
		o.Observe(req, resp, tc.err, time.Millisecond)
		if got := rec.events[0]; got.ReplyType != tc.want || got.Status != tc.status {
			t.Errorf("%s: reply %s status %s, want %s %s", tc.name, got.ReplyType, got.Status, tc.want, tc.status)
		}
	}
}

func TestObserverSkipsHealthName(t *testing.T) {
	rec := &recorder{}
	o := &Observer{Node: "n", Collector: rec}
	o.Observe(request(ingress.HealthName, dns.TypeTXT), nil, nil, time.Millisecond)
	if len(rec.events) != 0 {
		t.Error("health probe was logged")
	}
}

// TestObserverSkipsSyntheticRequests is the regression test of the M5
// finding "the built-in dig forges query-log rows": POST /api/v1/dns/query
// (stats.read, every viewer) runs a query as any client address the caller
// names, and the log stage recorded it as that client's traffic.
func TestObserverSkipsSyntheticRequests(t *testing.T) {
	rec := &recorder{}
	o := &Observer{Node: "n", Collector: rec}
	req := request("www.example.com.", dns.TypeA)
	req.Client.Synthetic = true
	o.Observe(req, &pipeline.Response{Msg: reply(req, dns.RcodeSuccess), Status: pipeline.StatusForwarded}, nil, time.Millisecond)
	if len(rec.events) != 0 {
		t.Errorf("a synthetic request was logged as %+v", rec.events[0])
	}
}

func TestZoneNameTypedNil(t *testing.T) {
	var z *fakeZone
	if got := zoneName(z); got != "" {
		t.Errorf("typed nil zone: %q", got)
	}
	if got := zoneName(nil); got != "" {
		t.Errorf("nil zone: %q", got)
	}
}
