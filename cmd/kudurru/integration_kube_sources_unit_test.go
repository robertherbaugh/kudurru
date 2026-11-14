package main

import (
	"encoding/json"
	"net/netip"
	"reflect"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/inbound"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// kubeSourceImage is the pure eligibility and assembly rule of every
// replica's source runtime (F09b-K5, D384).
func TestKubeSourceImageEligibilityAndCanonicalRebuild(t *testing.T) {
	at := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	const cluster = "cluster"
	integration := func(name, uid string) *public.Integration {
		return &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kudurru", UID: types.UID(uid), ResourceVersion: "5"}, Spec: public.IntegrationSpec{Type: "unifi", UniFi: &public.UniFiIntegration{Endpoint: "https://unifi.example", Groups: public.UniFiGroups{ByNetwork: map[string]string{"LAN": "devices"}}}}}
	}
	group := &public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "devices", Namespace: "kudurru", UID: "group-uid", ResourceVersion: "3"}, Spec: public.ClientGroupSpec{Sources: &public.ClientGroupSources{IntegrationRef: &public.ObjectRef{Name: "first"}}}}
	head := func(d *public.Integration, ip string, accepted time.Time, observed time.Time, groups bool) inbound.SourceImage {
		t.Helper()
		owner := kinds.Owner{ClusterID: cluster, Namespace: d.Namespace, Name: d.Name, UID: string(d.UID)}
		client := inventory.Client{Source: ih.SourceID(owner), ID: "client-" + d.Name, IPs: []netip.Addr{netip.MustParseAddr(ip)}, MACs: []string{"00:11:22:33:44:55"}, ObservedAt: observed, ExpiresAt: observed.Add(30 * time.Minute)}
		doc := inbound.Document{FormatVersion: kinds.FormatVersion, Owner: owner, Snapshot: ih.SourceSnapshot{Complete: true, Clients: []inventory.Client{client}}}
		if groups {
			desired := group.DeepCopy()
			desired.Spec.Clients = []public.ClientMatcher{{MAC: "00:11:22:33:44:55"}}
			doc.Snapshot.Desired = &ih.DesiredBatch{SourceID: ih.SourceID(owner), ClientGroups: []*public.ClientGroup{desired}}
			doc.Groups = []inbound.GroupContribution{{Group: evidence(group, group.Spec), MACs: []string{"00:11:22:33:44:55"}}}
		}
		raw, _ := json.Marshal(d.Spec)
		encoded, err := inbound.EncodeSource(doc, 1, kinds.Digest(raw), kinds.Digest([]byte("credentials")), accepted)
		if err != nil {
			t.Fatal(err)
		}
		encoded.Head.UID, encoded.Head.ResourceVersion = types.UID("head-"+d.Name), "1"
		for i, c := range encoded.Chunks {
			c.UID, c.ResourceVersion = types.UID("chunk-"+d.Name+string(rune('a'+i))), "1"
		}
		return encoded
	}
	listing := func(images ...inbound.SourceImage) *inbound.DecodedImage {
		t.Helper()
		var l inbound.CarrierListing
		l.ClusterID = cluster
		for _, image := range images {
			l.Heads = append(l.Heads, image.Head)
			l.Chunks = append(l.Chunks, image.Chunks...)
		}
		decoded, err := inbound.DecodeCarriers(l)
		if err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	snapshot := func(objects ...store.Object) zonecache.ObjectSet {
		t.Helper()
		snap, err := zonecache.Prepare(zonecache.PrepareOptions{Kinds: []store.Kind{"Integration", "DNSZone", "DNSRecordSet", "ClientGroup"}, Now: at}, objects, nil)
		if err != nil {
			t.Fatal(err)
		}
		return snap.Objects
	}
	first, second := integration("first", "uid-1"), integration("second", "uid-2")
	// Both claim 127.0.0.7; the later observation wins on every replica.
	a := head(first, "127.0.0.7", at.Add(-2*time.Minute), at.Add(-3*time.Minute), true)
	b := head(second, "127.0.0.7", at.Add(-time.Minute), at.Add(-90*time.Second), false)
	ip := netip.MustParseAddr("127.0.0.7")

	t.Run("eligible", func(t *testing.T) {
		inv, roots, grants, err := kubeSourceImage(listing(a), snapshot(first, group), cluster, "kudurru", at)
		if err != nil {
			t.Fatal(err)
		}
		if c := inv.Lookup(ip, at); c == nil || c.ID != "client-first" {
			t.Fatal("eligible source not identified", c)
		}
		if roots[a.Head.Name] != a.Head.Spec.RootDigest || len(grants) != 1 || grants[0].Group != "kudurru/devices" || grants[0].MAC != "00:11:22:33:44:55" || !grants[0].ExpiresAt.Equal(at.Add(-3*time.Minute).Add(30*time.Minute)) {
			t.Fatal("eligible root or grant", roots, grants)
		}
	})
	for name, objects := range map[string][]store.Object{
		"integration absent":      {group},
		"integration re-created":  {integration("first", "uid-other"), group},
		"integration suspended":   {func() *public.Integration { d := first.DeepCopy(); d.Spec.Suspend = true; return d }(), group},
		"integration spec edited": {func() *public.Integration { d := first.DeepCopy(); d.Spec.UniFi.Site = "other"; return d }(), group},
		"integration deleting": {func() *public.Integration {
			d := first.DeepCopy()
			now := metav1.NewTime(at)
			d.DeletionTimestamp = &now
			return d
		}(), group},
		"integration refused": {func() *public.Integration { d := first.DeepCopy(); d.Namespace = "tenant"; return d }(), group},
	} {
		t.Run(name, func(t *testing.T) {
			inv, roots, grants, err := kubeSourceImage(listing(a), snapshot(objects...), cluster, "kudurru", at)
			if err != nil {
				t.Fatal(err)
			}
			if inv.Lookup(ip, at) != nil || len(roots) != 0 || len(grants) != 0 {
				t.Fatal("an ineligible source contributed", roots, grants)
			}
		})
	}
	t.Run("group re-created grants nothing", func(t *testing.T) {
		other := group.DeepCopy()
		other.UID = "group-other"
		inv, _, grants, err := kubeSourceImage(listing(a), snapshot(first, other), cluster, "kudurru", at)
		if err != nil {
			t.Fatal(err)
		}
		if inv.Lookup(ip, at) == nil || len(grants) != 0 {
			t.Fatal("a re-created group received the grant", grants)
		}
	})
	t.Run("manual member is not granted again", func(t *testing.T) {
		manual := group.DeepCopy()
		manual.Spec.Clients = []public.ClientMatcher{{MAC: "00:11:22:33:44:55"}}
		_, _, grants, err := kubeSourceImage(listing(a), snapshot(first, manual), cluster, "kudurru", at)
		if err != nil {
			t.Fatal(err)
		}
		if len(grants) != 0 {
			t.Fatal("a manual member was granted", grants)
		}
	})
	t.Run("expiry is original", func(t *testing.T) {
		later := at.Add(28 * time.Minute)
		inv, _, _, err := kubeSourceImage(listing(a), snapshot(first, group), cluster, "kudurru", later)
		if err != nil {
			t.Fatal(err)
		}
		if inv.Lookup(ip, later) != nil {
			t.Fatal("an observation was renewed past its original expiry")
		}
	})
	t.Run("future head left out", func(t *testing.T) {
		future := head(second, "127.0.0.9", at.Add(time.Minute), at.Add(-time.Minute), false)
		inv, roots, _, err := kubeSourceImage(listing(a, future), snapshot(first, second, group), cluster, "kudurru", at)
		if err != nil {
			t.Fatal(err)
		}
		if roots[future.Head.Name] != "" || inv.Lookup(netip.MustParseAddr("127.0.0.9"), at) != nil || roots[a.Head.Name] == "" {
			t.Fatal("a head accepted after the replica's clock was used or refused the listing", roots)
		}
	})
	t.Run("canonical rebuild", func(t *testing.T) {
		objects := snapshot(first, second, group)
		inv1, roots1, grants1, err := kubeSourceImage(listing(a, b), objects, cluster, "kudurru", at)
		if err != nil {
			t.Fatal(err)
		}
		inv2, roots2, grants2, err := kubeSourceImage(listing(b, a), objects, cluster, "kudurru", at)
		if err != nil {
			t.Fatal(err)
		}
		if c := inv1.Lookup(ip, at); c == nil || c.ID != "client-second" {
			t.Fatal("the newer observation did not win the address", c)
		}
		if !reflect.DeepEqual(inv1.Clients(), inv2.Clients()) || !reflect.DeepEqual(roots1, roots2) || !reflect.DeepEqual(grants1, grants2) {
			t.Fatal("listing order changed the rebuilt inventory")
		}
		// The first source lost its address, so its MAC contribution has
		// no live client and grants nothing.
		if len(grants1) != 0 {
			t.Fatal("a source without a live client granted", grants1)
		}
	})
}
