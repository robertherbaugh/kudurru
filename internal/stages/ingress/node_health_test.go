package ingress

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/nodehealth"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func TestTargetedHealthUsesCapturedOwner(t *testing.T) {
	owner := "node-a"
	stage := &Stage{NodeID: owner, Epoch: func() uint64 { return 42 }}
	owner = "node-b" // a later configuration owner never changes this pipeline
	calls := 0
	next := func(req *pipeline.Request) (*pipeline.Response, error) {
		calls++
		return pipeline.Reply(req, dns.RcodeSuccess, pipeline.StatusForwarded), nil
	}
	for _, name := range []string{nodehealth.Target("node-a"), strings.ToUpper(nodehealth.Target("node-a")), HealthName} {
		req := udpReq("192.0.2.1:1", name)
		req.Msg.Question[0].Qtype = dns.TypeTXT
		response, err := stage.Serve(req, next)
		if err != nil || response.Msg.Rcode != dns.RcodeSuccess || !response.Msg.Authoritative || len(response.Msg.Answer) != 1 || response.Status != pipeline.StatusSpecialDomain {
			t.Fatal("captured node proof was not local authoritative health")
		}
		txt, ok := response.Msg.Answer[0].(*dns.TXT)
		if !ok || txt.Hdr.Ttl != 0 || len(txt.Txt) != 1 || txt.Txt[0] != "node-a 42" || txt.Hdr.Name != name {
			t.Fatal("original health TXT semantics changed")
		}
	}
	for _, tc := range []struct {
		name        string
		kind, class uint16
		opcode      int
	}{
		{nodehealth.Target(owner), dns.TypeTXT, dns.ClassINET, dns.OpcodeQuery},
		{"malformed." + nodehealth.Namespace, dns.TypeTXT, dns.ClassINET, dns.OpcodeQuery},
		{"x.v2." + nodehealth.Namespace, dns.TypeTXT, dns.ClassINET, dns.OpcodeQuery},
		{nodehealth.Target("node-a"), dns.TypeA, dns.ClassINET, dns.OpcodeQuery},
		{nodehealth.Target("node-a"), dns.TypeTXT, dns.ClassCHAOS, dns.OpcodeQuery},
		{nodehealth.Target("node-a"), dns.TypeTXT, dns.ClassINET, dns.OpcodeNotify},
	} {
		req := udpReq("192.0.2.1:1", tc.name)
		req.Msg.Question[0].Qtype = tc.kind
		req.Msg.Question[0].Qclass = tc.class
		req.Msg.Opcode = tc.opcode
		response, err := stage.Serve(req, next)
		if err != nil || response.Msg.Rcode != dns.RcodeRefused || !response.Msg.Authoritative || len(response.Msg.Answer) != 0 {
			t.Fatal("wrong/malformed/unsupported target was not refused locally")
		}
	}
	if calls != 0 {
		t.Fatal("health name escaped to resolution")
	}
}

func TestTargetedHealthRetainsDrainAndRateLimitRules(t *testing.T) {
	limiter, _ := newLimiter(t, RateLimitConfig{IPv4: map[int]int{32: 1}, Slip: 0})
	var draining atomic.Bool
	stage := &Stage{NodeID: "node-a", Limiter: limiter, Lameduck: draining.Load}
	next := func(req *pipeline.Request) (*pipeline.Response, error) {
		return pipeline.Reply(req, dns.RcodeSuccess, pipeline.StatusForwarded), nil
	}
	for range 3 {
		_, _ = stage.Serve(udpReq("192.0.2.1:1", "ordinary.example."), next)
	}
	for _, name := range []string{nodehealth.Target("node-a"), nodehealth.Target("node-b"), HealthName} {
		req := udpReq("192.0.2.1:1", name)
		req.Msg.Question[0].Qtype = dns.TypeTXT
		response, err := stage.Serve(req, next)
		if err != nil || response.Msg == nil || response.Msg.Truncated || response.Status != pipeline.StatusSpecialDomain {
			t.Fatal("health probe lost original rate-limit exemption")
		}
	}
	draining.Store(true)
	for _, kind := range []uint16{dns.TypeTXT, dns.TypeA} {
		req := udpReq("192.0.2.1:1", nodehealth.Target("node-a"))
		req.Msg.Question[0].Qtype = kind
		response, err := stage.Serve(req, next)
		if err != nil || response.Msg.Rcode != dns.RcodeServerFailure || len(response.Msg.Answer) != 0 {
			t.Fatal("draining targeted health remained usable")
		}
	}
	empty := &Stage{}
	req := udpReq("192.0.2.1:1", nodehealth.Target("node-a"))
	req.Msg.Question[0].Qtype = dns.TypeTXT
	response, err := empty.Serve(req, next)
	if err != nil || response.Msg.Rcode != dns.RcodeRefused {
		t.Fatal("empty local owner fabricated a proof")
	}
}
