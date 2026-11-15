package servingbuild

import (
	"context"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
)

func TestPrimaryStatusExactPlaceholderAndProtectedPending(t *testing.T) {
	keys, err := dnsserver.NewStaticKeyring()
	if err != nil {
		t.Fatal(err)
	}
	owner, err := NewKeyringOwner(keys)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixtureWithKeyrings(t, owner)
	status, err := NewPrimaryStatus(PrimaryStatusOptions{Store: f.store, Publisher: f.pub, Pin: owner.PrimaryPin(f.pub), Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	token, err := f.store.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	ctx := sqlite.WithWriterToken(t.Context(), token)
	read := func() *public.DNSZone {
		t.Helper()
		var z public.DNSZone
		if err := f.store.Get(ctx, store.Key{Kind: "DNSZone", Name: "uncommitted"}, &z); err != nil {
			t.Fatal(err)
		}
		return &z
	}
	z := read()
	if wrote, retry, err := status.Reconcile(ctx, z); err != nil || !wrote || retry <= 0 {
		t.Fatal("placeholder status failed", wrote, retry, err)
	}
	z = read()
	if ready := meta.FindStatusCondition(z.Status.Conditions, public.ConditionReady); ready == nil || ready.Reason != "AwaitingCommittedPrimary" || ready.Status != "False" || z.Status.Serial != 0 || z.Status.ContentHash != "" {
		t.Fatal("placeholder claimed data", z.Status)
	}
	f.waitCurrent(t)
	if wrote, _, err := status.Reconcile(ctx, z); err != nil || wrote {
		t.Fatal("placeholder status churned", wrote, err)
	}
	// Same owner, same keys, but the original complete dependency cut is gone.
	f.blocked.Store(true)
	f.epoch.Add(1)
	f.pub.Wake()
	before := read()
	if wrote, retry, err := status.Reconcile(ctx, before); err != nil || wrote || retry <= 0 {
		t.Fatal("pending capture wrote status", wrote, retry, err)
	}
	if !equality.Semantic.DeepEqual(before, read()) {
		t.Fatal("pending capture changed source RV")
	}
}

func TestPrimaryStatusMismatchedCachedObjectCannotBorrowNewLease(t *testing.T) {
	keys, err := dnsserver.NewStaticKeyring()
	if err != nil {
		t.Fatal(err)
	}
	owner, err := NewKeyringOwner(keys)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixtureWithKeyrings(t, owner)
	status, err := NewPrimaryStatus(PrimaryStatusOptions{Store: f.store, Publisher: f.pub, Pin: owner.PrimaryPin(f.pub)})
	if err != nil {
		t.Fatal(err)
	}
	token, err := f.store.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	ctx := sqlite.WithWriterToken(t.Context(), token)
	var old public.DNSZone
	if err := f.store.Get(ctx, store.Key{Kind: "DNSZone", Name: "uncommitted"}, &old); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Command(ctx, token.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := tx.Delete(ctx, store.KeyOf(&old), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		next := old.DeepCopy()
		next.UID = ""
		next.ResourceVersion = ""
		next.Generation = 0
		_, err := tx.Apply(ctx, next, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	before, err := f.store.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if wrote, retry, err := status.Reconcile(ctx, &old); err != nil || wrote || retry <= 0 {
		t.Fatal("old UID borrowed new lease", wrote, retry, err)
	}
	after, err := f.store.ReadGeneration(ctx)
	if err != nil || !after.Generation().Same(before.Generation()) {
		t.Fatal("old UID changed durable status", err)
	}
}

// A primary zone without a committed head keeps the serial and content hash
// an earlier release stamped in its status: the first commit continues that
// serial when the content is the same (primaryzone.Compiler), and a
// placeholder written before it must not erase them. 0.1.0 directories lost
// their zone serials on some upgrades when this status write won the race
// (D373). The placeholder still claims no records and no readiness.
func TestPrimaryStatusPlaceholderKeepsTheSerialOfAnEarlierRelease(t *testing.T) {
	keys, err := dnsserver.NewStaticKeyring()
	if err != nil {
		t.Fatal(err)
	}
	owner, err := NewKeyringOwner(keys)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixtureWithKeyrings(t, owner)
	status, err := NewPrimaryStatus(PrimaryStatusOptions{Store: f.store, Publisher: f.pub, Pin: owner.PrimaryPin(f.pub), Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	token, err := f.store.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	ctx := sqlite.WithWriterToken(t.Context(), token)
	read := func() *public.DNSZone {
		t.Helper()
		var z public.DNSZone
		if err := f.store.Get(ctx, store.Key{Kind: "DNSZone", Name: "uncommitted"}, &z); err != nil {
			t.Fatal(err)
		}
		return &z
	}
	// What 0.1.0 stamped.
	z := read()
	z.Status.Serial, z.Status.ContentHash, z.Status.RecordCount = 1794376393, "sha256:5429400000000000000000000000000000000000000000000000000000000000", 3
	if err := f.store.UpdateStatus(ctx, z); err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	var wrote bool
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var retry time.Duration
		if wrote, retry, err = status.Reconcile(ctx, read()); err != nil {
			t.Fatal(err)
		}
		if wrote || retry <= 0 || time.Now().After(deadline) {
			break
		}
	}
	if !wrote {
		t.Fatal("placeholder status not written")
	}
	z = read()
	ready := meta.FindStatusCondition(z.Status.Conditions, public.ConditionReady)
	if ready == nil || ready.Reason != "AwaitingCommittedPrimary" || ready.Status != "False" || z.Status.RecordCount != 0 {
		t.Fatal("placeholder claimed data", z.Status)
	}
	if z.Status.Serial != 1794376393 || z.Status.ContentHash != "sha256:5429400000000000000000000000000000000000000000000000000000000000" {
		t.Fatalf("placeholder erased the serial of the earlier release: %d %q", z.Status.Serial, z.Status.ContentHash)
	}
}
