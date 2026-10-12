package zone

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// testNow is the fixed build instant of the test zones.
var testNow = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

// rr parses a record in presentation format or fails the test.
func rr(t testing.TB, s string) dns.RR {
	t.Helper()
	r, err := dns.NewRR(s)
	if err != nil || r == nil {
		t.Fatalf("bad test record %q: %v", s, err)
	}
	return r
}

// rrs parses records.
func rrs(t testing.TB, ss ...string) []dns.RR {
	t.Helper()
	out := make([]dns.RR, 0, len(ss))
	for _, s := range ss {
		out = append(out, rr(t, s))
	}
	return out
}

// ep builds an endpoint.
func ep(name, typ string, targets ...string) v1alpha1.Endpoint {
	return v1alpha1.Endpoint{DNSName: name, RecordType: typ, Targets: targets}
}

func epTTL(name, typ string, ttl int64, targets ...string) v1alpha1.Endpoint {
	e := ep(name, typ, targets...)
	e.RecordTTL = ttl
	return e
}

func dnsZone(name string, mutate ...func(*v1alpha1.DNSZone)) *v1alpha1.DNSZone {
	dz := &v1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: strings.TrimSuffix(name, "."), Generation: 1},
		Spec: v1alpha1.DNSZoneSpec{
			ZoneName:    name,
			Type:        v1alpha1.ZoneTypePrimary,
			Nameservers: []string{childName("ns1", name), childName("ns2", name)},
			DefaultTTL:  300,
			SOA:         &v1alpha1.SOASpec{RName: childName("hostmaster", name)},
		},
		Status: v1alpha1.DNSZoneStatus{Serial: 2026101100},
	}
	for _, m := range mutate {
		m(dz)
	}
	return dz
}

func recordSet(ns, name string, eps ...v1alpha1.Endpoint) *v1alpha1.DNSRecordSet {
	return &v1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: "z"}, Endpoints: eps},
	}
}

// mustBuild builds a zone from one record set and fails on any error.
func mustBuild(t testing.TB, dz *v1alpha1.DNSZone, eps ...v1alpha1.Endpoint) *Zone {
	t.Helper()
	z, errs := Build(dz, []*v1alpha1.DNSRecordSet{recordSet("default", "records", eps...)}, BuildOptions{Now: testNow})
	if !errs.Empty() {
		t.Fatalf("build errors: zone=%v sets=%v", errs.Zone, errs.RecordSets)
	}
	return z
}

// Case is one lookup expectation in the style of CoreDNS plugin/test.Case:
// a question and the exact sections expected back.
type Case struct {
	Name     string
	Qname    string
	Qtype    uint16
	Kind     Kind
	Answer   []string
	Ns       []string
	Extra    []string
	Target   string
	Wildcard bool
}

// sectionStrings renders a section sorted, for order-insensitive compares.
func sectionStrings(rrs []dns.RR) []string {
	out := make([]string, len(rrs))
	for i, r := range rrs {
		out[i] = r.String()
	}
	slices.Sort(out)
	return out
}

// wantStrings normalises expected records through the parser so spacing
// in the test table does not matter.
func wantStrings(t testing.TB, ss []string) []string {
	t.Helper()
	return sectionStrings(rrs(t, ss...))
}

func checkSection(t *testing.T, name string, got []dns.RR, want []string) {
	t.Helper()
	g, w := sectionStrings(got), wantStrings(t, want)
	if !slices.Equal(g, w) {
		t.Errorf("%s section:\n got: %q\nwant: %q", name, g, w)
	}
}

func runCases(t *testing.T, z *Zone, cases []Case) {
	t.Helper()
	for _, tc := range cases {
		name := tc.Name
		if name == "" {
			name = tc.Qname + "/" + dns.TypeToString[tc.Qtype]
		}
		t.Run(name, func(t *testing.T) {
			res := z.Lookup(tc.Qname, tc.Qtype)
			if res.Kind != tc.Kind {
				t.Errorf("kind = %v, want %v (answer %v ns %v)", res.Kind, tc.Kind, res.Answer, res.Ns)
			}
			checkSection(t, "answer", res.Answer, tc.Answer)
			checkSection(t, "authority", res.Ns, tc.Ns)
			checkSection(t, "additional", res.Extra, tc.Extra)
			if res.Target != tc.Target {
				t.Errorf("target = %q, want %q", res.Target, tc.Target)
			}
			if res.Wildcard != tc.Wildcard {
				t.Errorf("wildcard = %v, want %v", res.Wildcard, tc.Wildcard)
			}
		})
	}
}
