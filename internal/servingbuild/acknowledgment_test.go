package servingbuild

import (
	"context"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestRuntimeAcknowledgmentRetainsDependenciesAcrossOwnCommit(t *testing.T) {
	first, second := ring(t, "a"), ring(t, "b")
	keys, err := NewKeyringOwner(first)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixtureWithKeyrings(t, keys)
	lease, err := f.pub.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	raw, _ := lease.Bundle().Component("runtime")
	owner := raw.(*RuntimeOwner)
	_, _, imageCurrent := owner.NotifyKeys()
	if !imageCurrent() || !owner.DependenciesCurrent() {
		t.Fatal("original complete cut unavailable")
	}
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var settings public.Kudurru
		if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
			return err
		}
		settings.Status.Blocking.Disabled = true
		return tx.UpdateStatus(ctx, &settings)
	})
	if imageCurrent() || !owner.DependenciesCurrent() {
		t.Fatal("own commit must retire the image without replacing its dependencies")
	}
	for _, next := range []*dnsserver.StaticKeyring{second, first} {
		if err := keys.Replace(t.Context(), next); err != nil {
			t.Fatal(err)
		}
		if owner.DependenciesCurrent() {
			t.Fatal("replacement or restoration revived the original dependency cut")
		}
	}
	f.waitCurrent(t)
	newLease, err := f.pub.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer newLease.Release()
	raw, _ = newLease.Bundle().Component("runtime")
	newOwner := raw.(*RuntimeOwner)
	if !newOwner.DependenciesCurrent() {
		t.Fatal("new complete cut unavailable")
	}
	f.epoch.Add(1)
	if newOwner.DependenciesCurrent() || !keys.Capture().Current() {
		t.Fatal("acknowledgment ignored a changed non-key dependency")
	}
}
