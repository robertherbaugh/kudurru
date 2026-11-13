package zone

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func inView(e v1alpha1.Endpoint, view string) v1alpha1.Endpoint {
	e.Kudurru = &v1alpha1.EndpointExt{View: view}
	return e
}

// viewZone is split horizon in one record set (namespace "dns"): a default
// answer for www, a lan answer, a kids answer, a lan-only name, a lan CNAME
// that replaces default data, and a publish-only endpoint.
func viewZone(t *testing.T) (*Zone, Errors) {
	t.Helper()
	set := recordSet("dns", "split",
		ep("www.example.org", "A", "203.0.113.10"),
		ep("www.example.org", "TXT", "public"),
		inView(ep("www.example.org", "A", "10.0.0.10"), "lan"),
		inView(ep("www.example.org", "A", "10.9.0.10"), "kids"),
		inView(ep("nas.example.org", "A", "10.0.0.20"), "lan"),
		inView(ep("deep.only.example.org", "AAAA", "fd00::1"), "lan"),
		ep("app.example.org", "A", "203.0.113.11"),
		ep("app.example.org", "MX", "10 mail.example.org"),
		inView(ep("app.example.org", "CNAME", "nas.example.org"), "lan"),
		ep("alias.example.org", "CNAME", "www.example.org"),
		inView(ep("alias.example.org", "A", "10.0.0.30"), "kids"),
		inView(ep("tunnel.example.org", "CNAME", "tunnel.cloudflare.example"), "public"),
	)
	if err := RecordSetError(set); err != nil {
		t.Fatalf("split-horizon record set must be valid: %v", err)
	}
	return Build(dnsZone("example.org."), []*v1alpha1.DNSRecordSet{set}, BuildOptions{Now: testNow})
}

func addrs(res Result) []string {
	var out []string
	for _, rr := range res.Answer {
		switch r := rr.(type) {
		case *dns.A:
			out = append(out, r.A.String())
		case *dns.AAAA:
			out = append(out, r.AAAA.String())
		case *dns.CNAME:
			out = append(out, "CNAME "+r.Target)
		}
	}
	return out
}

func TestClientGroupViewsAnswerPerGroup(t *testing.T) {
	z, errs := viewZone(t)
	if !errs.Empty() {
		t.Fatalf("errors: %v %v", errs.Zone, errs.RecordSets)
	}
	if !z.HasViews() || !slices.Equal(z.ViewGroups(), []string{"dns/kids", "dns/lan"}) {
		t.Fatalf("view groups %v", z.ViewGroups())
	}
	lan := z.ForGroups([]string{"dns/lan"})
	kids := z.ForGroups([]string{"dns/kids"})
	if lan == z || kids == z || lan == kids || lan.Base() != z || z.Base() != z {
		t.Fatal("variants not selected")
	}
	for _, other := range [][]string{nil, {}, {"dns/guests"}, {"other/lan"}} {
		if z.ForGroups(other) != z {
			t.Fatalf("groups %v must get the default zone", other)
		}
	}
	// A client in both groups gets the view of the group that sorts first.
	if z.ForGroups([]string{"dns/kids", "dns/lan"}) != kids {
		t.Fatal("first group view must win")
	}
	for _, tc := range []struct {
		name  string
		zone  *Zone
		qname string
		qtype uint16
		kind  Kind
		want  []string
	}{
		{"default www", z, "www.example.org.", dns.TypeA, Success, []string{"203.0.113.10"}},
		{"lan www", lan, "www.example.org.", dns.TypeA, Success, []string{"10.0.0.10"}},
		{"kids www", kids, "www.example.org.", dns.TypeA, Success, []string{"10.9.0.10"}},
		// Types the view does not define are inherited.
		{"lan www txt", lan, "www.example.org.", dns.TypeTXT, Success, nil},
		// View-only names do not exist for other clients.
		{"default nas", z, "nas.example.org.", dns.TypeA, NXDomain, nil},
		{"kids nas", kids, "nas.example.org.", dns.TypeA, NXDomain, nil},
		{"lan nas", lan, "nas.example.org.", dns.TypeA, Success, []string{"10.0.0.20"}},
		{"lan nas nodata", lan, "nas.example.org.", dns.TypeAAAA, NoData, nil},
		// Empty non-terminals of a view exist only in that view.
		{"default ent", z, "only.example.org.", dns.TypeA, NXDomain, nil},
		{"lan ent", lan, "only.example.org.", dns.TypeA, NoData, nil},
		{"lan deep", lan, "deep.only.example.org.", dns.TypeAAAA, Success, []string{"fd00::1"}},
		// A view CNAME replaces every default RRset at its owner.
		{"default app", z, "app.example.org.", dns.TypeA, Success, []string{"203.0.113.11"}},
		{"lan app", lan, "app.example.org.", dns.TypeA, Success, []string{"CNAME nas.example.org.", "10.0.0.20"}},
		{"lan app mx", lan, "app.example.org.", dns.TypeMX, NoData, []string{"CNAME nas.example.org."}},
		// A view RRset replaces a default CNAME at its owner.
		{"default alias", z, "alias.example.org.", dns.TypeA, Success, []string{"CNAME www.example.org.", "203.0.113.10"}},
		{"kids alias", kids, "alias.example.org.", dns.TypeA, Success, []string{"10.0.0.30"}},
		{"lan alias", lan, "alias.example.org.", dns.TypeA, Success, []string{"CNAME www.example.org.", "10.0.0.10"}},
		// Publish-only data is never served, in any view.
		{"default tunnel", z, "tunnel.example.org.", dns.TypeCNAME, NXDomain, nil},
		{"lan tunnel", lan, "tunnel.example.org.", dns.TypeCNAME, NXDomain, nil},
	} {
		res := tc.zone.Lookup(tc.qname, tc.qtype)
		if res.Kind != tc.kind {
			t.Errorf("%s: kind %v, want %v (%v)", tc.name, res.Kind, tc.kind, res.Answer)
			continue
		}
		if tc.want != nil && !slices.Equal(addrs(res), tc.want) {
			t.Errorf("%s: answer %v, want %v", tc.name, addrs(res), tc.want)
		}
		if (tc.kind == NXDomain || tc.kind == NoData) && (len(res.Ns) != 1 || res.Ns[0].Header().Rrtype != dns.TypeSOA) {
			t.Errorf("%s: negative answer without the SOA: %v", tc.name, res.Ns)
		}
	}
}

// Transfers and exports use RRsets: the default data only, so a secondary
// or an export never receives group-only data.
func TestClientGroupViewsStayOutOfTransfersAndExport(t *testing.T) {
	z, _ := viewZone(t)
	plain, errs := Build(dnsZone("example.org."), []*v1alpha1.DNSRecordSet{recordSet("dns", "split",
		ep("www.example.org", "A", "203.0.113.10"),
		ep("www.example.org", "TXT", "public"),
		ep("app.example.org", "A", "203.0.113.11"),
		ep("app.example.org", "MX", "10 mail.example.org"),
		ep("alias.example.org", "CNAME", "www.example.org"),
	)}, BuildOptions{Now: testNow})
	if !errs.Empty() {
		t.Fatal(errs.RecordSets)
	}
	var got, want []string
	z.RRsets(func(s *RRset) bool {
		for _, rr := range s.RRs {
			got = append(got, rr.String())
		}
		return true
	})
	plain.RRsets(func(s *RRset) bool {
		for _, rr := range s.RRs {
			want = append(want, rr.String())
		}
		return true
	})
	if !slices.Equal(got, want) || z.RecordCount() != plain.RecordCount() {
		t.Fatalf("transfer data with views:\n%v\nwant\n%v", got, want)
	}
	exported := string(export(t, z))
	for _, private := range []string{"10.0.0.", "10.9.0.", "fd00::1", "nas.example.org", "tunnel"} {
		if strings.Contains(exported, private) {
			t.Fatalf("export leaked %q:\n%s", private, exported)
		}
	}
	// The hash covers the views, so a view change advances the serial.
	if z.ContentHash() == plain.ContentHash() || contentHash(z) != plain.ContentHash() {
		t.Fatal("views must change the content hash, and only through the view part")
	}
	var viewData []string
	z.ViewRRsets(func(group string, s *RRset) bool {
		viewData = append(viewData, group+" "+s.Name+" "+s.TypeString())
		return true
	})
	if !slices.Equal(viewData, []string{
		"dns/kids alias.example.org. A", "dns/kids www.example.org. A",
		"dns/lan app.example.org. CNAME", "dns/lan nas.example.org. A", "dns/lan deep.only.example.org. AAAA", "dns/lan www.example.org. A",
	}) {
		t.Fatalf("view RRsets %v", viewData)
	}
}

func TestClientGroupViewRefusals(t *testing.T) {
	signed := func(dz *v1alpha1.DNSZone) { dz.Spec.DNSSEC = &v1alpha1.DNSSECSpec{Enabled: true} }
	for _, tc := range []struct {
		name   string
		zone   *v1alpha1.DNSZone
		eps    []v1alpha1.Endpoint
		reason string
	}{
		{"ns", dnsZone("example.org."), []v1alpha1.Endpoint{inView(ep("sub.example.org", "NS", "ns.example.net"), "lan")}, "NS or DNAME"},
		{"dname", dnsZone("example.org."), []v1alpha1.Endpoint{inView(ep("d.example.org", "DNAME", "example.net"), "lan")}, "NS or DNAME"},
		{"apex cname", dnsZone("example.org."), []v1alpha1.Endpoint{inView(ep("@", "CNAME", "example.net"), "lan")}, "apex"},
		{"bad name", dnsZone("example.org."), []v1alpha1.Endpoint{inView(ep("x.example.org", "A", "10.0.0.1"), "Not_A_Group")}, "ClientGroup name"},
		{"cname and data", dnsZone("example.org."), []v1alpha1.Endpoint{inView(ep("x.example.org", "A", "10.0.0.1"), "lan"), inView(ep("x.example.org", "CNAME", "y.example.org"), "lan")}, "CNAME cannot coexist"},
		{"below delegation", dnsZone("example.org."), []v1alpha1.Endpoint{ep("sub.example.org", "NS", "ns.example.net"), inView(ep("host.sub.example.org", "A", "10.0.0.1"), "lan")}, v1alpha1.ReasonOccluded},
		{"at delegation", dnsZone("example.org."), []v1alpha1.Endpoint{ep("sub.example.org", "NS", "ns.example.net"), inView(ep("sub.example.org", "TXT", "x"), "lan")}, v1alpha1.ReasonOccluded},
		{"below dname", dnsZone("example.org."), []v1alpha1.Endpoint{ep("d.example.org", "DNAME", "example.net"), inView(ep("h.d.example.org", "A", "10.0.0.1"), "lan")}, v1alpha1.ReasonOccluded},
		{"signed", dnsZone("example.org.", signed), []v1alpha1.Endpoint{ep("www.example.org", "A", "192.0.2.1"), inView(ep("www.example.org", "A", "10.0.0.1"), "lan")}, "DNSSEC-signed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			z, errs := Build(tc.zone, []*v1alpha1.DNSRecordSet{recordSet("dns", "rs", tc.eps...)}, BuildOptions{Now: testNow})
			if z == nil {
				t.Fatal(errs.Zone)
			}
			list := errs.RecordSets["dns/rs"]
			if len(list) != 1 || !strings.Contains(list[0].Error(), tc.reason) {
				t.Fatalf("errors %v, want one with %q", list, tc.reason)
			}
			if tc.name != "cname and data" && z.ForGroups([]string{"dns/lan"}).Lookup("x.example.org.", dns.TypeA).Kind == Success {
				t.Fatal("refused view data served")
			}
		})
	}
	// The view limit refuses later groups, not earlier ones.
	var eps []v1alpha1.Endpoint
	for i := range MaxViews + 1 {
		eps = append(eps, inView(ep("x.example.org", "A", "10.0.0.1"), "g"+strings.Repeat("x", i)))
	}
	z, errs := Build(dnsZone("example.org."), []*v1alpha1.DNSRecordSet{recordSet("dns", "rs", eps...)}, BuildOptions{Now: testNow})
	if list := errs.RecordSets["dns/rs"]; len(list) != 1 || !strings.Contains(list[0].Error(), "at most") || len(z.ViewGroups()) != MaxViews {
		t.Fatalf("view limit: %v %v", list, z.ViewGroups())
	}
	// The same view identity from a later record set is Duplicated; a
	// default endpoint of that identity is not.
	z, errs = Build(dnsZone("example.org."), []*v1alpha1.DNSRecordSet{
		recordSet("dns", "a", inView(ep("x.example.org", "A", "10.0.0.1"), "lan")),
		recordSet("dns", "b", inView(ep("x.example.org", "A", "10.0.0.2"), "lan"), ep("x.example.org", "A", "192.0.2.1")),
	}, BuildOptions{Now: testNow})
	if list := errs.RecordSets["dns/b"]; len(list) != 1 || list[0].Type != field.ErrorTypeDuplicate || ErrorReason(list[0]) != v1alpha1.ReasonDuplicated {
		t.Fatalf("duplicate view identity: %v", list)
	}
	if got := addrs(z.ForGroups([]string{"dns/lan"}).Lookup("x.example.org.", dns.TypeA)); !slices.Equal(got, []string{"10.0.0.1"}) {
		t.Fatalf("earlier view endpoint must win: %v", got)
	}
	if got := addrs(z.Lookup("x.example.org.", dns.TypeA)); !slices.Equal(got, []string{"192.0.2.1"}) {
		t.Fatalf("default endpoint: %v", got)
	}
}

// Disabled and expired view endpoints are not served and still hold their
// view identity.
func TestClientGroupViewInactiveEndpoints(t *testing.T) {
	disabled := inView(ep("x.example.org", "A", "10.0.0.1"), "lan")
	disabled.Kudurru.Disabled = true
	z, errs := Build(dnsZone("example.org."), []*v1alpha1.DNSRecordSet{
		recordSet("dns", "a", disabled),
		recordSet("dns", "b", inView(ep("x.example.org", "A", "10.0.0.2"), "lan")),
	}, BuildOptions{Now: testNow})
	if z.HasViews() || len(errs.RecordSets["dns/b"]) != 1 {
		t.Fatalf("disabled view endpoint: views %v errors %v", z.ViewGroups(), errs.RecordSets)
	}
}

// Selecting a variant is on the query path: it must not allocate, with or
// without views.
func TestForGroupsAllocation(t *testing.T) {
	z, _ := viewZone(t)
	plain := mustBuild(t, dnsZone("example.org."), ep("www.example.org", "A", "192.0.2.1"))
	groups := []string{"dns/a", "dns/lan", "dns/z"}
	var got *Zone
	if n := testing.AllocsPerRun(1000, func() { got = plain.ForGroups(groups) }); n != 0 || got != plain {
		t.Fatalf("no-view selection allocates %g", n)
	}
	if n := testing.AllocsPerRun(1000, func() { got = z.ForGroups(groups) }); n != 0 || got.Base() != z || got == z {
		t.Fatalf("view selection allocates %g", n)
	}
}

func TestCommittedPrimaryViews(t *testing.T) {
	dz := dnsZone("example.org.")
	records := rrs(t,
		"example.org. 300 IN SOA ns1.example.org. hostmaster.example.org. 7 3600 900 1209600 300",
		"example.org. 300 IN NS ns1.example.org.",
		"example.org. 300 IN NS ns2.example.org.",
		"www.example.org. 300 IN A 203.0.113.10",
	)
	views := []CommittedView{
		{Group: "dns/kids", Records: rrs(t, "www.example.org. 60 IN A 10.9.0.10")},
		{Group: "dns/lan", Records: rrs(t, "www.example.org. 60 IN A 10.0.0.10", "nas.example.org. 60 IN A 10.0.0.20")},
	}
	z, err := BuildCommittedPrimaryWithViews(dz, records, views, 7, time.Time{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := addrs(z.ForGroups([]string{"dns/lan"}).Lookup("nas.example.org.", dns.TypeA)); !slices.Equal(got, []string{"10.0.0.20"}) {
		t.Fatalf("committed lan view %v", got)
	}
	if z.Lookup("nas.example.org.", dns.TypeA).Kind != NXDomain {
		t.Fatal("committed view leaked into default data")
	}
	if got := addrs(z.ForGroups([]string{"dns/kids"}).Lookup("www.example.org.", dns.TypeA)); !slices.Equal(got, []string{"10.9.0.10"}) {
		t.Fatalf("committed kids view %v", got)
	}
	plain, err := BuildCommittedPrimary(dz, records, 7, time.Time{}, false)
	if err != nil || plain.HasViews() || plain.ContentHash() == z.ContentHash() {
		t.Fatal("committed views must count in the hash")
	}
	for name, bad := range map[string][]CommittedView{
		"ns":       {{Group: "dns/lan", Records: rrs(t, "sub.example.org. 60 IN NS ns.example.net.")}},
		"unsorted": {views[1], views[0]},
		"empty":    {{Group: "dns/lan"}},
		"outside":  {{Group: "dns/lan", Records: rrs(t, "www.example.net. 60 IN A 10.0.0.1")}},
		"cname":    {{Group: "dns/lan", Records: rrs(t, "x.example.org. 60 IN CNAME a.example.org.", "x.example.org. 60 IN A 10.0.0.1")}},
	} {
		if _, err := BuildCommittedPrimaryWithViews(dz, records, bad, 7, testNow, false); err == nil {
			t.Errorf("%s: committed view accepted", name)
		}
	}
	signed := append(slices.Clone(records), rr(t, "www.example.org. 300 IN RRSIG A 13 3 300 20270101000000 20260101000000 1234 example.org. AQID"))
	if _, err := BuildCommittedPrimaryWithViews(dz, signed, views, 7, testNow, false); err == nil {
		t.Error("views accepted in a signed committed zone")
	}
}
