package inbound

import (
	"net/netip"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/unifi"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Exercise the committed provider's real pure translation seam, including empty
// reverse coverage, rather than assuming a parallel source DTO interpretation.
func TestUniFiTranslationSourceCodec(t *testing.T) {
	at := time.Unix(100000, 0).UTC()
	owner := allocationInput().Owner
	zone := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "lan", UID: "zone-original", ResourceVersion: "captured"}, Spec: public.DNSZoneSpec{ZoneName: "lan.test.", Type: public.ZoneTypePrimary}}
	input := unifi.TranslationInput{Owner: owner, Zone: zone, Config: public.UniFiIntegration{Endpoint: "https://controller.test", Inventory: &public.UniFiInventory{ZoneRef: public.ObjectRef{Name: "lan"}, TTL: 120}}, Observation: unifi.Observation{SiteID: "00000000-0000-4000-8000-000000000001", ObservedAt: at, Complete: true, Networks: []unifi.Network{{ID: "00000000-0000-4000-8000-000000000002", Name: "LAN", VLAN: 10, Enabled: true, Prefixes: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/23")}}}, Clients: []unifi.Device{{ID: "00000000-0000-4000-8000-000000000001/02:00:00:00:00:01", MAC: "02:00:00:00:00:01", Name: "Phone", IPs: []netip.Addr{netip.MustParseAddr("192.0.2.3")}, ObservedAt: at, ExpiresAt: at.Add(3 * time.Minute)}}}}
	requests, err := unifi.NameRequests(input)
	if err != nil {
		t.Fatal(err)
	}
	allocationIn := allocationInput()
	for _, r := range requests {
		allocationIn.Requests = append(allocationIn.Requests, NameRequest{ID: r.ID, Base: r.Base})
	}
	allocated, err := AllocateNames(allocationIn)
	if err != nil {
		t.Fatal(err)
	}
	input.Allocations = allocated.Labels
	snapshot, _, err := unifi.Translate(input)
	if err != nil {
		t.Fatal(err)
	}
	doc := Document{FormatVersion: 1, Owner: owner, Snapshot: snapshot}
	token := func(kind, ns, name string, spec any) ObjectEvidence {
		digest, err := IntentDigest(spec)
		if err != nil {
			t.Fatal(err)
		}
		return ObjectEvidence{Kind: kind, Namespace: ns, Name: name, UID: "actual-" + name, ResourceVersion: "admitted", IntentDigest: digest}
	}
	forward := token("DNSZone", "", zone.Name, zone.Spec)
	forward.UID = string(zone.UID)
	forward.ResourceVersion = zone.ResourceVersion
	doc.Targets = append(doc.Targets, TargetEvidence{Object: forward, Apex: zone.Spec.ZoneName})
	for _, z := range snapshot.Desired.Zones {
		o := token("DNSZone", "", z.Name, z.Spec)
		doc.Objects = append(doc.Objects, o)
		doc.Targets = append(doc.Targets, TargetEvidence{Object: o, Apex: z.Spec.ZoneName})
	}
	for _, r := range snapshot.Desired.RecordSets {
		doc.Objects = append(doc.Objects, token("DNSRecordSet", r.Namespace, r.Name, r.Spec))
	}
	for _, allocation := range persisted(allocated.New) {
		o := token(kinds.KindIntegrationNameAllocation, "", allocation.Name, allocation.Spec)
		o.UID = string(allocation.UID)
		o.ResourceVersion = allocation.ResourceVersion
		doc.Allocations = append(doc.Allocations, AllocationEvidence{Object: o, Spec: allocation.Spec})
	}
	image, err := EncodeSource(doc, 1, kinds.Digest([]byte("config")), kinds.Digest([]byte("credential")), at)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := DecodeSource(image)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Snapshot.Desired.SourceID != host.SourceID(owner) || len(restored.Snapshot.Desired.Zones) != 2 || len(restored.Snapshot.Desired.RecordSets) != 2 {
		t.Fatal("lost /23 child coverage or forward/PTR desired")
	}
}
