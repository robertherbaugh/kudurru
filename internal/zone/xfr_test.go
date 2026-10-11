package zone

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func xfrZone(t testing.TB, n int) *Zone {
	t.Helper()
	eps := []v1alpha1.Endpoint{ep("ns1.example.org", "A", "192.0.2.1"), ep("example.org", "MX", "10 mail.example.org")}
	for i := range n {
		eps = append(eps, ep(fmt.Sprintf("h%03d.example.org", i), "TXT", strings.Repeat("t", 40)))
	}
	return mustBuild(t, dnsZone("example.org."), eps...)
}

func TestAXFRChunks(t *testing.T) {
	z := xfrZone(t, 300)
	var want []string
	z.RRsets(func(s *RRset) bool {
		for _, r := range s.RRs {
			want = append(want, r.String())
		}
		return true
	})
	want = append(want, z.SOA().String())
	if len(want) != z.RecordCount()+2 {
		t.Fatalf("stream has %d RRs, want RecordCount+2 = %d", len(want), z.RecordCount()+2)
	}
	for _, budget := range []int{0, 512, 4096, 1} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			var got []string
			chunks := 0
			for c := range z.AXFR(budget) {
				chunks++
				size := 0
				for _, r := range c {
					size += dns.Len(r)
					got = append(got, r.String())
				}
				limit := budget
				if limit <= 0 {
					limit = DefaultChunkBytes
				}
				if size > limit && len(c) > 1 {
					t.Fatalf("chunk of %d RRs is %d octets, budget %d", len(c), size, limit)
				}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("stream differs: %d vs %d RRs", len(got), len(want))
			}
			if got[0] != z.SOA().String() || got[len(got)-1] != z.SOA().String() {
				t.Fatal("SOA must be first and last")
			}
			switch budget {
			case 0:
				if chunks != 1 {
					t.Fatalf("%d chunks for a small zone with the default budget", chunks)
				}
			case 1:
				if chunks != len(want) {
					t.Fatalf("a 1-octet budget gives one RR per chunk: %d chunks", chunks)
				}
			default:
				if chunks < 2 {
					t.Fatalf("%d chunks with budget %d", chunks, budget)
				}
			}
		})
	}
	// Stopping early ends the walk; the sequence can be iterated again.
	n := 0
	for range z.AXFR(512) {
		n++
		break
	}
	if n != 1 || len(slices.Collect(z.AXFR(512))) < 2 {
		t.Fatal("AXFR sequence must stop early and be reusable")
	}
	fwd := mustBuild(t, dnsZone("corp.example.", func(dz *v1alpha1.DNSZone) { dz.Spec.Type = v1alpha1.ZoneTypeForward }))
	if len(slices.Collect(fwd.AXFR(0))) != 0 {
		t.Fatal("forward zones have nothing to transfer")
	}
}

func TestSerialArithmetic(t *testing.T) {
	for _, tc := range []struct {
		a, b  uint32
		newer bool
	}{
		{2, 1, true}, {1, 2, false}, {1, 1, false},
		{0, 0xffffffff, true}, {0xffffffff, 0, false},
		{1 << 31, 0, false}, {0, 1 << 31, false}, // exactly 2^31 apart: undefined
		{(1 << 31) - 1, 0, true},
	} {
		if got := SerialNewer(tc.a, tc.b); got != tc.newer {
			t.Errorf("SerialNewer(%d, %d) = %v", tc.a, tc.b, got)
		}
	}
	z := xfrZone(t, 1) // serial 2026101100
	for serial, want := range map[uint32]bool{2026101100: true, 2026101101: true, 2026101099: false, 0: false} {
		if got := z.UpToDate(serial); got != want {
			t.Errorf("UpToDate(%d) = %v", serial, got)
		}
	}
}

func TestTransferAllowed(t *testing.T) {
	ns := func(dz *v1alpha1.DNSZone) {
		dz.Status.NameserverAddresses = []string{"203.0.113.9"}
	}
	build := func(p v1alpha1.ZonePolicy) *Zone {
		return mustBuild(t, dnsZone("example.org.", ns, func(dz *v1alpha1.DNSZone) { dz.Spec.Policy = p }),
			ep("ns1.example.org", "A", "192.0.2.53"), ep("ns2.example.org", "AAAA", "2001:db8::53"))
	}
	const (
		lan     = "10.1.2.3"
		denied  = "10.0.0.66"
		public  = "198.51.100.7"
		glue4   = "192.0.2.53"
		glue6   = "2001:db8::53"
		status  = "203.0.113.9"
		mapped  = "::ffff:192.0.2.53"
		loop6   = "::1"
		keyName = "xfr-key."
	)
	acl := []string{"!" + denied, "10.0.0.0/8"}
	for _, tc := range []struct {
		name    string
		policy  v1alpha1.ZonePolicy
		allowed []string
		refused []string
	}{
		{"default-deny", v1alpha1.ZonePolicy{}, nil, []string{lan, glue4, loop6}},
		{"deny", v1alpha1.ZonePolicy{ZoneTransfer: v1alpha1.AccessDeny}, nil, []string{lan, glue4}},
		{"allow", v1alpha1.ZonePolicy{ZoneTransfer: v1alpha1.AccessAllow}, []string{lan, public, glue6}, nil},
		{"private", v1alpha1.ZonePolicy{ZoneTransfer: v1alpha1.AccessAllowOnlyPrivate}, []string{lan, loop6}, []string{public, glue4}},
		{"nameservers", v1alpha1.ZonePolicy{ZoneTransfer: v1alpha1.AccessAllowOnlyZoneNameServers}, []string{glue4, glue6, status, mapped}, []string{lan, public}},
		{"acl", v1alpha1.ZonePolicy{ZoneTransfer: v1alpha1.AccessUseNetworkACL, TransferACL: acl}, []string{lan}, []string{denied, public, glue4}},
		{"nameservers-and-acl", v1alpha1.ZonePolicy{ZoneTransfer: v1alpha1.AccessZoneNameServersAndACL, TransferACL: acl}, []string{lan, glue4, status}, []string{denied, public}},
		// queryACL never grants transfers.
		{"query-acl-ignored", v1alpha1.ZonePolicy{ZoneTransfer: v1alpha1.AccessUseNetworkACL, QueryACL: []string{"0.0.0.0/0"}}, nil, []string{lan, public}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			z := build(tc.policy)
			for _, a := range tc.allowed {
				if !z.TransferAllowed(netip.MustParseAddr(a), "") {
					t.Errorf("%s refused", a)
				}
			}
			for _, a := range tc.refused {
				if z.TransferAllowed(netip.MustParseAddr(a), "") {
					t.Errorf("%s allowed", a)
				}
			}
		})
	}

	// transferTSIGKeyRefs adds a key requirement to the address check.
	keyed := build(v1alpha1.ZonePolicy{ZoneTransfer: v1alpha1.AccessUseNetworkACL, TransferACL: acl, TransferTSIGKeyRefs: []string{"XFR-Key", "other."}})
	for _, tc := range []struct {
		client, key string
		want        bool
	}{
		{lan, keyName, true},
		{lan, "XFR-KEY.", true},
		{lan, "other.", true},
		{lan, "", false},
		{lan, "stranger.", false},
		{public, keyName, false}, // a good key from a refused address
	} {
		if got := keyed.TransferAllowed(netip.MustParseAddr(tc.client), tc.key); got != tc.want {
			t.Errorf("client %s key %q: %v, want %v", tc.client, tc.key, got, tc.want)
		}
	}
	if !slices.Equal(keyed.TransferKeys(), []string{keyName, "other."}) {
		t.Fatalf("keys %v", keyed.TransferKeys())
	}
	fwd := mustBuild(t, dnsZone("corp.example.", func(dz *v1alpha1.DNSZone) {
		dz.Spec.Type = v1alpha1.ZoneTypeForward
		dz.Spec.Policy.ZoneTransfer = v1alpha1.AccessAllow
	}))
	if fwd.TransferAllowed(netip.MustParseAddr(lan), "") {
		t.Fatal("forward zones are never transferable")
	}
}

func TestNotifyPolicy(t *testing.T) {
	z := mustBuild(t, dnsZone("example.org.", func(dz *v1alpha1.DNSZone) {
		dz.Spec.Nameservers = []string{"ns1.example.org", "ns2.example.net", "ns3.example.org"}
		dz.Spec.Policy.Notify = v1alpha1.NotifyBoth
		dz.Spec.Policy.NotifyTargets = []string{"192.0.2.10", "192.0.2.11:5353", "[2001:db8::1]:5300", "::ffff:192.0.2.12"}
	}), ep("ns1.example.org", "A", "192.0.2.1"), ep("ns1.example.org", "AAAA", "2001:db8::a"))
	if z.NotifyMode() != v1alpha1.NotifyBoth {
		t.Fatalf("mode %s", z.NotifyMode())
	}
	want := []netip.AddrPort{
		netip.MustParseAddrPort("192.0.2.10:53"), netip.MustParseAddrPort("192.0.2.11:5353"),
		netip.MustParseAddrPort("[2001:db8::1]:5300"), netip.MustParseAddrPort("192.0.2.12:53"),
	}
	if !slices.Equal(z.NotifyTargets(), want) {
		t.Fatalf("targets %v", z.NotifyTargets())
	}
	got := z.NameServers()
	// Canonical rdata order; only in-zone hosts with glue have addresses.
	if len(got) != 3 || got[0].Host != "ns1.example.org." || len(got[0].Addrs) != 2 || got[1].Host != "ns2.example.net." || len(got[1].Addrs) != 0 || got[2].Host != "ns3.example.org." || len(got[2].Addrs) != 0 {
		t.Fatalf("nameservers %+v", got)
	}
	if def := mustBuild(t, dnsZone("example.org.")); def.NotifyMode() != v1alpha1.NotifyNone || len(def.NotifyTargets()) != 0 {
		t.Fatal("notify defaults to none")
	}

	// Transfer and notify policy errors do not take the zone off the air:
	// they fail closed and are reported as policy errors.
	lan := netip.MustParseAddr("10.0.0.1")
	for _, tc := range []struct {
		name    string
		policy  v1alpha1.ZonePolicy
		field   string
		targets int
	}{
		{"bad-target", v1alpha1.ZonePolicy{Notify: v1alpha1.NotifySpecified, NotifyTargets: []string{"192.0.2.1", "ns1.example.org"}}, "spec.policy.notifyTargets[1]", 1},
		{"port-zero", v1alpha1.ZonePolicy{NotifyTargets: []string{"192.0.2.1:0"}}, "spec.policy.notifyTargets[0]", 0},
		{"bad-mode", v1alpha1.ZonePolicy{Notify: "loudly"}, "spec.policy.notify", 0},
		{"bad-transfer-mode", v1alpha1.ZonePolicy{ZoneTransfer: "maybe"}, "spec.policy.zoneTransfer", 0},
		{"bad-transfer-acl", v1alpha1.ZonePolicy{ZoneTransfer: v1alpha1.AccessUseNetworkACL, TransferACL: []string{"10.0.0.0/8", "10.0.0.0/33"}}, "spec.policy.transferACL[1]", 0},
		{"bad-key-ref", v1alpha1.ZonePolicy{ZoneTransfer: v1alpha1.AccessAllow, TransferTSIGKeyRefs: []string{"good.", "a..b"}}, "spec.policy.transferTSIGKeyRefs[1]", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			z, errs := Build(dnsZone("example.org.", func(dz *v1alpha1.DNSZone) { dz.Spec.Policy = tc.policy }), nil, BuildOptions{Now: testNow})
			if z == nil || len(errs.Zone) != 0 || len(errs.Policy) != 1 || errs.Policy[0].Field != tc.field || errs.Empty() {
				t.Fatalf("zone %v, errors %+v; want a served zone and one policy error at %s", z, errs, tc.field)
			}
			if z.TransferAllowed(lan, "good.") {
				t.Fatal("transfers must be denied while their policy is in error")
			}
			if len(z.NotifyTargets()) != tc.targets {
				t.Fatalf("targets %v", z.NotifyTargets())
			}
			if tc.name == "bad-mode" && z.NotifyMode() != v1alpha1.NotifyNone {
				t.Fatalf("an invalid notify mode is none, got %s", z.NotifyMode())
			}
		})
	}
	if _, errs := Build(dnsZone("example.org.", func(dz *v1alpha1.DNSZone) {
		dz.Spec.Policy.QueryACL = []string{"bogus"}
	}), nil, BuildOptions{}); len(errs.Zone) != 1 {
		t.Fatal("queryACL errors still stop the zone (failing open would leak it)")
	}
}

func TestOcclusion(t *testing.T) {
	for _, tc := range []struct {
		name string
		eps  []v1alpha1.Endpoint
		bad  int // index of the refused endpoint, -1 for none
	}{
		{"data-below-dname", []v1alpha1.Endpoint{ep("dn.example.org", "DNAME", "example.net"), ep("www.dn.example.org", "A", "192.0.2.1")}, 1},
		{"dname-above-data", []v1alpha1.Endpoint{ep("a.b.dn.example.org", "TXT", "x"), ep("dn.example.org", "DNAME", "example.net")}, 1},
		{"wildcard-below-dname", []v1alpha1.Endpoint{ep("dn.example.org", "DNAME", "example.net"), ep("*.dn.example.org", "A", "192.0.2.1")}, 1},
		{"dname-below-dname", []v1alpha1.Endpoint{ep("dn.example.org", "DNAME", "example.net"), ep("x.dn.example.org", "DNAME", "example.com")}, 1},
		{"apex-dname", []v1alpha1.Endpoint{ep("@", "DNAME", "example.net"), ep("www.example.org", "A", "192.0.2.1")}, 1},
		{"data-at-dname-owner", []v1alpha1.Endpoint{ep("dn.example.org", "DNAME", "example.net"), ep("dn.example.org", "A", "192.0.2.1")}, -1},
		{"sibling", []v1alpha1.Endpoint{ep("dn.example.org", "DNAME", "example.net"), ep("dn2.example.org", "A", "192.0.2.1"), ep("example.org", "TXT", "x")}, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			z, errs := Build(dnsZone("example.org."), []*v1alpha1.DNSRecordSet{recordSet("ns", "rs", tc.eps...)}, BuildOptions{Now: testNow})
			got := errs.RecordSets["ns/rs"]
			if tc.bad < 0 {
				if len(got) != 0 {
					t.Fatalf("unexpected errors %v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Field != fmt.Sprintf("spec.endpoints[%d].dnsName", tc.bad) || ErrorReason(got[0]) != v1alpha1.ReasonOccluded {
				t.Fatalf("errors %v, want endpoint %d Occluded", got, tc.bad)
			}
			// The refused endpoint is not in the zone (nor in AXFR).
			name := tc.eps[tc.bad].DNSName
			for c := range z.AXFR(0) {
				for _, r := range c {
					if r.Header().Name == dns.Fqdn(name) && typeString(r.Header().Rrtype) == tc.eps[tc.bad].RecordType {
						t.Fatalf("occluded record %s is served", r)
					}
				}
			}
		})
	}
}

func TestConflictReasons(t *testing.T) {
	sets := []*v1alpha1.DNSRecordSet{
		recordSet("a", "first", ep("x.example.org", "A", "192.0.2.1"), ep("y.example.org", "CNAME", "z.example.org")),
		recordSet("b", "later",
			ep("x.example.org", "CNAME", "elsewhere.example.net"), // CNAME onto another object's data
			ep("y.example.org", "TXT", "t"),                       // data onto another object's CNAME
			ep("x.example.org", "A", "192.0.2.9"),                 // identity duplicate
			ep("outside.example.net", "A", "192.0.2.2"),           // outside
			ep("w.example.org", "A", "192.0.2.3"),
			ep("w.example.org", "CNAME", "v.example.org"), // conflict within this object
		),
	}
	_, errs := Build(dnsZone("example.org."), sets, BuildOptions{Now: testNow})
	got := errs.RecordSets["b/later"]
	want := []struct {
		reason string
		typ    field.ErrorType
	}{
		{v1alpha1.ReasonDuplicated, field.ErrorTypeInvalid},
		{v1alpha1.ReasonDuplicated, field.ErrorTypeInvalid},
		{v1alpha1.ReasonDuplicated, field.ErrorTypeDuplicate},
		{v1alpha1.ReasonOutsideZone, field.ErrorTypeInvalid},
		{v1alpha1.ReasonInvalid, field.ErrorTypeInvalid},
	}
	if len(got) != len(want) {
		t.Fatalf("errors %v", got)
	}
	for i, w := range want {
		if r := ErrorReason(got[i]); r != w.reason || got[i].Type != w.typ {
			t.Errorf("error %d (%v): reason %s type %s, want %s %s", i, got[i], r, got[i].Type, w.reason, w.typ)
		}
	}
	if ErrorReason(nil) != "" {
		t.Fatal("nil error has no reason")
	}
}

func TestIdentityRules(t *testing.T) {
	eu := ep("geo.example.org", "A", "192.0.2.20")
	eu.SetIdentifier = "eu"
	us := ep("geo.example.org", "A", "192.0.2.21")
	us.SetIdentifier = "us"
	off := ep("off.example.org", "A", "192.0.2.30")
	off.Kudurru = &v1alpha1.EndpointExt{Disabled: true}
	z, errs := Build(dnsZone("example.org."), []*v1alpha1.DNSRecordSet{
		recordSet("a", "eu", eu, off),
		recordSet("b", "us", us, ep("off.example.org", "A", "192.0.2.31")),
	}, BuildOptions{Now: testNow})
	// Different set identifiers are different identities: their records
	// are served together.
	if r := z.Lookup("geo.example.org.", dns.TypeA); len(r.Answer) != 2 {
		t.Fatalf("set identifiers must union: %v", r.Answer)
	}
	// A disabled endpoint keeps its identity: the later object's same
	// identity is Duplicated and not served.
	got := errs.RecordSets["b/us"]
	if len(got) != 1 || got[0].Type != field.ErrorTypeDuplicate || !strings.Contains(got[0].Detail, "a/eu") {
		t.Fatalf("errors %v", errs.RecordSets)
	}
	if r := z.Lookup("off.example.org.", dns.TypeA); r.Kind != NXDomain {
		t.Fatalf("taken-over identity served: %v", r.Answer)
	}
}

func TestGate(t *testing.T) {
	z := xfrZone(t, 1)
	for _, tc := range []struct {
		synced bool
		gate   Gate
	}{{true, GateOpen}, {false, GateUnsynced}} {
		s, err := NewSnapshot(3, tc.synced, z)
		if err != nil {
			t.Fatal(err)
		}
		if s.Gate() != tc.gate || s.Synced() != tc.synced {
			t.Fatalf("synced=%v: gate %s", tc.synced, s.Gate())
		}
	}
	s, _ := NewSnapshot(3, true, z)
	stale := s.WithGate(GateStale)
	if stale.Gate() != GateStale || stale.Synced() || stale.Generation() != 3 || stale.Match("www.example.org.") != z {
		t.Fatal("WithGate keeps the zones and generation")
	}
	if s.Gate() != GateOpen || s.WithGate(GateOpen) != s {
		t.Fatal("WithGate must not modify the receiver and returns it when unchanged")
	}
	if s.WithSynced(4, false).Gate() != GateUnsynced || stale.WithSynced(5, true).Gate() != GateOpen {
		t.Fatal("WithSynced maps onto the gate")
	}
	for g, want := range map[Gate]string{GateOpen: "open", GateUnsynced: "unsynced", GateStale: "stale", 9: "unknown"} {
		if g.String() != want {
			t.Errorf("%d: %s", g, g)
		}
	}
}

func TestServedRecordSets(t *testing.T) {
	at := func(rs *v1alpha1.DNSRecordSet, sec int, labels map[string]string) *v1alpha1.DNSRecordSet {
		rs.CreationTimestamp = metav1.NewTime(testNow.Add(time.Duration(sec) * time.Second))
		rs.Labels = labels
		return rs
	}
	pub := map[string]string{"dns": "public"}
	sets := []*v1alpha1.DNSRecordSet{
		at(recordSet("apps", "late", ep("a.example.org", "A", "192.0.2.1")), 30, pub),
		at(recordSet("apps", "early", ep("b.example.org", "A", "192.0.2.2")), 10, pub),
		at(recordSet("apps", "nolabel", ep("c.example.org", "A", "192.0.2.3")), 0, nil),
		at(recordSet("other", "ns", ep("d.example.org", "A", "192.0.2.4")), 0, pub),
		at(recordSet("apps", "invalid", ep("e.example.org", "A", "not-an-ip")), 0, pub),
		at(recordSet("apps", "dup", ep("f.example.org", "A", "192.0.2.5"), ep("F.example.org.", "a", "192.0.2.6")), 0, pub),
		at(recordSet("apps", "tie-b", ep("g.example.org", "A", "192.0.2.7")), 20, pub),
		at(recordSet("apps", "tie-a", ep("h.example.org", "A", "192.0.2.8")), 20, pub),
	}
	dz := dnsZone("example.org.", func(dz *v1alpha1.DNSZone) {
		dz.Spec.RecordSetSelector = &v1alpha1.RecordSetSelector{Namespaces: []string{"apps"}, LabelSelector: &metav1.LabelSelector{MatchLabels: pub}}
	})
	var names []string
	for _, rs := range ServedRecordSets(dz, sets) {
		names = append(names, rs.Name)
	}
	if want := []string{"early", "tie-a", "tie-b", "late"}; !slices.Equal(names, want) {
		t.Fatalf("served %v, want %v", names, want)
	}
	if sets[0].Name != "late" {
		t.Fatal("input reordered")
	}
	bad := dnsZone("example.org.", func(dz *v1alpha1.DNSZone) {
		dz.Spec.RecordSetSelector = &v1alpha1.RecordSetSelector{LabelSelector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "dns", Operator: "Sometimes"}},
		}}
	})
	if got := ServedRecordSets(bad, sets); len(got) != 0 {
		t.Fatalf("a broken selector admits nothing, got %d", len(got))
	}
	if got := ServedRecordSets(dnsZone("example.org."), sets); len(got) != 6 {
		t.Fatalf("no selector: %d served, want the 6 valid sets", len(got))
	}
}

func TestValidateEndpoint(t *testing.T) {
	for _, tc := range []struct {
		ep   v1alpha1.Endpoint
		want string
	}{
		{ep("www.example.org", "A", "192.0.2.1"), ""},
		{ep("www.example.org", "a", "192.0.2.1"), ""},
		{ep("*.example.org", "TXT", "free text"), ""},
		{ep("www.example.org", "A", "999.0.0.1"), "not an IPv4 address"},
		{ep("www.example.org", "AAAA", "192.0.2.1"), "not an IPv6 address"},
		{ep("www.example.org", "CNAME", "a.example.org", "b.example.org"), "Too many"},
		{ep("bad name.example.org", "A", "192.0.2.1"), "dnsName"},
		{ep("www.example.org", "SOA", "x"), "recordType"},
		{ep("www.example.org", "A"), "at least one target"},
	} {
		err := ValidateEndpoint(tc.ep)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%v: %v", tc.ep, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%v: %v, want %q", tc.ep, err, tc.want)
		}
	}
	if EndpointIdentity(ep("WWW.Example.org", "a", "x")) != EndpointIdentity(ep("www.example.org.", "A", "y")) {
		t.Fatal("identity ignores case, the trailing dot and targets")
	}
}
