package zone

import (
	"testing"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestFallbackLookupAndHash(t *testing.T) {
	dz := dnsZone("example.org.")
	strict := mustBuild(t, dz)
	dz.Spec.Fallback = &v1alpha1.ZoneFallbackSpec{}
	overlay := mustBuild(t, dz)
	if strict.ContentHash() == overlay.ContentHash() {
		t.Fatal("enabling fallback must change the serial's content hash")
	}
	dz.Spec.Fallback.UpstreamGroup = "public"
	routed := mustBuild(t, dz)
	if overlay.ContentHash() == routed.ContentHash() {
		t.Fatal("changing the fallback group must change the content hash")
	}
	for _, name := range []string{"www.example.org.", "example.org.", "EXAMPLE.ORG."} {
		for _, typ := range []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeMX, dns.TypeTXT} {
			r := overlay.Lookup(name, typ)
			if r.Kind != Fallback || r.Target != name || r.Positive() || r.Authoritative() || len(r.Ns) != 0 {
				t.Fatalf("%s/%d: %+v", name, typ, r)
			}
		}
	}
	localApex := mustBuild(t, dz, ep("@", "A", "10.0.0.10"))
	if r := localApex.Lookup("example.org.", dns.TypeAAAA); r.Kind != NoData {
		t.Fatalf("an explicit apex record suppresses missing-type fallback: %+v", r)
	}
}

func TestFallbackOnlyPrimary(t *testing.T) {
	dz := dnsZone("example.org.")
	dz.Spec.Type = v1alpha1.ZoneTypeForward
	dz.Spec.Fallback = &v1alpha1.ZoneFallbackSpec{}
	if b, errs := NewBuilder(dz, BuildOptions{}); b != nil || len(errs) != 1 || errs[0].Field != "spec.fallback" {
		t.Fatalf("forward zone with fallback accepted: %v", errs)
	}
}
