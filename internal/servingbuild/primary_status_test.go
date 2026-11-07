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
