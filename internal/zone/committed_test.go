package zone

import (
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func committedRecords(t testing.TB, extra ...string) []dns.RR {
	t.Helper()
	var out []dns.RR
	texts := append([]string{"example.org. 123 IN SOA original.example.org. mailbox.example.org. 99 61 32 604 17", "example.org. 123 IN NS ns.example.org.", "ns.example.org. 123 IN A 192.0.2.53"}, extra...)
	for _, text := range texts {
		rr, err := dns.NewRR(text)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, rr)
	}
	return out
}
func TestCommittedPrimaryRawRecordsAndPolicy(t *testing.T) {
	config := dnsZone("example.org.")
	config.Spec.Policy = v1alpha1.ZonePolicy{QueryAccess: v1alpha1.AccessUseNetworkACL, QueryACL: []string{"192.0.2.0/24"}, ZoneTransfer: v1alpha1.AccessAllow, TransferTSIGKeyRefs: []string{"transfer-key."}, Notify: v1alpha1.NotifySpecified, NotifyTargets: []string{"192.0.2.8"}}
	records := committedRecords(t, "opaque.example.org. 70 IN TYPE65200 \\# 3 AbCdEF", "text.example.org. 200 IN TXT \"\\000MiXeD\\255\"", "pair.example.org. 300 IN A 192.0.2.1", "pair.example.org. 60 IN A 192.0.2.2", "alias.example.org. 77 IN CNAME opaque.example.org.", "*.wild.example.org. 90 IN A 192.0.2.99", "sub.example.org. 99 IN NS ns.sub.example.org.", "ns.sub.example.org. 88 IN A 192.0.2.10")
	before := make([]string, len(records))
	for i, rr := range records {
		before[i] = rr.String()
	}
	// TransferAllowed checks wall-clock availability; keep its policy fixture live.
	expiry := time.Now().Add(time.Hour)
	model, err := BuildCommittedPrimary(config, records, 99, expiry)
	if err != nil {
		t.Fatal(err)
	}
	if model.Type() != v1alpha1.ZoneTypePrimary || model.SecondaryIdentity() != "" || model.HasSignatures() {
		t.Fatal("secondary model substituted")
	}
	if model.SOA().Ns != "original.example.org." || model.SOA().Minttl != 17 || model.Serial() != 99 {
		t.Fatal("generated replacement SOA")
	}
	if !model.QueryAllowed(netip.MustParseAddr("192.0.2.8")) || model.QueryAllowed(netip.MustParseAddr("198.51.100.8")) || !model.TransferAllowed(netip.MustParseAddr("192.0.2.8"), "TRANSFER-KEY.") || model.TransferAllowed(netip.MustParseAddr("192.0.2.8"), "") {
		t.Fatal("frozen policy lost")
	}
	if len(model.NotifyTargets()) != 1 {
		t.Fatal("notify policy lost")
	}
	for _, rr := range model.Lookup("pair.example.org.", dns.TypeA).Answer {
		if rr.Header().Ttl != 60 {
			t.Fatal("RRset TTL not normalized")
		}
	}
	if got := model.Lookup("opaque.example.org.", 65200).Answer[0].(*dns.RFC3597).Rdata; got != "abcdef" {
		t.Fatalf("opaque octets %s", got)
	}
	if got := model.Lookup("text.example.org.", dns.TypeTXT).Answer[0].(*dns.TXT).Txt; !reflect.DeepEqual(got, records[4].(*dns.TXT).Txt) {
		t.Fatalf("TXT octets: %v", got)
	}
	if model.Lookup("x.wild.example.org.", dns.TypeA).Kind != Success || model.Lookup("host.sub.example.org.", dns.TypeA).Kind != Delegation || model.Lookup("none.example.org.", dns.TypeA).Kind != NXDomain {
		t.Fatal("primary tree behavior changed")
	}
	for i, rr := range records {
		if rr.String() != before[i] {
			t.Fatal("input modified")
		}
	}
	records[3].(*dns.RFC3597).Rdata = "00"
	if model.Lookup("opaque.example.org.", 65200).Answer[0].(*dns.RFC3597).Rdata != "abcdef" {
		t.Fatal("model aliases input")
	}
	if !model.CommittedAvailableAt(expiry.Add(-time.Nanosecond)) || model.CommittedAvailableAt(expiry) {
		t.Fatal("expiry boundary")
	}
	failure, err := BuildCommittedFailureCoverage(model)
	if err != nil {
		t.Fatal(err)
	}
	if failure.Type() != v1alpha1.ZoneTypePrimary || failure.SOA() != nil || failure.AvailableAt(expiry) || failure.TransferAllowed(netip.MustParseAddr("192.0.2.8"), "transfer-key.") || !failure.QueryAllowed(netip.MustParseAddr("192.0.2.8")) || failure.QueryAllowed(netip.MustParseAddr("198.51.100.8")) {
		t.Fatal("failure coverage lost primary authority or frozen ACL")
	}
	unavailable, err := BuildCommittedUnavailable(config)
	if err != nil {
		t.Fatal(err)
	}
	if unavailable.Type() != v1alpha1.ZoneTypePrimary || !unavailable.Contains("child.example.org.") || unavailable.CommittedAvailableAt(testNow) || unavailable.SOA() != nil {
		t.Fatal("unavailable coverage")
	}
}
func TestCommittedPrimaryRejectsMalformedCompleteData(t *testing.T) {
	for _, extra := range [][]string{
		{"outside.test. 60 IN A 192.0.2.1"},
		{"x.example.org. 60 IN CNAME y.example.org.", "x.example.org. 60 IN TXT \"data\""},
		{"dn.example.org. 60 IN DNAME other.test.", "child.dn.example.org. 60 IN A 192.0.2.1"},
		{"example.org. 60 IN SOA ns.example.org. hostmaster.example.org. 99 60 30 600 30"},
	} {
		if _, err := BuildCommittedPrimary(dnsZone("example.org."), committedRecords(t, extra...), 99, time.Time{}); err == nil {
			t.Fatalf("accepted %v", extra)
		}
	}
	if _, err := BuildCommittedPrimary(dnsZone("example.org."), committedRecords(t), 98, time.Time{}); err == nil {
		t.Fatal("mismatched serial")
	}
}
