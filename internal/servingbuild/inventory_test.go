package servingbuild

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/inventory/persistence"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func inventoryFromLease(t *testing.T, lease *serving.Lease) *inventory.Snapshot {
	t.Helper()
	value, ok := lease.Bundle().Component("inventory")
	if !ok {
		t.Fatal("complete bundle has no inventory owner")
	}
	owner, ok := value.(*InventoryOwner)
	if !ok || owner == nil {
		t.Fatal("complete bundle has wrong inventory owner")
	}
	return owner.Snapshot
}

func stageInventory(t *testing.T, f *fixture, source string, revision uint64, clients []inventory.Client) {
	t.Helper()
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		current, err := persistence.Load(ctx, tx, tx.Authority().ClusterID, f.now)
		if err != nil {
			return err
		}
		next, err := inventory.PrepareReplacement(current.State, source, revision, clients, f.now)
		if err != nil {
			return err
		}
		_, err = persistence.Stage(ctx, tx, current.Version, next, f.now)
		return err
	})
	f.waitCurrent(t)
}

func TestDurableInventoryHeldCutsKeepOriginalExpiryAndRemoval(t *testing.T) {
	f := newFixture(t)
	ip := netip.MustParseAddr("192.0.2.41")
	oldClient := inventory.Client{ID: "old-device", IPs: []netip.Addr{ip}, ObservedAt: f.now.Add(-time.Minute), ExpiresAt: f.now.Add(time.Hour)}
	stageInventory(t, f, "old-source", 1, []inventory.Client{oldClient})
	client := inventory.Client{ID: "new-device", IPs: []netip.Addr{ip}, Hostname: "device.example", ObservedAt: f.now, ExpiresAt: f.now.Add(time.Hour)}
	stageInventory(t, f, "current-source", 1, []inventory.Client{client})
	held, err := f.pub.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	before := inventoryFromLease(t, held)
	if got := before.Lookup(ip, f.now); got == nil || got.ID != client.ID || !got.ExpiresAt.Equal(client.ExpiresAt) {
		t.Fatalf("held inventory did not retain original observation: %+v", got)
	}

	stageInventory(t, f, "current-source", 2, nil)
	fresh, err := f.pub.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Release()
	after := inventoryFromLease(t, fresh)
	if after.Generation() != before.Generation()+1 || after.Lookup(ip, f.now) != nil {
		t.Fatal("withdrawal fell back to an older address owner")
	}
	if got := before.Lookup(ip, f.now); got == nil || got.ID != client.ID {
		t.Fatal("a held complete cut changed after inventory replacement")
	}
	if before.Lookup(ip, client.ExpiresAt) != nil {
		t.Fatal("held inventory renewed its original absolute expiry")
	}
	f.stop()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	if before.Lookup(ip, f.now) == nil || after.Lookup(ip, f.now) != nil {
		t.Fatal("detached inventory depended on an open store")
	}
}

func TestInventoryPreparationRequiresCompleteDurableEvidence(t *testing.T) {
	f := newFixture(t)
	image, err := f.store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := prepareInventory(image, f.now)
	if err != nil || owner == nil || owner.Snapshot.Generation() != 0 {
		t.Fatalf("explicit empty root: owner=%v error=%v", owner, err)
	}
	for _, mutate := range []func(*replication.Image){
		func(i *replication.Image) { i.Payload.Runtime = nil },
		func(i *replication.Image) {
			for n, row := range i.Payload.Runtime {
				if row.Key == persistence.RootKey {
					i.Payload.Runtime[n].Data = []byte(`{"formatVersion":99}`)
				}
			}
		},
	} {
		copyImage := *image
		copyImage.Payload.Runtime = append([]replication.Entry(nil), image.Payload.Runtime...)
		mutate(&copyImage)
		if _, err := prepareInventory(&copyImage, f.now); err == nil {
			t.Fatal("incomplete inventory silently became empty")
		}
	}
}
