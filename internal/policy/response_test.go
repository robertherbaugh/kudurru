package policy

import (
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/responsepolicy"
)

func TestResponsePolicyPriorityAndInvalidDiagnosis(t *testing.T) {
	hi := pol(ns, "high", v1alpha1.PolicySpec{Priority: 20, ClientGroupRefs: []string{"clients"}, Response: &v1alpha1.PolicyResponse{FilterAAAA: ptr(false)}, Rewrites: []v1alpha1.Rewrite{{Name: "*.example", Targets: []string{"192.0.2.20"}}}})
	lo := pol(ns, "low", v1alpha1.PolicySpec{Priority: 10, ClientGroupRefs: []string{"clients"}, Response: &v1alpha1.PolicyResponse{DNS64Prefix: ptr(""), FilterAAAA: ptr(true)}, Rewrites: []v1alpha1.Rewrite{{Name: "www.example", Targets: []string{"192.0.2.10"}}}})
	in := Input{ClientGroups: []*v1alpha1.ClientGroup{group(ns, "clients", v1alpha1.ClientMatcher{IP: "192.0.2.5"})}, Policies: []*v1alpha1.Policy{lo, hi}}
	ix := Build(in, Options{})
	d := ix.SelectPolicy(client("192.0.2.5"), time.Now())
	q := new(dns.Msg)
	q.SetQuestion("www.example.", dns.TypeA)
	req := &pipeline.Request{Msg: q}
	s, _ := responsepolicy.Compile(responsepolicy.Options{DNS64Prefix: "64:ff9b::/96", FilterAAAA: true})
	effective := s.With(d.Response)
	got := effective.RewriteAnswer(req, d.Rewrites)
	if got.Msg.Answer[0].(*dns.A).A.String() != "192.0.2.20" {
		t.Fatal("lower-priority exact rule defeated higher-priority wildcard")
	}
	q.SetQuestion("other.example.", dns.TypeAAAA)
	denial := pipeline.Reply(req, 0, pipeline.StatusForwarded)
	denial.Origin = pipeline.OriginForwarded
	if effective.WantsDNS64(req, denial) {
		t.Fatal("explicit empty prefix did not disable inherited DNS64")
	}
	// The compiler copied RDATA and overrides from objects.
	hi.Spec.Rewrites[0].Targets[0] = "192.0.2.99"
	q.SetQuestion("www.example.", dns.TypeA)
	if got := effective.RewriteAnswer(req, d.Rewrites); got.Msg.Answer[0].(*dns.A).A.String() != "192.0.2.20" {
		t.Fatal("published rule changed with source object")
	}
	hi.Spec.Rewrites[0].Targets[0] = "invalid address"
	ix = Build(in, Options{})
	d = ix.SelectPolicy(client("192.0.2.5"), time.Now())
	if !d.ResponseInvalid {
		t.Fatal("invalid rewrite would silently fall through")
	}
	if len(ix.Diagnoses()) == 0 || ix.Diagnoses()[0].Reason != "InvalidResponsePolicy" {
		t.Fatalf("missing condition: %v", ix.Diagnoses())
	}
}
