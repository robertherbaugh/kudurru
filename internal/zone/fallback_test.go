package zone

import (
	"strings"
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
	dz.Spec.Fallback = nil
	if strict.ContentHash() != mustBuild(t, dz).ContentHash() {
		t.Fatal("removing fallback must restore the strict zone's content hash")
	}
	for _, name := range []string{"www.example.org.", "example.org.", "EXAMPLE.ORG."} {
		for _, typ := range []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeMX, dns.TypeTXT} {
			r := overlay.Lookup(name, typ)
			if r.Kind != Fallback || r.Target != name || r.Positive() || r.Authoritative() || len(r.Ns) != 0 {
				t.Fatalf("%s/%d: %+v", name, typ, r)
			}
		}
	}
	dz.Spec.Fallback = &v1alpha1.ZoneFallbackSpec{}
	localApex := mustBuild(t, dz, ep("@", "A", "10.0.0.10"))
	if r := localApex.Lookup("example.org.", dns.TypeAAAA); r.Kind != NoData {
		t.Fatalf("an explicit apex record suppresses missing-type fallback: %+v", r)
	}
}

func TestFallbackLookupKinds(t *testing.T) {
	dz := dnsZone("example.org.")
	dz.Spec.Fallback = &v1alpha1.ZoneFallbackSpec{}
	z := mustBuild(t, dz,
		ep("auth.example.org", "A", "10.0.0.10"),
		ep("child.example.org", "NS", "ns.external."),
		ep("alias.example.org", "CNAME", "www.external."),
		ep("loop.example.org", "CNAME", "loop.example.org."),
		ep("overflow.example.org", "DNAME", strings.Repeat(strings.Repeat("a", 60)+".", 4)),
	)
	for _, tc := range []struct {
		name string
		typ  uint16
		kind Kind
	}{
		{"auth.example.org.", dns.TypeA, Success},
		{"auth.example.org.", dns.TypeAAAA, NoData},
		{"child.example.org.", dns.TypeA, Delegation},
		{"alias.example.org.", dns.TypeA, CNAME},
		{"loop.example.org.", dns.TypeA, Loop},
		{strings.Repeat("a", 20) + ".overflow.example.org.", dns.TypeA, YXDomain},
		{"missing.example.org.", dns.TypeA, Fallback},
	} {
		t.Run(tc.kind.String(), func(t *testing.T) {
			if got := z.Lookup(tc.name, tc.typ); got.Kind != tc.kind {
				t.Fatalf("%s/%d: got %s, want %s", tc.name, tc.typ, got.Kind, tc.kind)
			}
		})
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
