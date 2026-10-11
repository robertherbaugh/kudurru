package zone

import (
	"slices"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// TestLookupFlatMatchesWalk checks the single-search fast path for zones
// without cuts against the label-by-label walk on every owner name, a
// child, a grandchild and an upper-cased spelling of each, for several
// query types.
func TestLookupFlatMatchesWalk(t *testing.T) {
	var eps []v1alpha1.Endpoint
	for _, e := range exampleEndpoints() {
		if e.RecordType != "NS" && e.RecordType != "DNAME" && !strings.HasSuffix(e.DNSName, "sub.example.org") {
			eps = append(eps, e)
		}
	}
	flat := mustBuild(t, dnsZone("example.org."), eps...)
	if flat.cuts {
		t.Fatal("fixture must have no cuts")
	}
	walked := *flat
	walked.cuts = true
	var names []string
	flat.tree.walk(func(n *node) bool {
		names = append(names, n.name, "x."+n.name, "a.b."+n.name, strings.ToUpper(n.name))
		return true
	})
	if len(names) < 100 {
		t.Fatalf("only %d names", len(names))
	}
	for _, q := range names {
		for _, qt := range []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeTXT, dns.TypeMX, dns.TypeCNAME, dns.TypeANY, dns.TypeSOA} {
			a, b := flat.Lookup(q, qt), walked.Lookup(q, qt)
			if a.Kind != b.Kind || a.Wildcard != b.Wildcard || a.Target != b.Target ||
				!slices.Equal(sectionStrings(a.Answer), sectionStrings(b.Answer)) ||
				!slices.Equal(sectionStrings(a.Ns), sectionStrings(b.Ns)) ||
				!slices.Equal(sectionStrings(a.Extra), sectionStrings(b.Extra)) {
				t.Errorf("%s/%s: flat %+v, walk %+v", q, dns.TypeToString[qt], a, b)
			}
		}
	}
	if !exampleZone(t).cuts {
		t.Fatal("the example zone has a delegation and DNAMEs")
	}
}
