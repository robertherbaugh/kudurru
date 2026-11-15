package responsepolicy

import (
	"testing"

	"github.com/miekg/dns"
)

func TestRewriteSpecificityFamiliesAndCNAME(t *testing.T) {
	rules, err := CompileRewrites([]RewriteSpec{
		{Name: "*.example", Targets: []string{"192.0.2.1", "2001:db8::1"}},
		{Name: "www.example", Targets: []string{"192.0.2.2"}},
		{Name: "alias.example", CNAME: "local.owned.example."},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := settings(t, Options{TTL: 45})
	q := query(dns.TypeA)
	r := s.RewriteAnswer(q, rules)
	if r.Msg.Answer[0].(*dns.A).A.String() != "192.0.2.2" || r.Msg.Answer[0].Header().Ttl != 45 {
		t.Fatal(r.Msg)
	}
	q.Msg.Question[0].Name = "example."
	if s.RewriteAnswer(q, rules) != nil {
		t.Fatal("wildcard rewrote its apex")
	}
	q.Msg.Question[0].Name = "alias.example."
	if r = s.RewriteAnswer(q, rules); r.Msg.Answer[0].(*dns.CNAME).Target != "local.owned.example." {
		t.Fatal(r.Msg)
	}
	q.Msg.Question[0].Name, q.Msg.Question[0].Qtype = "child.example.", dns.TypeAAAA
	if r = settings(t, Options{FilterAAAA: true}).RewriteAnswer(q, rules); len(r.Msg.Answer) != 0 {
		t.Fatal("configured AAAA escaped client filter")
	}
	for _, rule := range []RewriteSpec{
		{Name: "*.example", Type: "TXT", Targets: []string{"\"hello\""}},
		{Name: "*.example", CNAME: "example."},
		{Name: "example", Type: "MX", Targets: []string{"10 mail.example."}},
		{Name: "example", Type: "HTTPS", Targets: []string{"1 . alpn=h2"}},
	} {
		if _, err := CompileRewrites([]RewriteSpec{rule}); err != nil {
			t.Fatalf("valid rewrite: %v", err)
		}
	}
	for _, rule := range []RewriteSpec{
		{Name: "*"}, {Name: "a.*.example", Targets: []string{"192.0.2.1"}}, {Name: "www.example", CNAME: "www.example."},
		{Name: "www.example", Type: "SOA", Targets: []string{"bad"}}, {Name: "www.example", Targets: []string{"x"}},
		{Name: "www.example", Type: "TXT", Targets: []string{"\"x\"\nother.example. IN A 192.0.2.1"}},
	} {
		if _, err := CompileRewrites([]RewriteSpec{rule}); err == nil {
			t.Fatalf("invalid rewrite accepted: %+v", rule)
		}
	}
}
