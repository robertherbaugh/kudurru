package zone

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestBuildReportsPerRecordSetErrors(t *testing.T) {
	sets := []*v1alpha1.DNSRecordSet{
		recordSet("team-a", "web", ep("web.example.org", "A", "192.0.2.1"), ep("bad.example.org", "A", "nope")),
		recordSet("team-b", "web", ep("web.example.org", "A", "192.0.2.2"), ep("api.example.org", "A", "192.0.2.3")),
	}
	z, errs := Build(dnsZone("example.org."), sets, BuildOptions{Now: testNow})
	if z == nil || len(errs.Zone) != 0 {
		t.Fatalf("zone must build: %v", errs.Zone)
	}
	a := errs.RecordSets["team-a/web"]
	if len(a) != 1 || a[0].Field != "spec.endpoints[1].targets[0]" {
		t.Fatalf("team-a errors: %v", a)
	}
	b := errs.RecordSets["team-b/web"]
	if len(b) != 1 || b[0].Field != "spec.endpoints[0].dnsName" || !strings.Contains(b[0].Error(), "Duplicated") || !strings.Contains(b[0].Error(), "team-a/web") {
		t.Fatalf("team-b errors: %v", b)
	}
	// The earlier definition wins; unrelated endpoints of the later object
	// are served.
	if r := z.Lookup("web.example.org.", dns.TypeA); len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != "192.0.2.1" {
		t.Fatalf("web: %v", r.Answer)
	}
	if r := z.Lookup("api.example.org.", dns.TypeA); r.Kind != Success {
		t.Fatalf("api: %v", r.Kind)
	}
	if r := z.Lookup("bad.example.org.", dns.TypeA); r.Kind != NXDomain {
		t.Fatalf("a failed endpoint must not be served: %v", r.Kind)
	}
}

func TestBuildMergeUnion(t *testing.T) {
	sets := []*v1alpha1.DNSRecordSet{
		recordSet("a", "x", epTTL("web.example.org", "A", 600, "192.0.2.1")),
		recordSet("b", "x", epTTL("web.example.org", "A", 60, "192.0.2.2", "192.0.2.1")),
	}
	z, errs := Build(dnsZone("example.org."), sets, BuildOptions{Now: testNow, MergeUnion: true})
	if !errs.Empty() {
		t.Fatalf("errors: %v", errs.RecordSets)
	}
	runCases(t, z, []Case{{Qname: "web.example.org.", Qtype: dns.TypeA, Kind: Success,
		Answer: []string{"web.example.org. 60 IN A 192.0.2.1", "web.example.org. 60 IN A 192.0.2.2"}}})
}

func TestBuildSameSourceMerges(t *testing.T) {
	z := mustBuild(t, dnsZone("example.org."),
		ep("web.example.org", "A", "192.0.2.1"),
		ep("web.example.org", "A", "192.0.2.2", "192.0.2.1"),
	)
	if r := z.Lookup("web.example.org.", dns.TypeA); len(r.Answer) != 2 {
		t.Fatalf("same-object endpoints merge with duplicates removed: %v", r.Answer)
	}
	if z.RecordCount() != 2+2 { // two A + two apex NS
		t.Fatalf("record count %d", z.RecordCount())
	}
}

func TestBuildRejectsConflicts(t *testing.T) {
	for _, tc := range []struct {
		name string
		eps  []v1alpha1.Endpoint
		want string
	}{
		{"cname-after-data", []v1alpha1.Endpoint{ep("x.example.org", "A", "192.0.2.1"), ep("x.example.org", "CNAME", "y.example.org")}, "cannot coexist"},
		{"data-after-cname", []v1alpha1.Endpoint{ep("x.example.org", "CNAME", "y.example.org"), ep("x.example.org", "TXT", "t")}, "already has a CNAME"},
		{"apex-cname", []v1alpha1.Endpoint{ep("@", "CNAME", "y.example.net")}, "apex"},
		{"apex-ns", []v1alpha1.Endpoint{ep("example.org", "NS", "ns.example.net")}, "spec.nameservers"},
		{"two-cnames-same-object", []v1alpha1.Endpoint{ep("x.example.org", "CNAME", "a.example.net"), ep("x.example.org", "CNAME", "b.example.net")}, "at most one CNAME"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := Build(dnsZone("example.org."), []*v1alpha1.DNSRecordSet{recordSet("ns", "rs", tc.eps...)}, BuildOptions{Now: testNow})
			got := errs.RecordSets["ns/rs"]
			if len(got) != 1 || !strings.Contains(got[0].Error(), tc.want) || got[0].Field != "spec.endpoints[1].dnsName" && got[0].Field != "spec.endpoints[0].dnsName" && got[0].Field != "spec.endpoints[1].targets" {
				t.Fatalf("errors %v, want one containing %q", got, tc.want)
			}
		})
	}
}

func TestNewBuilderSpecErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*v1alpha1.DNSZone)
		field  string
	}{
		{"bad-zone-name", func(dz *v1alpha1.DNSZone) { dz.Spec.ZoneName = "a..b." }, "spec.zoneName"},
		{"secondary-not-yet", func(dz *v1alpha1.DNSZone) { dz.Spec.Type = v1alpha1.ZoneTypeSecondary }, "spec.type"},
		{"bad-acl", func(dz *v1alpha1.DNSZone) {
			dz.Spec.Policy.QueryAccess = v1alpha1.AccessUseNetworkACL
			dz.Spec.Policy.QueryACL = []string{"10.0.0.0/8", "10.0.0.300"}
		}, "spec.policy.queryACL[1]"},
		{"bad-mode", func(dz *v1alpha1.DNSZone) { dz.Spec.Policy.QueryAccess = "sometimes" }, "spec.policy.queryAccess"},
		{"bad-nameserver", func(dz *v1alpha1.DNSZone) { dz.Spec.Nameservers = []string{"ns1.example.org", "bad..name"} }, "spec.nameservers[1]"},
		{"bad-ns-address", func(dz *v1alpha1.DNSZone) { dz.Status.NameserverAddresses = []string{"10.0.0.1", "ten"} }, "status.nameserverAddresses[1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, errs := NewBuilder(dnsZone("example.org.", tc.mutate), BuildOptions{})
			if b != nil || len(errs) != 1 || errs[0].Field != tc.field {
				t.Fatalf("errors %v, want one at %s", errs, tc.field)
			}
		})
	}
}

func TestSOADefaultsAndOverrides(t *testing.T) {
	z := mustBuild(t, dnsZone("example.org.", func(dz *v1alpha1.DNSZone) {
		dz.Spec.SOA = &v1alpha1.SOASpec{MName: "primary.example.net", RName: "first.last@example.net", Refresh: 7200, Minimum: 30}
	}))
	want := `example.org. 300 IN SOA primary.example.net. first\.last.example.net. 2026101100 7200 900 1209600 30`
	if got := z.SOA().String(); got != rr(t, want).String() {
		t.Fatalf("SOA %s, want %s", got, want)
	}
	z = mustBuild(t, dnsZone("example.org.", func(dz *v1alpha1.DNSZone) {
		dz.Spec.SOA = nil
		dz.Spec.Nameservers = nil
		dz.Spec.DefaultTTL = 0
		dz.Status.Serial = 0
	}))
	want = "example.org. 300 IN SOA ns1.example.org. hostmaster.example.org. 0 3600 900 1209600 300"
	if got := z.SOA().String(); got != rr(t, want).String() {
		t.Fatalf("SOA %s, want %s", got, want)
	}
	if r := z.Lookup("example.org.", dns.TypeNS); r.Kind != NoData {
		t.Fatalf("no nameservers configured: NS is NODATA, got %v", r.Kind)
	}
	s := z.SOA()
	s.Serial = 99
	if z.SOA().Serial == 99 {
		t.Fatal("SOA() must return a copy")
	}
}

func TestForwardZone(t *testing.T) {
	dz := dnsZone("corp.example.", func(dz *v1alpha1.DNSZone) {
		dz.Spec.Type = v1alpha1.ZoneTypeForward
		dz.Spec.Forward = &v1alpha1.ForwardSpec{UpstreamGroup: "corp"}
	})
	b, errs := NewBuilder(dz, BuildOptions{})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	e := ep("x.corp.example", "A", "192.0.2.1")
	if errs := b.Add("ns/rs", nil, &e); len(errs) != 1 {
		t.Fatalf("forward zones take no records: %v", errs)
	}
	z := b.Build()
	if z.Type() != v1alpha1.ZoneTypeForward || z.UpstreamGroup() != "corp" || z.SOA() != nil || !strings.HasPrefix(z.ContentHash(), HashPrefix) {
		t.Fatalf("forward zone: %+v", z)
	}
	if r := z.Lookup("x.corp.example.", dns.TypeA); r.Kind != CNAME || r.Target != "x.corp.example." {
		t.Fatalf("lookup on a forward zone continues elsewhere: %+v", r)
	}
	dz.Spec.Forward = nil
	z, _ = Build(dz, nil, BuildOptions{})
	if z.UpstreamGroup() != DefaultUpstreamGroup {
		t.Fatalf("default group: %q", z.UpstreamGroup())
	}
}

func TestNextExpiryAndServed(t *testing.T) {
	soon := testNow.Add(10 * time.Minute)
	later := testNow.Add(time.Hour)
	e1 := ep("a.example.org", "A", "192.0.2.1")
	e1.Kudurru = &v1alpha1.EndpointExt{ExpiresAt: &metav1.Time{Time: later}}
	e2 := ep("b.example.org", "A", "192.0.2.2")
	e2.Kudurru = &v1alpha1.EndpointExt{ExpiresAt: &metav1.Time{Time: soon}}
	z := mustBuild(t, dnsZone("example.org."), e1, e2)
	if !z.NextExpiry().Equal(soon) {
		t.Fatalf("next expiry %v, want %v", z.NextExpiry(), soon)
	}
	z2, _ := Build(dnsZone("example.org."), []*v1alpha1.DNSRecordSet{recordSet("n", "r", e1, e2)}, BuildOptions{Now: soon})
	if z2.Lookup("b.example.org.", dns.TypeA).Kind != NXDomain || !z2.NextExpiry().Equal(later) {
		t.Fatal("rebuilding at the expiry instant drops the record")
	}
	if !Served(&v1alpha1.Endpoint{}, testNow) {
		t.Fatal("plain endpoints are served")
	}
}

func TestZoneAccessors(t *testing.T) {
	z := exampleZone(t)
	if z.Name() != "example.org." || z.Serial() != 2026101100 || z.Generation() != 1 || z.Type() != v1alpha1.ZoneTypePrimary {
		t.Fatal("accessors")
	}
	if !z.Contains("A.EXAMPLE.ORG") || z.Contains("example.net.") || z.Contains("xexample.org.") {
		t.Fatal("Contains")
	}
	if z.Names() < z.RecordCount()/3 {
		t.Fatalf("names %d records %d", z.Names(), z.RecordCount())
	}
	first := true
	n := 0
	z.RRsets(func(s *RRset) bool {
		if first && s.Type != dns.TypeSOA {
			t.Fatal("SOA must come first")
		}
		if !first && s.Type == dns.TypeSOA {
			t.Fatal("SOA listed twice")
		}
		first = false
		n += len(s.RRs)
		if s.TypeString() == "" {
			t.Fatal("type string")
		}
		return true
	})
	if n != z.RecordCount()+1 {
		t.Fatalf("RRsets yielded %d records, RecordCount %d (+SOA)", n, z.RecordCount())
	}
}

func TestQueryAllowedWithZoneNameServers(t *testing.T) {
	dz := dnsZone("example.org.", func(dz *v1alpha1.DNSZone) {
		dz.Spec.Policy.QueryAccess = v1alpha1.AccessZoneNameServersAndACL
		dz.Spec.Policy.QueryACL = []string{"!10.1.2.3", "10.0.0.0/8"}
		dz.Status.NameserverAddresses = []string{"198.51.100.7"}
	})
	z := mustBuild(t, dz, ep("ns1.example.org", "A", "192.0.2.1"), ep("ns2.example.org", "AAAA", "2001:db8::2"))
	for addr, want := range map[string]bool{
		"192.0.2.1":    true,  // in-zone glue of an apex NS
		"2001:db8::2":  true,  // ditto, IPv6
		"198.51.100.7": true,  // status.nameserverAddresses
		"10.9.9.9":     true,  // ACL
		"10.1.2.3":     false, // negated before the /8
		"192.0.2.99":   false,
	} {
		if got := z.QueryAllowed(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: %v, want %v", addr, got, want)
		}
	}
}
