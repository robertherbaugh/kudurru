package zone

import (
	"bytes"
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func secondaryConfig() *v1alpha1.DNSZone {
	return dnsZone("example.org.", func(dz *v1alpha1.DNSZone) {
		dz.UID = types.UID("zone-instance-1")
		dz.Spec.Type = v1alpha1.ZoneTypeSecondary
		dz.Spec.Secondary = &v1alpha1.SecondarySpec{Primaries: []string{"192.0.2.53:53"}, TSIGKeyRef: "primary-key", TransferProtocol: "tcp"}
	})
}

func secondaryData(t testing.TB) []dns.RR {
	t.Helper()
	return rrs(t,
		"Example.ORG. 7200 IN SOA NS.Example.ORG. Hostmaster.Example.ORG. 4294967295 1234 45 6789 90",
		"Example.ORG. 600 IN NS NS.Example.ORG.",
		"NS.Example.ORG. 150 IN A 192.0.2.53",
	)
}

func mustSecondary(t testing.TB, dz *v1alpha1.DNSZone, data []dns.RR, expires time.Time) *Zone {
	t.Helper()
	z, err := BuildSecondary(dz, data, expires)
	if err != nil {
		t.Fatal(err)
	}
	return z
}

// Pack copies because dns.PackRR writes Rdlength to its argument.
func secondaryWire(t testing.TB, r dns.RR) []byte {
	t.Helper()
	c := dns.Copy(r)
	b := make([]byte, dns.Len(c)+16)
	n, err := dns.PackRR(c, b, 0, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	return b[:n]
}

func secondaryWires(t testing.TB, records []dns.RR) []string {
	t.Helper()
	out := make([]string, len(records))
	for i, r := range records {
		out[i] = string(secondaryWire(t, r))
	}
	slices.Sort(out)
	return out
}

func secondaryRecords(z *Zone) []dns.RR {
	var out []dns.RR
	z.RRsets(func(s *RRset) bool {
		out = append(out, s.RRs...)
		return true
	})
	return out
}

func TestSecondaryRawPreservation(t *testing.T) {
	dz := secondaryConfig()
	dz.Spec.DefaultTTL = 1
	dz.Spec.SOA = &v1alpha1.SOASpec{MName: "replace.invalid.", Refresh: 1}
	dz.Spec.Nameservers = []string{"replace.invalid."}
	dz.Status.Serial = 42
	data := append(secondaryData(t), rrs(t,
		"web.Example.ORG. 120 IN A 192.0.2.80",
		"web.Example.ORG. 30 IN A 192.0.2.81", // Preserve even unequal RRset TTLs.
		"opaque.Example.ORG. 43 IN TYPE65280 \\# 4 00ff007f",
		"Example.ORG. 300 IN DNSKEY 257 3 13 AQIDBA==",
		"Example.ORG. 300 IN RRSIG SOA 13 2 7200 20271014000000 20261014000000 12345 Example.ORG. AQIDBA==",
		"alias.Example.ORG. 88 IN CNAME web.Example.ORG.",
		"alias.Example.ORG. 88 IN RRSIG CNAME 13 3 88 20271014000000 20261014000000 12345 Example.ORG. AQIDBA==",
		"alias.Example.ORG. 88 IN NSEC web.Example.ORG. CNAME RRSIG NSEC",
		"hash.Example.ORG. 300 IN NSEC3 1 0 2 AB 00000000000000000000000000000000 A RRSIG",
		"Example.ORG. 300 IN NSEC3PARAM 1 0 2 AB",
		"child.Example.ORG. 601 IN DS 12345 13 2 00112233445566778899AABBCCDDEEFF00112233445566778899AABBCCDDEEFF",
		"Example.ORG. 300 IN CDS 12345 13 2 00112233445566778899AABBCCDDEEFF00112233445566778899AABBCCDDEEFF",
		"Example.ORG. 300 IN CDNSKEY 257 3 13 AQIDBA==",
	)...)
	before := secondaryWires(t, data)
	headers := make([]dns.RR_Header, len(data))
	for i, r := range data {
		headers[i] = *r.Header()
	}
	expires := testNow.Add(time.Hour)
	z := mustSecondary(t, dz, data, expires)
	if got := secondaryWires(t, secondaryRecords(z)); !slices.Equal(got, before) {
		t.Fatal("raw wire records changed during build")
	}
	for i, r := range data {
		if *r.Header() != headers[i] {
			t.Fatalf("build mutated input header %d", i)
		}
	}
	if z.Serial() != 4294967295 || !bytes.Equal(secondaryWire(t, z.SOA()), secondaryWire(t, data[0])) {
		t.Fatal("external SOA was restamped or normalized")
	}
	if z.RecordCount() != len(data)-1 || z.Type() != v1alpha1.ZoneTypeSecondary || z.NextExpiry() != expires || !z.HasSignatures() {
		t.Fatal("incorrect secondary metadata")
	}
	if r := z.Lookup("web.example.org.", dns.TypeA); len(r.Answer) != 2 || r.Answer[0].Header().Ttl != 120 || r.Answer[1].Header().Ttl != 30 {
		t.Fatalf("raw TTLs lost in answer: %+v", r)
	}
	if r := z.Lookup("opaque.example.org.", 65280); r.Kind != Success || !bytes.Equal(secondaryWire(t, r.Answer[0]), secondaryWire(t, data[5])) {
		t.Fatalf("unknown RR lookup: %+v", r)
	}
	// Caller changes cannot reach copied data, including IP and DNSSEC slices.
	data[0].(*dns.SOA).Serial = 1
	data[2].(*dns.A).A[0] = 203
	for _, r := range data {
		if n, ok := r.(*dns.NSEC); ok {
			n.TypeBitMap[0] = dns.TypeTXT
		}
		r.Header().Ttl = 1
	}
	dz.Spec.Policy.QueryAccess = v1alpha1.AccessDeny
	if !slices.Equal(secondaryWires(t, secondaryRecords(z)), before) || !z.QueryAllowed(netip.MustParseAddr("192.0.2.1")) {
		t.Fatal("zone shares mutable input data")
	}
	copySOA := z.SOA()
	copySOA.Serial = 9
	if z.Serial() != 4294967295 || z.SOA().Serial != 4294967295 {
		t.Fatal("SOA accessor exposes mutable zone state")
	}
	// Explicit DNSSEC queries can retrieve raw records; ordinary queries do
	// not synthesize DNSSEC response proofs or add signatures automatically.
	if r := z.Lookup("alias.example.org.", dns.TypeRRSIG); r.Kind != Success || len(r.Answer) != 1 {
		t.Fatal("raw signatures are not queryable")
	}
	if r := z.Lookup("alias.example.org.", dns.TypeA); len(r.Answer) != 3 || r.Answer[0].Header().Rrtype != dns.TypeCNAME {
		t.Fatal("signed CNAME does not use existing lookup behavior")
	}
}

func TestSecondaryInvalidData(t *testing.T) {
	tests := []struct {
		name string
		edit func([]dns.RR) []dns.RR
		want string
	}{
		{"empty", func([]dns.RR) []dns.RR { return []dns.RR{} }, "exactly one apex SOA"},
		{"missing-soa", func(r []dns.RR) []dns.RR { return r[1:] }, "exactly one apex SOA"},
		{"closing-soa", func(r []dns.RR) []dns.RR { return append(r, dns.Copy(r[0])) }, "exactly one apex SOA"},
		{"soa-below-apex", func(r []dns.RR) []dns.RR { r[0].Header().Name = "child.example.org."; return r }, "exactly one apex SOA"},
		{"missing-ns", func(r []dns.RR) []dns.RR { return []dns.RR{r[0], r[2]} }, "apex NS"},
		{"out-of-zone", func(r []dns.RR) []dns.RR { r[2].Header().Name = "example.net."; return r }, "in-zone"},
		{"suffix-trap", func(r []dns.RR) []dns.RR { r[2].Header().Name = "badexample.org."; return r }, "in-zone"},
		{"escaped-suffix-trap", func(r []dns.RR) []dns.RR { r[2].Header().Name = `bad\.example.org.`; return r }, "in-zone"},
		{"relative-owner", func(r []dns.RR) []dns.RR { r[2].Header().Name = "ns.example.org"; return r }, "wire"},
		{"bad-owner", func(r []dns.RR) []dns.RR { r[2].Header().Name = "a..example.org."; return r }, "wire"},
		{"wrong-class", func(r []dns.RR) []dns.RR { r[2].Header().Class = dns.ClassCHAOS; return r }, "class must be IN"},
		{"update-class", func(r []dns.RR) []dns.RR { r[2].Header().Class = dns.ClassANY; return r }, "class must be IN"},
		{"nil", func(r []dns.RR) []dns.RR { return append(r, nil) }, "nil resource"},
		{"typed-nil", func(r []dns.RR) []dns.RR { return append(r, (*dns.A)(nil)) }, "nil resource"},
		{"bad-ip", func(r []dns.RR) []dns.RR { r[2].(*dns.A).A = []byte{1, 2, 3}; return r }, "wire"},
		{"empty-ip", func(r []dns.RR) []dns.RR { r[2].(*dns.A).A = nil; return r }, "RDATA"},
		{"wrong-struct", func(r []dns.RR) []dns.RR { r[2].Header().Rrtype = dns.TypeCNAME; return r }, "does not match"},
		{"wrong-unknown-struct", func(r []dns.RR) []dns.RR { r[2].Header().Rrtype = 65280; return r }, "does not match"},
		{"empty-ns", func(r []dns.RR) []dns.RR { r[1].(*dns.NS).Ns = ""; return r }, "RDATA"},
		{"nil-svcb-value", func(r []dns.RR) []dns.RR {
			return append(r, &dns.SVCB{Hdr: dns.RR_Header{Name: "x.example.org.", Rrtype: dns.TypeSVCB, Class: dns.ClassINET}, Target: ".", Value: []dns.SVCBKeyValue{nil}})
		}, "malformed resource record"},
		{"raw-known-truncated", func(r []dns.RR) []dns.RR {
			return append(r, &dns.RFC3597{Hdr: dns.RR_Header{Name: "bad.example.org.", Rrtype: dns.TypeA, Class: dns.ClassINET}, Rdata: "0102"})
		}, "wire structure"},
		{"raw-bad-hex", func(r []dns.RR) []dns.RR {
			return append(r, &dns.RFC3597{Hdr: dns.RR_Header{Name: "bad.example.org.", Rrtype: 65280, Class: dns.ClassINET}, Rdata: "x"})
		}, "wire"},
		{"relative-target", func(r []dns.RR) []dns.RR { r[1].(*dns.NS).Ns = "ns.example.org"; return r }, "wire"},
		{"cname-with-a", func(r []dns.RR) []dns.RR { return append(r, rr(t, "ns.example.org. 1 IN CNAME target.example.org.")) }, "cannot coexist"},
		{"cname-with-unknown", func(r []dns.RR) []dns.RR {
			return append(r, rrs(t, "alias.example.org. 1 IN CNAME target.example.org.", "alias.example.org. 1 IN TYPE65280 \\# 1 ff")...)
		}, "cannot coexist"},
		{"apex-cname", func(r []dns.RR) []dns.RR { return append(r, rr(t, "example.org. 1 IN CNAME target.example.org.")) }, "cannot coexist"},
		{"two-cnames", func(r []dns.RR) []dns.RR {
			return append(r, rrs(t, "alias.example.org. 1 IN CNAME target.example.org.", "alias.example.org. 1 IN CNAME other.example.org.")...)
		}, "exactly one CNAME"},
		{"two-dnames", func(r []dns.RR) []dns.RR {
			return append(r, rrs(t, "dn.example.org. 1 IN DNAME example.net.", "dn.example.org. 1 IN DNAME example.com.")...)
		}, "exactly one DNAME"},
		{"sig-zero", func(r []dns.RR) []dns.RR {
			return append(r, rr(t, "x.example.org. 1 IN SIG TYPE0 13 3 1 20271014000000 20261014000000 12345 example.org. AQIDBA=="))
		}, "SIG covers"},
		{"rrsig-zero", func(r []dns.RR) []dns.RR {
			return append(r, rr(t, "x.example.org. 1 IN RRSIG TYPE0 13 3 1 20271014000000 20261014000000 12345 example.org. AQIDBA=="))
		}, "RRSIG covers"},
		{"oversized", func(r []dns.RR) []dns.RR {
			return append(r, &dns.RFC3597{Hdr: dns.RR_Header{Name: "x.example.org.", Rrtype: 65280, Class: dns.ClassINET}, Rdata: strings.Repeat("ff", 65536)})
		}, "wire"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			z, err := BuildSecondary(secondaryConfig(), tc.edit(secondaryData(t)), testNow.Add(time.Hour))
			if err == nil || z != nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("zone=%v error=%v; want %q", z, err, tc.want)
			}
		})
	}
	for _, typ := range []uint16{dns.TypeNone, dns.TypeReserved, dns.TypeOPT, dns.TypeTSIG, dns.TypeTKEY, dns.TypeAXFR, dns.TypeIXFR, dns.TypeMAILA, dns.TypeMAILB, dns.TypeANY} {
		t.Run(fmt.Sprintf("pseudo-%d", typ), func(t *testing.T) {
			r := &dns.RFC3597{Hdr: dns.RR_Header{Name: "x.example.org.", Rrtype: typ, Class: dns.ClassINET}}
			if z, err := BuildSecondary(secondaryConfig(), append(secondaryData(t), r), testNow.Add(time.Hour)); err == nil || z != nil {
				t.Fatalf("accepted pseudo type %d", typ)
			}
		})
	}
}

func TestSecondaryPlaceholderAndExpiry(t *testing.T) {
	dz := secondaryConfig()
	dz.Spec.Policy = v1alpha1.ZonePolicy{QueryAccess: v1alpha1.AccessUseNetworkACL, QueryACL: []string{"!192.0.2.1", "192.0.2.0/24"}, ZoneTransfer: v1alpha1.AccessAllow}
	z := mustSecondary(t, dz, nil, testNow.Add(time.Hour))
	if z.AvailableAt(testNow) || z.SOA() != nil || z.Serial() != 0 || z.RecordCount() != 0 || z.HasSignatures() || !z.NextExpiry().IsZero() {
		t.Fatal("placeholder contains serving data")
	}
	if !z.Contains("www.EXAMPLE.org.") || z.Contains("example.net.") || z.Names() != 1 || z.Type() != v1alpha1.ZoneTypeSecondary {
		t.Fatal("placeholder lost authority coverage")
	}
	if !z.QueryAllowed(netip.MustParseAddr("192.0.2.2")) || z.QueryAllowed(netip.MustParseAddr("192.0.2.1")) || z.QueryAllowed(netip.MustParseAddr("203.0.113.1")) {
		t.Fatal("placeholder lost query ACL")
	}
	if z.TransferAllowed(netip.MustParseAddr("192.0.2.2"), "") || len(slices.Collect(z.AXFR(0))) != 0 || len(secondaryRecords(z)) != 0 {
		t.Fatal("placeholder permits transfers")
	}
	// Serving code checks availability before lookup and retains this zone
	// in its suffix index. It must not use empty-tree lookup for a negative.
	for _, qname := range []string{"example.org.", "www.example.org."} {
		if !z.Contains(qname) || z.AvailableAt(testNow) {
			t.Fatalf("placeholder cannot be gated for %s", qname)
		}
	}
	expires := testNow.Add(time.Hour)
	z = mustSecondary(t, dz, secondaryData(t), expires)
	for _, tc := range []struct {
		now  time.Time
		want bool
	}{
		{testNow, true}, {expires.Add(-time.Nanosecond), true}, {expires, false}, {expires.Add(time.Hour), false},
	} {
		if got := z.AvailableAt(tc.now); got != tc.want {
			t.Fatalf("AvailableAt(%v)=%v, want %v", tc.now, got, tc.want)
		}
	}
	if z.Serial() != 4294967295 || !z.Contains("ns.example.org.") {
		t.Fatal("expiry removed authority coverage or diagnostic data")
	}
	if _, err := BuildSecondary(dz, secondaryData(t), time.Time{}); err == nil {
		t.Fatal("accepted data with no expiry")
	}
	for _, typ := range []v1alpha1.ZoneType{v1alpha1.ZoneTypePrimary, v1alpha1.ZoneTypeForward} {
		nonsecondary := mustBuild(t, dnsZone("example.org.", func(dz *v1alpha1.DNSZone) { dz.Spec.Type = typ }))
		if !nonsecondary.AvailableAt(time.Time{}) || !nonsecondary.AvailableAt(expires) || nonsecondary.SecondaryIdentity() != "" {
			t.Fatal("changed non-secondary availability or identity")
		}
	}
}

func TestSecondaryIdentity(t *testing.T) {
	dz := secondaryConfig()
	id := SecondaryIdentity(dz)
	if len(id) != len(HashPrefix)+64 || !strings.HasPrefix(id, HashPrefix) || SecondaryIdentity(nil) != "" {
		t.Fatal("incorrect SHA256 identity format")
	}
	placeholder := mustSecondary(t, dz, nil, time.Time{})
	loaded := mustSecondary(t, dz, secondaryData(t), testNow.Add(time.Hour))
	if placeholder.SecondaryIdentity() != id || loaded.SecondaryIdentity() != id || SecondaryIdentity(dz.DeepCopy()) != id {
		t.Fatal("runtime data and placeholder identities differ")
	}
	changed := dz.DeepCopy()
	changed.Generation++
	changed.ResourceVersion = "new"
	changed.Labels = map[string]string{"irrelevant": "yes"}
	changed.Status.Serial++
	changed.Status.NameserverAddresses = []string{"192.0.2.90"}
	if SecondaryIdentity(changed) != id {
		t.Fatal("metadata or status invalidates runtime identity")
	}
	for name, mutate := range map[string]func(*v1alpha1.DNSZone){
		"uid":             func(z *v1alpha1.DNSZone) { z.UID = "recreated" },
		"name":            func(z *v1alpha1.DNSZone) { z.Name = "renamed" },
		"namespace":       func(z *v1alpha1.DNSZone) { z.Namespace = "other" },
		"origin":          func(z *v1alpha1.DNSZone) { z.Spec.ZoneName = "example.net." },
		"primary":         func(z *v1alpha1.DNSZone) { z.Spec.Secondary.Primaries[0] = "192.0.2.99:53" },
		"key-ref":         func(z *v1alpha1.DNSZone) { z.Spec.Secondary.TSIGKeyRef = "other-key" },
		"protocol":        func(z *v1alpha1.DNSZone) { z.Spec.Secondary.TransferProtocol = "tls" },
		"query-policy":    func(z *v1alpha1.DNSZone) { z.Spec.Policy.QueryAccess = v1alpha1.AccessDeny },
		"transfer-policy": func(z *v1alpha1.DNSZone) { z.Spec.Policy.TransferACL = []string{"192.0.2.0/24"} },
		"other-spec":      func(z *v1alpha1.DNSZone) { z.Spec.DefaultTTL++ },
	} {
		t.Run(name, func(t *testing.T) {
			other := dz.DeepCopy()
			mutate(other)
			if SecondaryIdentity(other) == id {
				t.Fatal("configuration change does not invalidate identity")
			}
		})
	}
	// Map insertion order must not affect the JSON identity.
	dz.Spec.RecordSetSelector = &v1alpha1.RecordSetSelector{LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"a": "1", "b": "2"}}}
	changed = dz.DeepCopy()
	changed.Spec.RecordSetSelector.LabelSelector.MatchLabels = map[string]string{"b": "2", "a": "1"}
	if SecondaryIdentity(dz) != SecondaryIdentity(changed) {
		t.Fatal("map order changes identity")
	}
}

func TestSecondaryLookup(t *testing.T) {
	data := append(secondaryData(t), rrs(t,
		"web.example.org. 42 IN A 192.0.2.80",
		"alias.example.org. 43 IN CNAME web.example.org.",
		"out.example.org. 44 IN CNAME target.example.net.",
		"leaf.branch.example.org. 45 IN TXT leaf",
		"*.wild.example.org. 46 IN A 192.0.2.81",
		"child.example.org. 47 IN NS ns.child.example.org.",
		"ns.child.example.org. 48 IN A 192.0.2.82",
		"child.example.org. 49 IN DS 12345 13 2 00112233445566778899AABBCCDDEEFF00112233445566778899AABBCCDDEEFF",
		"dn.example.org. 50 IN DNAME example.net.",
		// Occluded raw data must survive import and export, unlike primary
		// endpoint validation, which deliberately rejects it.
		"hidden.dn.example.org. 51 IN A 192.0.2.83",
	)...)
	z := mustSecondary(t, secondaryConfig(), data, testNow.Add(time.Hour))
	for _, tc := range []struct {
		name    string
		typ     uint16
		kind    Kind
		answers int
	}{
		{"example.org.", dns.TypeSOA, Success, 1},
		{"WEB.EXAMPLE.ORG.", dns.TypeA, Success, 1},
		{"alias.example.org.", dns.TypeA, Success, 2},
		{"out.example.org.", dns.TypeA, CNAME, 1},
		{"branch.example.org.", dns.TypeA, NoData, 0},
		{"missing.example.org.", dns.TypeA, NXDomain, 0},
		{"web.example.org.", dns.TypeAAAA, NoData, 0},
		{"host.wild.example.org.", dns.TypeA, Success, 1},
		{"child.example.org.", dns.TypeA, Delegation, 0},
		{"child.example.org.", dns.TypeDS, Success, 1},
		{"hidden.dn.example.org.", dns.TypeA, CNAME, 2},
	} {
		t.Run(tc.name+"/"+typeString(tc.typ), func(t *testing.T) {
			r := z.Lookup(tc.name, tc.typ)
			if r.Kind != tc.kind || len(r.Answer) != tc.answers {
				t.Fatalf("lookup %+v", r)
			}
			if tc.kind == NXDomain || tc.kind == NoData {
				if len(r.Ns) != 1 || r.Ns[0].Header().Ttl != 90 || r.Ns[0].(*dns.SOA).Serial != 4294967295 {
					t.Fatal("negative SOA does not preserve serial and use minimum TTL")
				}
			}
			if tc.kind == Delegation && (len(r.Ns) != 1 || len(r.Extra) != 1 || r.Extra[0].Header().Ttl != 48) {
				t.Fatal("delegation glue missing")
			}
		})
	}
	if z.HasSignatures() {
		t.Fatal("unsigned data is marked signed")
	}
	before := secondaryWires(t, secondaryRecords(z))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 30 {
				z.Lookup("host.wild.example.org.", dns.TypeA)
				z.Lookup("hidden.dn.example.org.", dns.TypeA)
				z.Lookup("alias.example.org.", dns.TypeA)
			}
		})
	}
	wg.Wait()
	if !slices.Equal(before, secondaryWires(t, secondaryRecords(z))) {
		t.Fatal("concurrent lookup mutates secondary")
	}
}

func TestSecondaryTransfersAndPolicy(t *testing.T) {
	dz := secondaryConfig()
	dz.Spec.Policy = v1alpha1.ZonePolicy{QueryAccess: v1alpha1.AccessAllowOnlyZoneNameServers, ZoneTransfer: v1alpha1.AccessUseNetworkACL, TransferACL: []string{"192.0.2.0/24"}, TransferTSIGKeyRefs: []string{"XFR-Key"}, Notify: v1alpha1.NotifySpecified, NotifyTargets: []string{"192.0.2.99"}}
	data := secondaryData(t)
	expires := time.Now().Add(time.Hour)
	z := mustSecondary(t, dz, data, expires)
	client := netip.MustParseAddr("192.0.2.53")
	if !z.TransferAllowed(client, "xfr-key.") || z.TransferAllowed(client, "") || z.TransferAllowed(client, "wrong") || z.TransferAllowed(netip.MustParseAddr("203.0.113.1"), "xfr-key.") {
		t.Fatal("secondary transfer policy not applied")
	}
	if !z.QueryAllowed(client) || z.QueryAllowed(netip.MustParseAddr("192.0.2.54")) || z.NotifyMode() != v1alpha1.NotifySpecified || !slices.Equal(z.NotifyTargets(), []netip.AddrPort{netip.MustParseAddrPort("192.0.2.99:53")}) {
		t.Fatal("nameserver access/notify policy not retained")
	}
	if !reflect.DeepEqual(z.NameServers(), []NameServer{{Host: "NS.Example.ORG.", Addrs: []netip.Addr{client}}}) || !z.UpToDate(4294967295) || z.UpToDate(4294967294) {
		t.Fatal("nameservers or transfer serial comparison changed")
	}
	var transferred []dns.RR
	for chunk := range z.AXFR(80) {
		transferred = append(transferred, chunk...)
	}
	if len(transferred) != len(data)+1 || transferred[0].Header().Rrtype != dns.TypeSOA || !bytes.Equal(secondaryWire(t, transferred[0]), secondaryWire(t, transferred[len(transferred)-1])) || !slices.Equal(secondaryWires(t, transferred[:len(transferred)-1]), secondaryWires(t, data)) {
		t.Fatal("outbound AXFR lost raw records or SOA framing")
	}
	expired := mustSecondary(t, dz, data, time.Now().Add(-time.Hour))
	if expired.TransferAllowed(client, "xfr-key.") {
		t.Fatal("expired secondary allows outbound transfer")
	}
	for name, mutate := range map[string]func(*v1alpha1.DNSZone){
		"bad-mode": func(z *v1alpha1.DNSZone) { z.Spec.Policy.ZoneTransfer = "bad" },
		"bad-acl":  func(z *v1alpha1.DNSZone) { z.Spec.Policy.TransferACL = []string{"invalid"} },
		"bad-key":  func(z *v1alpha1.DNSZone) { z.Spec.Policy.TransferTSIGKeyRefs = []string{"a..b"} },
	} {
		t.Run(name, func(t *testing.T) {
			config := dz.DeepCopy()
			mutate(config)
			z := mustSecondary(t, config, data, expires)
			if z.TransferAllowed(client, "xfr-key.") {
				t.Fatal("invalid transfer policy did not fail closed")
			}
		})
	}
}

func TestSecondaryOrderAndRawKnownTypes(t *testing.T) {
	data := secondaryData(t)
	// Generic RFC 3597 input for a known type must decode to a concrete RR
	// before lookup uses the type assertions for NS/SOA/CNAME/DNAME.
	for i, r := range data {
		generic := new(dns.RFC3597)
		if err := generic.ToRFC3597(dns.Copy(r)); err != nil {
			t.Fatal(err)
		}
		data[i] = generic
	}
	z := mustSecondary(t, secondaryConfig(), data, testNow.Add(time.Hour))
	if z.Lookup("ns.example.org.", dns.TypeA).Kind != Success || len(z.NameServers()) != 1 {
		t.Fatal("known raw RR did not decode safely")
	}
	permuted := slices.Clone(data)
	slices.Reverse(permuted)
	other := mustSecondary(t, secondaryConfig(), permuted, testNow.Add(time.Hour))
	if z.ContentHash() != other.ContentHash() || !slices.Equal(secondaryWires(t, secondaryRecords(z)), secondaryWires(t, secondaryRecords(other))) {
		t.Fatal("input order affects content")
	}
	changed := secondaryData(t)
	changed[0].(*dns.SOA).Serial = 0
	other = mustSecondary(t, secondaryConfig(), changed, testNow.Add(time.Hour))
	if other.Serial() != 0 || other.ContentHash() == z.ContentHash() {
		t.Fatal("raw hash excludes serial or serial zero was restamped")
	}
	// Root authority uses the same key/index path as primary zones.
	root := secondaryConfig()
	root.Spec.ZoneName = "."
	rootData := rrs(t, ". 300 IN SOA ns.example. hostmaster.example. 0 1 2 3 4", ". 300 IN NS ns.example.", "ns.example. 300 IN A 192.0.2.1")
	rootZone := mustSecondary(t, root, rootData, testNow.Add(time.Hour))
	if !rootZone.Contains("www.example.net.") || rootZone.Lookup("ns.example.", dns.TypeA).Kind != Success {
		t.Fatal("root secondary lookup failed")
	}
}

func TestSecondaryInvalidConfig(t *testing.T) {
	for name, dz := range map[string]*v1alpha1.DNSZone{
		"nil":          nil,
		"primary":      dnsZone("example.org."),
		"empty-origin": dnsZone("", func(z *v1alpha1.DNSZone) { z.Spec.Type = v1alpha1.ZoneTypeSecondary }),
		"bad-origin":   dnsZone("a..b", func(z *v1alpha1.DNSZone) { z.Spec.Type = v1alpha1.ZoneTypeSecondary }),
		"bad-query": dnsZone("example.org.", func(z *v1alpha1.DNSZone) {
			z.Spec.Type = v1alpha1.ZoneTypeSecondary
			z.Spec.Policy.QueryACL = []string{"invalid"}
		}),
	} {
		t.Run(name, func(t *testing.T) {
			if z, err := BuildSecondary(dz, nil, time.Time{}); err == nil || z != nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
}

func TestSecondaryPlaceholderInvalidConfigCoverage(t *testing.T) {
	for name, mutate := range map[string]func(*v1alpha1.DNSZone){
		"query-acl": func(z *v1alpha1.DNSZone) {
			z.Spec.Policy.QueryAccess = v1alpha1.AccessUseNetworkACL
			z.Spec.Policy.QueryACL = []string{"invalid"}
		},
		"query-mode": func(z *v1alpha1.DNSZone) { z.Spec.Policy.QueryAccess = "bad" },
	} {
		t.Run(name, func(t *testing.T) {
			config := secondaryConfig()
			mutate(config)
			original := config.DeepCopy()
			z, err := BuildSecondaryPlaceholder(config)
			if err != nil || z == nil {
				t.Fatalf("invalid config lost coverage: %v", err)
			}
			if !reflect.DeepEqual(config, original) || z.SecondaryIdentity() != SecondaryIdentity(config) || !z.Contains("www.example.org.") || z.Type() != v1alpha1.ZoneTypeSecondary || z.AvailableAt(testNow) {
				t.Fatal("placeholder altered input/identity or lost unavailable coverage")
			}
			for _, client := range []netip.Addr{{}, netip.MustParseAddr("192.0.2.53"), netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("203.0.113.1")} {
				if z.QueryAllowed(client) || z.TransferAllowed(client, "") {
					t.Fatal("invalid configuration did not fail closed")
				}
			}
		})
	}
	valid := secondaryConfig()
	valid.Spec.Policy.QueryAccess = v1alpha1.AccessUseNetworkACL
	valid.Spec.Policy.QueryACL = []string{"192.0.2.0/24"}
	z, err := BuildSecondaryPlaceholder(valid)
	if err != nil || !z.QueryAllowed(netip.MustParseAddr("192.0.2.1")) || z.QueryAllowed(netip.MustParseAddr("203.0.113.1")) {
		t.Fatal("valid placeholder query ACL changed")
	}
	for _, config := range []*v1alpha1.DNSZone{nil, dnsZone("example.org."), dnsZone("a..b", func(z *v1alpha1.DNSZone) { z.Spec.Type = v1alpha1.ZoneTypeSecondary })} {
		if z, err := BuildSecondaryPlaceholder(config); err == nil || z != nil {
			t.Fatal("placeholder fabricated invalid apex or type coverage")
		}
	}
}

func TestSecondaryIgnoresSharedNameserverAddresses(t *testing.T) {
	dz := secondaryConfig()
	dz.Spec.Policy.QueryAccess = v1alpha1.AccessAllowOnlyZoneNameServers
	dz.Spec.Policy.ZoneTransfer = v1alpha1.AccessAllowOnlyZoneNameServers
	dz.Status.NameserverAddresses = []string{"192.0.2.99", "invalid-legacy-status"}
	z := mustSecondary(t, dz, secondaryData(t), time.Now().Add(time.Hour))
	if z.QueryAllowed(netip.MustParseAddr("192.0.2.99")) || z.TransferAllowed(netip.MustParseAddr("192.0.2.99"), "") {
		t.Fatal("legacy shared status expanded secondary authorization")
	}
	if !z.QueryAllowed(netip.MustParseAddr("192.0.2.53")) || !z.TransferAllowed(netip.MustParseAddr("192.0.2.53"), "") {
		t.Fatal("transferred apex nameserver glue did not authorize nameserver")
	}
}
