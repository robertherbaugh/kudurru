package main

import (
	"testing"

	"github.com/miekg/dns"
)

// The UPDATE-owned set stores each RR as an endpoint target. Every supported
// type must round-trip through the zone builder's parser exactly.
func TestKubeUpdateEndpointsRoundTrip(t *testing.T) {
	var records []dns.RR
	for _, s := range []string{
		"a.dyn.test. 60 IN A 192.0.2.1",
		"a.dyn.test. 60 IN A 192.0.2.2",
		"a.dyn.test. 60 IN AAAA 2001:db8::1",
		"t.dyn.test. 60 IN TXT \"hello world\" \"second\"",
		"t2.dyn.test. 60 IN TXT \"quote \\\" inside\"",
		"m.dyn.test. 60 IN MX 10 mail.dyn.test.",
		"_sip._tcp.dyn.test. 60 IN SRV 10 20 5060 sip.dyn.test.",
		"c.dyn.test. 60 IN CNAME a.dyn.test.",
	} {
		rr, err := dns.NewRR(s)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, rr)
	}
	endpoints, ok := kubeUpdateEndpoints(records, map[[2]any]bool{})
	if !ok {
		t.Fatal("supported records did not convert")
	}
	if len(endpoints) != 7 {
		t.Fatal("RRsets were not grouped by name and type", len(endpoints))
	}
	for _, ep := range endpoints {
		if ep.DNSName == "a.dyn.test." && ep.RecordType == "A" && len(ep.Targets) != 2 {
			t.Fatal("A RRset lost a member")
		}
	}
	// Reserved RRsets and the SOA never enter the UPDATE-owned set.
	soa, _ := dns.NewRR("dyn.test. 60 IN SOA ns.dyn.test. h.dyn.test. 1 2 3 4 5")
	endpoints, ok = kubeUpdateEndpoints(append(records, soa), map[[2]any]bool{{"a.dyn.test.", dns.TypeA}: true})
	if !ok {
		t.Fatal(ok)
	}
	for _, ep := range endpoints {
		if ep.RecordType == "SOA" || ep.DNSName == "a.dyn.test." && ep.RecordType == "A" {
			t.Fatal("reserved or SOA data entered the UPDATE-owned set", ep)
		}
	}
}
