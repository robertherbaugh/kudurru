package primaryzone

import (
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

func TestCommittedFallbackPreservesApexOwnershipAndRoutingSerial(t *testing.T) {
	for _, typ := range []string{"", "A", "TXT"} {
		t.Run("apex-"+typ, func(t *testing.T) {
			z := desiredZone()
			z.Spec.Fallback = &public.ZoneFallbackSpec{UpstreamGroup: "public-a"}
			z.Spec.Policy.ZoneTransfer = public.AccessAllow
			z.Spec.Policy.Notify = public.NotifySpecified
			z.Spec.Policy.NotifyTargets = []string{"192.0.2.53"}
			in := &zonestate.DesiredInputs{Zone: z, Complete: true}
			if typ != "" {
				target := "192.0.2.9"
				if typ == "TXT" {
					target = "explicit apex"
				}
				in.RecordSets = []*public.DNSRecordSet{desiredSet("apex", public.Endpoint{DNSName: "example.", RecordType: typ, Targets: []string{target}, RecordTTL: 300})}
			}
			c := NewCompiler(CompilerOptions{})
			v := compile(t, c, blank(z), in, fixedNow)
			model := built(t, v)
			want := zone.Fallback
			if typ != "" {
				want = zone.NoData
			}
			if model.Lookup("example.", dns.TypeAAAA).Kind != want || model.Lookup("missing.example.", dns.TypeA).Kind != zone.Fallback || model.Lookup("example.", dns.TypeSOA).Kind != zone.Success {
				t.Fatal("committed fallback changed apex ownership or local SOA")
			}
			if typ == "A" && model.Lookup("example.", dns.TypeA).Kind != zone.Success {
				t.Fatal("explicit apex record fell through to public routing")
			}
			if model.NotifyMode() != public.NotifyNone || model.TransferAllowed(netip.MustParseAddr("192.0.2.8"), "") {
				t.Fatal("partial zone allowed NOTIFY or transfer")
			}
			// Config-only routing changes must invalidate serial-based edge caches.
			z.Spec.Fallback.UpstreamGroup = "public-b"
			z.ResourceVersion = "route-b"
			next := compile(t, c, v, in, fixedNow.Add(time.Second))
			if next.Head.Spec.Serial == v.Head.Spec.Serial || next.Head.Spec.ContentHash == v.Head.Spec.ContentHash || built(t, next).FallbackGroup() != "public-b" || model.FallbackGroup() != "public-a" {
				t.Fatal("routing change did not advance serial or altered held model")
			}
			z.ResourceVersion = "equivalent-route"
			same := compile(t, c, next, in, fixedNow.Add(2*time.Second))
			if same.Head.Spec.Serial != next.Head.Spec.Serial {
				t.Fatal("equivalent routing advanced serial")
			}
			z.Spec.Fallback = nil
			z.ResourceVersion = "no-fallback"
			closed := compile(t, c, same, in, fixedNow.Add(3*time.Second))
			if closed.Head.Spec.Serial == same.Head.Spec.Serial || built(t, closed).Lookup("missing.example.", dns.TypeA).Kind != zone.NXDomain {
				t.Fatal("removing fallback did not restore closed authority")
			}
		})
	}
}
