package api

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/dhcp"
	"github.com/robertherbaugh/kudurru/internal/dhcpdns"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

type inventoryTestLeases []dhcpdns.Lease

func (l inventoryTestLeases) Leases() []dhcpdns.Lease { return l }

// D397: GET /inventory lists devices that never queried, merges this node's
// DHCP leases under the identify stage's rule, flags stale and colliding
// entries, filters and pages by (source, id), and needs inventory.read.
func TestInventoryListsDevicesLeasesAndConflicts(t *testing.T) {
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	holder := inventory.New(func() time.Time { return now })
	ip := netip.MustParseAddr
	if err := holder.Replace("integration-a", 1, []inventory.Client{
		{ID: "quiet", Name: "Quiet TV", Hostname: "tv.lan", IPs: []netip.Addr{ip("192.0.2.5")}, MACs: []string{"02:00:00:00:00:05"}, Tags: []string{"media"}, ObservedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)},
		{ID: "gone", IPs: []netip.Addr{ip("192.0.2.6")}, ObservedAt: now.Add(-2 * time.Minute), ExpiresAt: now.Add(-time.Minute)},
		{ID: "tie-1", IPs: []netip.Addr{ip("192.0.2.7")}, ObservedAt: now, ExpiresAt: now.Add(time.Hour)},
		{ID: "tie-2", IPs: []netip.Addr{ip("192.0.2.7")}, ObservedAt: now, ExpiresAt: now.Add(time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	lease := func(scope, addr, mac string, expires time.Time) dhcpdns.Lease {
		return dhcpdns.Lease{Scope: scope, BoundLease: dhcp.BoundLease{Address: ip(addr), MAC: mac, ExpiresAt: expires}}
	}
	leases := inventoryTestLeases{
		lease("lan", "192.0.2.5", "02:00:00:00:00:99", now.Add(time.Hour)),  // inventory MACs win
		lease("lan", "192.0.2.10", "02:00:00:00:00:10", now.Add(time.Hour)), // fills the MAC
		lease("lan", "192.0.2.11", "02:00:00:00:00:11", now),                // expired
		lease("lan", "192.0.2.12", "02:00:00:00:00:12", now.Add(time.Hour)),
		lease("guest", "192.0.2.12", "02:00:00:00:00:13", now.Add(time.Hour)), // collides
	}
	failing := false
	h := newHarness(t, func(o *Options) {
		o.Now = func() time.Time { return now }
		o.InventoryReads = func(context.Context) (*inventory.Snapshot, error) {
			if failing {
				return nil, errors.New("cut unavailable")
			}
			return holder.Snapshot(), nil
		}
		o.InventoryLeases = leases
	})
	var body InventoryBody
	h.do("GET", BasePath+"/inventory", "auditor", nil).want(200).json(&body)
	if body.Node != "node-a" || body.Generation != 1 || !body.ReadAt.Equal(now) || len(body.Items) != 9 || body.Continue != "" {
		t.Fatalf("inventory: %+v", body)
	}
	byKey := map[string]InventoryDevice{}
	for _, d := range body.Items {
		byKey[d.Source+" "+d.ID] = d
	}
	state := func(key string) string {
		t.Helper()
		d, ok := byKey[key]
		if !ok || len(d.Addresses) != 1 {
			t.Fatalf("%s: %+v", key, d)
		}
		return d.Addresses[0].State
	}
	quiet := byKey["integration-a quiet"]
	if quiet.Kind != InventoryKindSource || quiet.Stale || quiet.Name != "Quiet TV" || quiet.ObservedAt == nil || len(quiet.MACs) != 1 || quiet.Tags[0] != "media" || state("integration-a quiet") != "assigned" {
		t.Fatalf("device without DNS traffic: %+v", quiet)
	}
	if !byKey["integration-a gone"].Stale || state("integration-a gone") != "expired" {
		t.Fatal("stale device not flagged")
	}
	if state("integration-a tie-1") != "ambiguous" || state("integration-a tie-2") != "ambiguous" {
		t.Fatal("colliding inventory claims not flagged")
	}
	if a := byKey["dhcp/lan 192.0.2.5"].Addresses[0]; a.State != "superseded" || a.OwnerSource != "integration-a" || a.OwnerID != "quiet" {
		t.Fatalf("inventory MACs must win over a lease: %+v", a)
	}
	if d := byKey["dhcp/lan 192.0.2.10"]; d.Kind != InventoryKindDHCP || d.ObservedAt != nil || d.MACs[0] != "02:00:00:00:00:10" || state("dhcp/lan 192.0.2.10") != "assigned" {
		t.Fatalf("lease fill: %+v", d)
	}
	if !byKey["dhcp/lan 192.0.2.11"].Stale || state("dhcp/lan 192.0.2.11") != "expired" {
		t.Fatal("expired lease not flagged")
	}
	if state("dhcp/lan 192.0.2.12") != "ambiguous" || state("dhcp/guest 192.0.2.12") != "ambiguous" {
		t.Fatal("colliding leases not flagged")
	}

	// Pages follow (source, id) and the filters apply before the cut.
	var keys []string
	cursor := ""
	for range 10 {
		var page InventoryBody
		h.do("GET", BasePath+"/inventory?limit=2&continue="+cursor, "operator", nil).want(200).json(&page)
		for _, d := range page.Items {
			keys = append(keys, d.Source+" "+d.ID)
		}
		if cursor = page.Continue; cursor == "" {
			break
		}
	}
	if len(keys) != 9 || keys[0] != "dhcp/guest 192.0.2.12" || keys[8] != "integration-a tie-2" {
		t.Fatalf("pages: %v", keys)
	}
	for query, want := range map[string]int{"kind=dhcp": 5, "kind=inventory&stale=false": 3, "stale=true": 2, "q=QUIET": 1, "q=02:00:00:00:00:13": 1, "source=dhcp/lan": 4, "q=192.0.2.7": 2} {
		var page InventoryBody
		h.do("GET", BasePath+"/inventory?"+query, "admin", nil).want(200).json(&page)
		if len(page.Items) != want {
			t.Fatalf("%s: %d items", query, len(page.Items))
		}
	}
	h.do("GET", BasePath+"/inventory?continue=***", "admin", nil).want(400)
	h.do("GET", BasePath+"/inventory", "viewer", nil).want(403)
	failing = true
	h.do("GET", BasePath+"/inventory", "admin", nil).want(503)

	unmounted := newHarness(t)
	unmounted.do("GET", BasePath+"/inventory", "admin", nil).want(503)
}

// infoStore answers Info with a fixed driver and replication role.
type infoStore struct {
	store.Store
	info store.Info
}

func (s infoStore) Info() store.Info { return s.info }

// D397: GET /inventory says whether the answering node is one member of a
// SQLite cluster (its DHCP leases are its own), so an operator or auditor,
// who cannot read GET /cluster, still gets the node-local caveat on a
// primary. A standalone node and other drivers answer false.
func TestInventorySaysWhenLeasesAreNodeLocal(t *testing.T) {
	for _, tc := range []struct {
		name string
		info store.Info
		want bool
	}{
		{"memory", store.Info{Driver: "memory"}, false},
		{"sqlite standalone", store.Info{Driver: sqlite.DriverName, Role: "standalone"}, false},
		{"sqlite primary", store.Info{Driver: sqlite.DriverName, Role: "primary"}, true},
		{"sqlite follower", store.Info{Driver: sqlite.DriverName, Role: "follower"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(o *Options) {
				o.Store = infoStore{Store: o.Store, info: tc.info}
				o.InventoryReads = func(context.Context) (*inventory.Snapshot, error) { return nil, nil }
			})
			for _, role := range []string{"auditor", "operator"} {
				var body InventoryBody
				h.do("GET", BasePath+"/inventory", role, nil).want(200).json(&body)
				if body.NodeLocalLeases != tc.want {
					t.Fatalf("%s: nodeLocalLeases %v, want %v", role, body.NodeLocalLeases, tc.want)
				}
			}
		})
	}
}
