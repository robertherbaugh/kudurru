package inbound

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func sourceDocument(t *testing.T) (Document, *public.ClientGroup, time.Time) {
	t.Helper()
	at := time.Unix(100000, 0).UTC()
	owner := allocationInput().Owner
	source := host.SourceID(owner)
	group := &public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "lan-clients", Namespace: owner.Namespace, UID: "group-original", ResourceVersion: "captured"}, Spec: public.ClientGroupSpec{Sources: &public.ClientGroupSources{IntegrationRef: &public.ObjectRef{Name: owner.Name}}, Clients: []public.ClientMatcher{{MAC: "02:00:00:00:00:01", Comment: "manual overlap"}, {CIDR: "10.0.0.0/8", Comment: "manual subnet"}}}}
	gd, _ := IntentDigest(group.Spec)
	ge := ObjectEvidence{Kind: "ClientGroup", Namespace: group.Namespace, Name: group.Name, UID: string(group.UID), ResourceVersion: group.ResourceVersion, IntentDigest: gd}
	zone := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "lan", UID: "zone-original", ResourceVersion: "captured"}, Spec: public.DNSZoneSpec{ZoneName: "lan.test.", Type: public.ZoneTypePrimary}}
	zd, _ := IntentDigest(zone.Spec)
	ze := ObjectEvidence{Kind: "DNSZone", Name: zone.Name, UID: string(zone.UID), ResourceVersion: zone.ResourceVersion, IntentDigest: zd}
	rr := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "phone", Namespace: owner.Namespace}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "lan"}, Endpoints: []public.Endpoint{{DNSName: "phone.lan.test.", RecordType: "A", Targets: []string{"192.0.2.1"}, RecordTTL: 120, Kudurru: &public.EndpointExt{ExpiresAt: &metav1.Time{Time: at.Add(time.Hour)}}}}}}
	rd, _ := IntentDigest(rr.Spec)
	re := ObjectEvidence{Kind: "DNSRecordSet", Namespace: rr.Namespace, Name: rr.Name, UID: "record-original", ResourceVersion: "captured", IntentDigest: rd}
	rawGroup := group.DeepCopy()
	rawGroup.Spec.Clients = []public.ClientMatcher{{MAC: "02:00:00:00:00:01"}, {MAC: "02:00:00:00:00:02"}}
	doc := Document{FormatVersion: kinds.FormatVersion, Owner: owner, Snapshot: host.SourceSnapshot{Complete: true, Clients: []inventory.Client{
		{Source: source, ID: "a", IPs: []netip.Addr{netip.MustParseAddr("192.0.2.1")}, MACs: []string{"02:00:00:00:00:01"}, ObservedAt: at, ExpiresAt: at.Add(time.Hour)},
		{Source: source, ID: "b", IPs: []netip.Addr{netip.MustParseAddr("192.0.2.2")}, MACs: []string{"02:00:00:00:00:02"}, ObservedAt: at, ExpiresAt: at.Add(time.Hour)},
	}, Desired: &host.DesiredBatch{SourceID: source, RecordSets: []*public.DNSRecordSet{rr}, ClientGroups: []*public.ClientGroup{rawGroup}}}, Targets: []TargetEvidence{{Object: ze, Apex: "lan.test."}}, Objects: []ObjectEvidence{re}, Groups: []GroupContribution{{Group: ge, MACs: []string{"02:00:00:00:00:01", "02:00:00:00:00:02"}}}}
	scope := kinds.AllocationScope{ClusterID: owner.ClusterID, ZoneUID: ze.UID, Apex: zone.Spec.ZoneName}
	spec := kinds.IntegrationNameAllocationSpec{FormatVersion: 1, Scope: scope, Label: "phone", Base: "phone", Owner: owner, ClientID: "a", CreatedAt: at}
	digest, _ := IntentDigest(spec)
	doc.Allocations = []AllocationEvidence{{Object: ObjectEvidence{Kind: kinds.KindIntegrationNameAllocation, Name: kinds.NameAllocationName(scope, "phone"), UID: "allocation-original", ResourceVersion: "captured", IntentDigest: digest}, Spec: spec}}
	return doc, group, at
}
func encodeDocument(t *testing.T, d Document, at time.Time) SourceImage {
	t.Helper()
	image, err := EncodeSource(d, 1, kinds.Digest([]byte("config")), kinds.Digest([]byte("credential")), at)
	if err != nil {
		t.Fatal(err)
	}
	return image
}
func cloneDocument(t *testing.T, d Document) Document {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var out Document
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestSourceCodecCompleteOriginalRoundTripAndIsolation(t *testing.T) {
	d, _, at := sourceDocument(t)
	image := encodeDocument(t, d, at)
	before, _ := json.Marshal(d)
	decoded, err := DecodeSource(image)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(decoded)
	if !bytes.Equal(before, after) {
		t.Fatal("roundtrip changed original desired/expiry/evidence")
	}
	d.Snapshot.Clients[0].MACs[0] = "changed"
	decoded.Snapshot.Clients[0].IPs[0] = netip.Addr{}
	if _, err := DecodeSource(image); err != nil {
		t.Fatal("input/output aliasing corrupted chunks", err)
	}
	original, err := DecodeSource(image)
	if err != nil {
		t.Fatal(err)
	}
	if !original.Snapshot.Desired.RecordSets[0].Spec.Endpoints[0].Kudurru.ExpiresAt.Time.Equal(at.Add(time.Hour)) {
		t.Fatal("expiry resampled")
	}
}
func TestSourceCodecBoundedMultipleChunksAndTamper(t *testing.T) {
	d, _, at := sourceDocument(t)
	d.Snapshot.Desired = nil
	d.Targets = nil
	d.Objects = nil
	d.Groups = nil
	d.Allocations = nil
	for i := 0; i < 500; i++ {
		d.Snapshot.Clients = append(d.Snapshot.Clients, inventory.Client{Source: host.SourceID(d.Owner), ID: fmt.Sprintf("long-%d", i), Tags: []string{strings.Repeat("a", 128), strings.Repeat("b", 128), strings.Repeat("c", 128)}, ObservedAt: at, ExpiresAt: at.Add(time.Hour)})
	}
	image := encodeDocument(t, d, at)
	if len(image.Chunks) < 2 {
		t.Fatal("not multichunk")
	}
	reordered := SourceImage{Head: image.Head, Chunks: append([]*kinds.IntegrationSourceChunk{}, image.Chunks...)}
	reordered.Chunks[0], reordered.Chunks[1] = reordered.Chunks[1], reordered.Chunks[0]
	if _, err := DecodeSource(reordered); err != nil {
		t.Fatal("carrier order should not affect ordered references", err)
	}
	for _, mode := range []string{"missing", "extra", "duplicate", "bytes", "mixed", "root", "index"} {
		t.Run(mode, func(t *testing.T) {
			bad := SourceImage{Head: image.Head.DeepCopy()}
			for _, c := range image.Chunks {
				bad.Chunks = append(bad.Chunks, c.DeepCopy())
			}
			switch mode {
			case "missing":
				bad.Chunks = bad.Chunks[1:]
			case "extra":
				bad.Chunks = append(bad.Chunks, bad.Chunks[0])
			case "duplicate":
				bad.Chunks[1] = bad.Chunks[0]
			case "bytes":
				bad.Chunks[0].Spec.Data[0] = '!'
			case "mixed":
				bad.Chunks[0].Spec.Owner.UID = "recreated"
			case "root":
				bad.Head.Spec.RootDigest = kinds.Digest([]byte("other"))
			case "index":
				bad.Chunks[0].Spec.Index = 3
			}
			if _, err := DecodeSource(bad); err == nil {
				t.Fatal("accepted incomplete/tampered image")
			}
		})
	}
}
func TestDocumentRejectsInvalidOriginalObservationAndEvidence(t *testing.T) {
	original, _, at := sourceDocument(t)
	for name, mutate := range map[string]func(*Document){
		"incomplete": func(d *Document) { d.Snapshot.Complete = false }, "source": func(d *Document) { d.Snapshot.Clients[0].Source = "foreign" },
		"duplicate": func(d *Document) { d.Snapshot.Clients = append(d.Snapshot.Clients, d.Snapshot.Clients[0]) }, "future": func(d *Document) { d.Snapshot.Clients[0].ObservedAt = at.Add(time.Minute) },
		"target UID": func(d *Document) { d.Targets[0].Object.UID = "" }, "record intent": func(d *Document) { d.Objects[0].IntentDigest = kinds.Digest([]byte("different")) },
		"foreign group": func(d *Document) { d.Groups[0].MACs[0] = "02:00:00:00:00:ff" }, "ip group": func(d *Document) { d.Snapshot.Desired.ClientGroups[0].Spec.Clients[0].IP = "192.0.2.1" },
		"allocation missing": func(d *Document) { d.Allocations = nil },
		"allocation UID":     func(d *Document) { d.Allocations[0].Object.UID = "" },
		"bound":              func(d *Document) { d.Snapshot.Clients = make([]inventory.Client, kinds.MaxSourceClients+1) },
	} {
		t.Run(name, func(t *testing.T) {
			d := cloneDocument(t, original)
			mutate(&d)
			if ValidateDocument(d, at) == nil {
				t.Fatal("accepted malformed original")
			}
		})
	}
	for _, mode := range []string{"out-of-zone", "nonfinite", "pseudo", "foreign namespace", "invalid target"} {
		t.Run(mode, func(t *testing.T) {
			d := cloneDocument(t, original)
			rr := d.Snapshot.Desired.RecordSets[0]
			e := &rr.Spec.Endpoints[0]
			switch mode {
			case "out-of-zone":
				e.DNSName = "foreign.test."
			case "nonfinite":
				e.Kudurru.ExpiresAt = nil
			case "pseudo":
				e.RecordType = "OPT"
			case "foreign namespace":
				rr.Namespace = "foreign"
			case "invalid target":
				e.Targets[0] = "not-an-address"
			}
			d.Objects[0].IntentDigest, _ = IntentDigest(rr.Spec)
			if ValidateDocument(d, at) == nil {
				t.Fatal("accepted invalid endpoint")
			}
		})
	}
}

func TestSourceCodecRejectsAliasCoexistenceAndForeignAllocation(t *testing.T) {
	d, _, at := sourceDocument(t)
	rr := d.Snapshot.Desired.RecordSets[0]
	alias := *rr.Spec.Endpoints[0].DeepCopy()
	alias.RecordType = "CNAME"
	alias.Targets = []string{"elsewhere.test."}
	rr.Spec.Endpoints = append(rr.Spec.Endpoints, alias)
	d.Objects[0].IntentDigest, _ = IntentDigest(rr.Spec)
	if ValidateDocument(d, at) == nil {
		t.Fatal("CNAME coexistence accepted")
	}
	d, _, at = sourceDocument(t)
	d.Allocations[0].Spec.Owner.UID = "foreign"
	d.Allocations[0].Object.IntentDigest, _ = IntentDigest(d.Allocations[0].Spec)
	if ValidateDocument(d, at) == nil {
		t.Fatal("foreign allocation UID adopted")
	}
	d, _, at = sourceDocument(t)
	d.Snapshot.Desired.RecordSets[0].Spec.Endpoints[0].Kudurru.ExpiresAt.Time = at.Add(2 * time.Hour)
	d.Objects[0].IntentDigest, _ = IntentDigest(d.Snapshot.Desired.RecordSets[0].Spec)
	if ValidateDocument(d, at) == nil {
		t.Fatal("extended DNS observation expiry")
	}
}
