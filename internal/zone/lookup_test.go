package zone

import (
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

const soaExample = "example.org. 300 IN SOA ns1.example.org. hostmaster.example.org. 2026101100 3600 900 1209600 300"

// longLabel is a 63-octet label used to push DNAME results past 255 octets.
var longLabel = strings.Repeat("x", 63)

func exampleEndpoints() []v1alpha1.Endpoint {
	disabled := ep("disabled.example.org", "A", "192.0.2.200")
	disabled.Kudurru = &v1alpha1.EndpointExt{Disabled: true, Comment: "parked"}
	expired := ep("expired.example.org", "A", "192.0.2.201")
	expired.Kudurru = &v1alpha1.EndpointExt{ExpiresAt: &metav1.Time{Time: testNow.Add(-time.Minute)}}
	expiring := ep("expiring.example.org", "A", "192.0.2.202")
	expiring.Kudurru = &v1alpha1.EndpointExt{ExpiresAt: &metav1.Time{Time: testNow.Add(time.Hour)}}
	viewed := ep("viewed.example.org", "A", "192.0.2.203")
	viewed.Kudurru = &v1alpha1.EndpointExt{View: "kids"}

	return []v1alpha1.Endpoint{
		ep("ns1.example.org", "A", "192.0.2.1"),
		ep("ns2.example.org", "A", "192.0.2.2"),
		ep("ns2.example.org", "AAAA", "2001:db8::2"),
		ep("@", "A", "192.0.2.10"),
		ep("example.org", "MX", "10 mail.example.org"),
		ep("mail.example.org", "A", "192.0.2.25"),
		ep("www.example.org", "CNAME", "web.example.org"),
		epTTL("web.example.org", "A", 60, "192.0.2.81", "192.0.2.80"),
		ep("web.example.org", "AAAA", "2001:db8::80"),
		ep("alias.example.org", "CNAME", "www.example.org."),
		ep("out.example.org", "CNAME", "target.example.net"),
		ep("chain-out.example.org", "CNAME", "out.example.org"),
		ep("loop1.example.org", "CNAME", "loop2.example.org"),
		ep("loop2.example.org", "CNAME", "loop1.example.org"),
		ep("*.wild.example.org", "A", "192.0.2.99"),
		ep("*.wild.example.org", "TXT", "wildcard"),
		ep("exists.wild.example.org", "A", "192.0.2.98"),
		ep("*.cn.example.org", "CNAME", "web.example.org"),
		ep("a.b.c.ent.example.org", "A", "192.0.2.5"),
		ep("sub.example.org", "NS", "ns.sub.example.org", "ns.elsewhere.example.net"),
		ep("ns.sub.example.org", "A", "192.0.2.53"),
		ep("dn.example.org", "DNAME", "example.net"),
		ep("dn2.example.org", "DNAME", "inside.example.org"),
		ep("host.inside.example.org", "A", "192.0.2.7"),
		ep("long.example.org", "DNAME", longLabel+"."+longLabel+"."+longLabel+".example.net"),
		ep("caa.example.org", "CAA", `0 issue "letsencrypt.org"`),
		ep("svc.example.org", "HTTPS", "1 . alpn=h2,h3"),
		ep("svcb.example.org", "SVCB", "1 web.example.org port=8443"),
		ep("_sip._tcp.example.org", "SRV", "10 5 5060 sip.example.org"),
		ep("sip.example.org", "A", "192.0.2.60"),
		ep("txt.example.org", "TXT", "v=spf1 -all", `"two" "strings"`),
		ep("unknown.example.org", "TYPE65280", `\# 2 abcd`),
		ep("_443._tcp.web.example.org", "TLSA", "3 1 1 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
		ep("ssh.example.org", "SSHFP", "1 1 dd465c09cfa51fb45020cc83316fff21b9ec74ac"),
		ep("naptr.example.org", "NAPTR", `100 10 "U" "E2U+sip" "!^.*$!sip:info@example.org!" .`),
		ep("10.2.0.192.in-addr.example.org", "PTR", "web.example.org"),
		ep("UPPER.Example.ORG", "A", "192.0.2.77"),
		disabled, expired, expiring, viewed,
	}
}

func exampleZone(t testing.TB) *Zone {
	return mustBuild(t, dnsZone("example.org."), exampleEndpoints()...)
}

func TestLookupGolden(t *testing.T) {
	z := exampleZone(t)
	nsApex := []string{"example.org. 300 IN NS ns1.example.org.", "example.org. 300 IN NS ns2.example.org."}
	glueApex := []string{"ns1.example.org. 300 IN A 192.0.2.1", "ns2.example.org. 300 IN A 192.0.2.2", "ns2.example.org. 300 IN AAAA 2001:db8::2"}
	runCases(t, z, []Case{
		// Success at the apex and below.
		{Qname: "example.org.", Qtype: dns.TypeSOA, Kind: Success, Answer: []string{soaExample}},
		{Qname: "example.org.", Qtype: dns.TypeNS, Kind: Success, Answer: nsApex, Extra: glueApex},
		{Qname: "example.org.", Qtype: dns.TypeA, Kind: Success, Answer: []string{"example.org. 300 IN A 192.0.2.10"}},
		{Qname: "example.org.", Qtype: dns.TypeMX, Kind: Success,
			Answer: []string{"example.org. 300 IN MX 10 mail.example.org."},
			Extra:  []string{"mail.example.org. 300 IN A 192.0.2.25"}},
		{Qname: "web.example.org.", Qtype: dns.TypeA, Kind: Success,
			Answer: []string{"web.example.org. 60 IN A 192.0.2.80", "web.example.org. 60 IN A 192.0.2.81"}},
		{Name: "case-insensitive", Qname: "WeB.ExAmPlE.oRg.", Qtype: dns.TypeAAAA, Kind: Success,
			Answer: []string{"web.example.org. 300 IN AAAA 2001:db8::80"}},
		{Name: "stored-lowercase", Qname: "upper.example.org.", Qtype: dns.TypeA, Kind: Success,
			Answer: []string{"upper.example.org. 300 IN A 192.0.2.77"}},
		{Qname: "_sip._tcp.example.org.", Qtype: dns.TypeSRV, Kind: Success,
			Answer: []string{"_sip._tcp.example.org. 300 IN SRV 10 5 5060 sip.example.org."},
			Extra:  []string{"sip.example.org. 300 IN A 192.0.2.60"}},

		// Passthrough types.
		{Qname: "caa.example.org.", Qtype: dns.TypeCAA, Kind: Success, Answer: []string{`caa.example.org. 300 IN CAA 0 issue "letsencrypt.org"`}},
		{Qname: "svc.example.org.", Qtype: dns.TypeHTTPS, Kind: Success, Answer: []string{`svc.example.org. 300 IN HTTPS 1 . alpn="h2,h3"`}},
		{Qname: "svcb.example.org.", Qtype: dns.TypeSVCB, Kind: Success, Answer: []string{"svcb.example.org. 300 IN SVCB 1 web.example.org. port=8443"}},
		{Qname: "txt.example.org.", Qtype: dns.TypeTXT, Kind: Success,
			Answer: []string{`txt.example.org. 300 IN TXT "v=spf1 -all"`, `txt.example.org. 300 IN TXT "two" "strings"`}},
		{Qname: "unknown.example.org.", Qtype: 65280, Kind: Success, Answer: []string{`unknown.example.org. 300 IN TYPE65280 \# 2 abcd`}},
		{Qname: "_443._tcp.web.example.org.", Qtype: dns.TypeTLSA, Kind: Success,
			Answer: []string{"_443._tcp.web.example.org. 300 IN TLSA 3 1 1 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}},
		{Qname: "ssh.example.org.", Qtype: dns.TypeSSHFP, Kind: Success, Answer: []string{"ssh.example.org. 300 IN SSHFP 1 1 dd465c09cfa51fb45020cc83316fff21b9ec74ac"}},
		{Qname: "naptr.example.org.", Qtype: dns.TypeNAPTR, Kind: Success, Answer: []string{`naptr.example.org. 300 IN NAPTR 100 10 "U" "E2U+sip" "!^.*$!sip:info@example.org!" .`}},
		{Qname: "10.2.0.192.in-addr.example.org.", Qtype: dns.TypePTR, Kind: Success, Answer: []string{"10.2.0.192.in-addr.example.org. 300 IN PTR web.example.org."}},

		// NODATA and NXDOMAIN carry the SOA.
		{Qname: "web.example.org.", Qtype: dns.TypeTXT, Kind: NoData, Ns: []string{soaExample}},
		{Qname: "example.org.", Qtype: dns.TypeAAAA, Kind: NoData, Ns: []string{soaExample}},
		{Qname: "nope.example.org.", Qtype: dns.TypeA, Kind: NXDomain, Ns: []string{soaExample}},
		{Qname: "deep.nope.example.org.", Qtype: dns.TypeA, Kind: NXDomain, Ns: []string{soaExample}},
		{Name: "empty-non-terminal", Qname: "c.ent.example.org.", Qtype: dns.TypeA, Kind: NoData, Ns: []string{soaExample}},
		{Name: "below-ent", Qname: "x.c.ent.example.org.", Qtype: dns.TypeA, Kind: NXDomain, Ns: []string{soaExample}},
		{Name: "disabled", Qname: "disabled.example.org.", Qtype: dns.TypeA, Kind: NXDomain, Ns: []string{soaExample}},
		{Name: "expired", Qname: "expired.example.org.", Qtype: dns.TypeA, Kind: NXDomain, Ns: []string{soaExample}},
		{Name: "not-yet-expired", Qname: "expiring.example.org.", Qtype: dns.TypeA, Kind: Success, Answer: []string{"expiring.example.org. 300 IN A 192.0.2.202"}},
		{Name: "view-withheld", Qname: "viewed.example.org.", Qtype: dns.TypeA, Kind: NXDomain, Ns: []string{soaExample}},

		// Wildcards (RFC 4592).
		{Qname: "host.wild.example.org.", Qtype: dns.TypeA, Kind: Success, Wildcard: true,
			Answer: []string{"host.wild.example.org. 300 IN A 192.0.2.99"}},
		{Name: "wildcard-multi-label", Qname: "a.b.wild.example.org.", Qtype: dns.TypeTXT, Kind: Success, Wildcard: true,
			Answer: []string{`a.b.wild.example.org. 300 IN TXT "wildcard"`}},
		{Name: "wildcard-nodata", Qname: "host.wild.example.org.", Qtype: dns.TypeMX, Kind: NoData, Wildcard: true, Ns: []string{soaExample}},
		{Name: "wildcard-blocked-by-existing-name", Qname: "exists.wild.example.org.", Qtype: dns.TypeTXT, Kind: NoData, Ns: []string{soaExample}},
		{Name: "wildcard-not-below-closer-encloser", Qname: "x.exists.wild.example.org.", Qtype: dns.TypeA, Kind: NXDomain, Ns: []string{soaExample}},
		{Name: "wildcard-literal", Qname: "*.wild.example.org.", Qtype: dns.TypeA, Kind: Success, Answer: []string{"*.wild.example.org. 300 IN A 192.0.2.99"}},
		{Name: "wildcard-cname", Qname: "anything.cn.example.org.", Qtype: dns.TypeA, Kind: Success, Wildcard: true,
			Answer: []string{"anything.cn.example.org. 300 IN CNAME web.example.org.", "web.example.org. 60 IN A 192.0.2.80", "web.example.org. 60 IN A 192.0.2.81"}},

		// CNAME chains.
		{Qname: "www.example.org.", Qtype: dns.TypeA, Kind: Success,
			Answer: []string{"www.example.org. 300 IN CNAME web.example.org.", "web.example.org. 60 IN A 192.0.2.80", "web.example.org. 60 IN A 192.0.2.81"}},
		{Name: "two-hop", Qname: "alias.example.org.", Qtype: dns.TypeAAAA, Kind: Success,
			Answer: []string{"alias.example.org. 300 IN CNAME www.example.org.", "www.example.org. 300 IN CNAME web.example.org.", "web.example.org. 300 IN AAAA 2001:db8::80"}},
		{Name: "cname-query", Qname: "www.example.org.", Qtype: dns.TypeCNAME, Kind: Success, Answer: []string{"www.example.org. 300 IN CNAME web.example.org."}},
		{Name: "chain-nodata", Qname: "www.example.org.", Qtype: dns.TypeMX, Kind: NoData,
			Answer: []string{"www.example.org. 300 IN CNAME web.example.org."}, Ns: []string{soaExample}},
		{Name: "leaves-zone", Qname: "out.example.org.", Qtype: dns.TypeA, Kind: CNAME, Target: "target.example.net.",
			Answer: []string{"out.example.org. 300 IN CNAME target.example.net."}},
		{Name: "leaves-zone-after-hop", Qname: "chain-out.example.org.", Qtype: dns.TypeA, Kind: CNAME, Target: "target.example.net.",
			Answer: []string{"chain-out.example.org. 300 IN CNAME out.example.org.", "out.example.org. 300 IN CNAME target.example.net."}},
		{Name: "loop", Qname: "loop1.example.org.", Qtype: dns.TypeA, Kind: Loop,
			Answer: loopAnswer()},

		// Delegations.
		{Qname: "sub.example.org.", Qtype: dns.TypeA, Kind: Delegation,
			Ns:    []string{"sub.example.org. 300 IN NS ns.sub.example.org.", "sub.example.org. 300 IN NS ns.elsewhere.example.net."},
			Extra: []string{"ns.sub.example.org. 300 IN A 192.0.2.53"}},
		{Name: "below-cut", Qname: "www.sub.example.org.", Qtype: dns.TypeA, Kind: Delegation,
			Ns:    []string{"sub.example.org. 300 IN NS ns.sub.example.org.", "sub.example.org. 300 IN NS ns.elsewhere.example.net."},
			Extra: []string{"ns.sub.example.org. 300 IN A 192.0.2.53"}},
		{Name: "glue-not-authoritative", Qname: "ns.sub.example.org.", Qtype: dns.TypeA, Kind: Delegation,
			Ns:    []string{"sub.example.org. 300 IN NS ns.sub.example.org.", "sub.example.org. 300 IN NS ns.elsewhere.example.net."},
			Extra: []string{"ns.sub.example.org. 300 IN A 192.0.2.53"}},
		{Name: "ds-at-cut-from-parent", Qname: "sub.example.org.", Qtype: dns.TypeDS, Kind: NoData, Ns: []string{soaExample}},

		// DNAME (RFC 6672).
		{Name: "dname-out", Qname: "x.dn.example.org.", Qtype: dns.TypeA, Kind: CNAME, Target: "x.example.net.",
			Answer: []string{"dn.example.org. 300 IN DNAME example.net.", "x.dn.example.org. 300 IN CNAME x.example.net."}},
		{Name: "dname-in", Qname: "host.dn2.example.org.", Qtype: dns.TypeA, Kind: Success,
			Answer: []string{"dn2.example.org. 300 IN DNAME inside.example.org.", "host.dn2.example.org. 300 IN CNAME host.inside.example.org.", "host.inside.example.org. 300 IN A 192.0.2.7"}},
		{Name: "dname-in-nxdomain", Qname: "nohost.dn2.example.org.", Qtype: dns.TypeA, Kind: NXDomain,
			Answer: []string{"dn2.example.org. 300 IN DNAME inside.example.org.", "nohost.dn2.example.org. 300 IN CNAME nohost.inside.example.org."},
			Ns:     []string{soaExample}},
		{Name: "dname-owner-itself", Qname: "dn.example.org.", Qtype: dns.TypeDNAME, Kind: Success, Answer: []string{"dn.example.org. 300 IN DNAME example.net."}},
		{Name: "dname-owner-other-type", Qname: "dn.example.org.", Qtype: dns.TypeA, Kind: NoData, Ns: []string{soaExample}},
		{Name: "dname-yxdomain", Qname: longLabel + "." + longLabel + ".long.example.org.", Qtype: dns.TypeA, Kind: YXDomain,
			Answer: []string{"long.example.org. 300 IN DNAME " + longLabel + "." + longLabel + "." + longLabel + ".example.net."}},
	})
}

// loopAnswer is the chain loop1 -> loop2 -> loop1 ... cut at MaxChain hops.
func loopAnswer() []string {
	var out []string
	for i := 0; i <= MaxChain; i++ {
		if i%2 == 0 {
			out = append(out, "loop1.example.org. 300 IN CNAME loop2.example.org.")
		} else {
			out = append(out, "loop2.example.org. 300 IN CNAME loop1.example.org.")
		}
	}
	return out
}

func TestLookupRcodeAndAA(t *testing.T) {
	z := exampleZone(t)
	for _, tc := range []struct {
		qname string
		qtype uint16
		rcode int
		aa    bool
	}{
		{"web.example.org.", dns.TypeA, dns.RcodeSuccess, true},
		{"nope.example.org.", dns.TypeA, dns.RcodeNameError, true},
		{"web.example.org.", dns.TypeMX, dns.RcodeSuccess, true},
		{"www.sub.example.org.", dns.TypeA, dns.RcodeSuccess, false},
		{"loop1.example.org.", dns.TypeA, dns.RcodeServerFailure, false},
		{longLabel + "." + longLabel + ".long.example.org.", dns.TypeA, dns.RcodeYXDomain, true},
	} {
		res := z.Lookup(tc.qname, tc.qtype)
		if res.Rcode() != tc.rcode || res.Authoritative() != tc.aa {
			t.Errorf("%s/%d: rcode %d aa %v, want %d %v", tc.qname, tc.qtype, res.Rcode(), res.Authoritative(), tc.rcode, tc.aa)
		}
	}
}

func TestLookupFromSharesHopBudget(t *testing.T) {
	z := exampleZone(t)
	res := z.LookupFrom("www.example.org.", dns.TypeA, MaxChain)
	if res.Kind != Loop {
		t.Fatalf("a chain arriving with the budget spent must stop: %v", res.Kind)
	}
	res = z.LookupFrom("www.example.org.", dns.TypeA, 3)
	if res.Kind != Success || res.Hops != 4 {
		t.Fatalf("got %v hops %d, want success after 4 hops", res.Kind, res.Hops)
	}
}

func TestLookupChainIntoDelegationLeaves(t *testing.T) {
	z := mustBuild(t, dnsZone("example.org."),
		ep("sub.example.org", "NS", "ns.example.net"),
		ep("go.example.org", "CNAME", "www.sub.example.org"),
	)
	res := z.Lookup("go.example.org.", dns.TypeA)
	if res.Kind != CNAME || res.Target != "www.sub.example.org." || len(res.Ns) != 0 {
		t.Fatalf("chain into a cut must leave the zone: %+v", res)
	}
}

func TestLookupApexDNAME(t *testing.T) {
	z := mustBuild(t, dnsZone("old.example."), ep("@", "DNAME", "new.example"))
	runCases(t, z, []Case{
		{Qname: "www.old.example.", Qtype: dns.TypeA, Kind: CNAME, Target: "www.new.example.",
			Answer: []string{"old.example. 300 IN DNAME new.example.", "www.old.example. 300 IN CNAME www.new.example."}},
		{Qname: "old.example.", Qtype: dns.TypeSOA, Kind: Success,
			Answer: []string{"old.example. 300 IN SOA ns1.old.example. hostmaster.old.example. 2026101100 3600 900 1209600 300"}},
	})
}

func TestLookupRootZone(t *testing.T) {
	dz := dnsZone(".", func(dz *v1alpha1.DNSZone) {
		dz.Spec.Nameservers = []string{"a.root.test"}
		dz.Spec.SOA = nil
	})
	z := mustBuild(t, dz, ep("test", "NS", "a.root.test"), ep("a.root.test", "A", "192.0.2.1"))
	runCases(t, z, []Case{
		{Qname: ".", Qtype: dns.TypeSOA, Kind: Success, Answer: []string{". 300 IN SOA a.root.test. hostmaster. 2026101100 3600 900 1209600 300"}},
		{Qname: "www.test.", Qtype: dns.TypeA, Kind: Delegation, Ns: []string{"test. 300 IN NS a.root.test."}, Extra: []string{"a.root.test. 300 IN A 192.0.2.1"}},
		{Qname: "nope.", Qtype: dns.TypeA, Kind: NXDomain, Ns: []string{". 300 IN SOA a.root.test. hostmaster. 2026101100 3600 900 1209600 300"}},
	})
}

func TestNegativeSOATTL(t *testing.T) {
	dz := dnsZone("example.org.", func(dz *v1alpha1.DNSZone) {
		dz.Spec.DefaultTTL = 3600
		dz.Spec.SOA.Minimum = 60
	})
	z := mustBuild(t, dz)
	res := z.Lookup("nope.example.org.", dns.TypeA)
	if got := res.Ns[0].Header().Ttl; got != 60 {
		t.Fatalf("negative SOA TTL = %d, want min(TTL, minimum) = 60 (RFC 2308 §3)", got)
	}
	if got := z.Lookup("example.org.", dns.TypeSOA).Answer[0].Header().Ttl; got != 3600 {
		t.Fatalf("positive SOA TTL = %d, want 3600", got)
	}
}

func TestLookupDoesNotMutateZone(t *testing.T) {
	z := exampleZone(t)
	before := z.ContentHash()
	for range 3 {
		z.Lookup("host.wild.example.org.", dns.TypeA)
		z.Lookup("anything.cn.example.org.", dns.TypeA)
		z.Lookup("host.dn2.example.org.", dns.TypeA)
	}
	again := mustBuild(t, dnsZone("example.org."), exampleEndpoints()...)
	if before != again.ContentHash() || contentHash(z) != before {
		t.Fatal("lookups changed zone content")
	}
	if got := z.Lookup("*.wild.example.org.", dns.TypeA).Answer[0].Header().Name; got != "*.wild.example.org." {
		t.Fatalf("wildcard synthesis rewrote the stored owner: %s", got)
	}
}
