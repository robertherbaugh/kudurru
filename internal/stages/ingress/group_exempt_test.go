package ingress

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGroupExemptDoesNotConsumeSharedBudgetsOrSkipValidation(t *testing.T) {
	g := &public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "busy", Namespace: "default"}, Spec: public.ClientGroupSpec{RateLimitExempt: true, Clients: []public.ClientMatcher{{IP: "192.0.2.1"}}}}
	id := identify.New(policy.Static{Idx: policy.Build(policy.Input{ClientGroups: []*public.ClientGroup{g}}, policy.Options{})}, nil)
	limiter, _ := newLimiter(t, RateLimitConfig{IPv4: map[int]int{24: 1}, Slip: 100})
	stage := &Stage{Limiter: limiter, GroupExempt: id.RateLimitExempt}
	next := func(r *pipeline.Request) (*pipeline.Response, error) {
		return pipeline.Reply(r, dns.RcodeSuccess, pipeline.StatusForwarded), nil
	}
	for range 50 {
		if r, e := stage.Serve(udpReq("192.0.2.1:53", "example."), next); e != nil || r.Status != pipeline.StatusForwarded {
			t.Fatal("exempt client limited", e)
		}
	}
	if limiter.Buckets() != 0 {
		t.Fatal("exempt traffic allocated limiter buckets")
	}
	// A forged label cannot exempt a neighbor, and exempt traffic did not spend
	// the neighbor's shared /24 token. A slip still contains only TC=1.
	r := udpReq("192.0.2.2:53", "example.")
	r.Client.GroupIDs = []string{"default/busy"}
	if out, e := stage.Serve(r, next); e != nil || out.Status != pipeline.StatusForwarded {
		t.Fatal("shared token was consumed by exempt traffic")
	}
	if out, e := stage.Serve(udpReq("192.0.2.2:53", "example."), next); e != nil || !out.Msg.Truncated || len(out.Msg.Answer) != 0 {
		t.Fatal("non-exempt prefix escaped slip")
	}
	malformed := udpReq("192.0.2.1:53", "example.")
	malformed.Msg.Question = nil
	if out, e := stage.Serve(malformed, next); e != nil || out.Msg.Rcode != dns.RcodeFormatError {
		t.Fatal("exemption bypassed sanity checks")
	}
	if limiter.Buckets() != 1 {
		t.Fatal("group exemption changed prefix bucket accounting")
	}
}

func TestGlobalExemptionsPrecedeGroupLookupAndCaptureFailureCloses(t *testing.T) {
	l, _ := newLimiter(t, RateLimitConfig{IPv4: map[int]int{32: 1}, Exempt: []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}})
	s := &Stage{Limiter: l, GroupExempt: func(*pipeline.Request) (bool, error) {
		t.Fatal("globally exempt query consulted group source")
		return false, nil
	}}
	next := func(r *pipeline.Request) (*pipeline.Response, error) {
		return pipeline.Reply(r, dns.RcodeSuccess, pipeline.StatusForwarded), nil
	}
	for _, r := range []*pipeline.Request{udpReq("127.0.0.1:53", "example."), udpReq("198.51.100.1:53", "example."), udpReq("192.0.2.1:53", HealthName)} {
		if _, e := s.Serve(r, next); e != nil {
			t.Fatal(e)
		}
	}
	tcp := udpReq("192.0.2.1:53", "example.")
	tcp.Client.Proto = pipeline.ProtoTCP
	if _, e := s.Serve(tcp, next); e != nil {
		t.Fatal(e)
	}
	if e := l.Reconfigure(nil); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Serve(udpReq("192.0.2.1:53", "example."), next); e != nil {
		t.Fatal(e)
	}
	if e := l.Reconfigure(&RateLimitConfig{IPv4: map[int]int{32: 1}}); e != nil {
		t.Fatal(e)
	}
	s.GroupExempt = func(*pipeline.Request) (bool, error) { return false, pipeline.ErrServingCapture }
	if out, e := s.Serve(udpReq("192.0.2.1:53", "example."), next); !errors.Is(e, pipeline.ErrServingCapture) || out != nil || l.Buckets() != 0 {
		t.Fatal("failed capture charged or exempted request", e)
	}
}
