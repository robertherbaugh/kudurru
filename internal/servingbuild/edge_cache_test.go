package servingbuild

import (
	"context"
	"errors"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEdgeCacheOriginalPublicationAndRegexABA(t *testing.T) {
	f := newFixture(t)
	edge, err := NewEdgeCache(f.pub, "node-a", func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	original, err := edge.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	zones, err := edge.Zones(t.Context())
	if err != nil || zones.Node != "node-a" || zones.Resolver == nil || *zones.Resolver != *original {
		t.Fatal("zone serial projection did not retain the same complete owner")
	}
	held, err := f.pub.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	chain := newChain(t, f.pub)
	for i, expression := range []string{`^blocked\.example$`, `^other\.example$`, `^blocked\.example$`} {
		list := &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "deny", Namespace: "default"}, Spec: public.BlockListSpec{Mode: "block", Sources: []public.ListSource{{Name: "inline", Entries: []public.ListEntry{{Type: public.EntryRegex, Value: expression}}}}}}
		if _, err := f.store.Apply(t.Context(), list, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			t.Fatal(err)
		}
		f.waitCurrent(t)
		state, err := edge.State(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if state.Publication <= original.Publication || state.Identity == original.Identity || state.Instance != original.Instance {
			t.Fatal("regex publication/ABA lost provenance", i)
		}
		original = state
	}
	if err := f.pub.CheckReadLease(t.Context(), held); !errors.Is(err, serving.ErrPending) {
		t.Fatal("original lease adopted newer publication")
	}
	// Original DNS retains its original complete owner even after regex ABA.
	wantIP(t, query(t, chain, held, "blocked.example."), "192.0.2.11")
	f.blocked.Store(true)
	if _, err := edge.State(t.Context()); !errors.Is(err, serving.ErrPending) {
		t.Fatal("changed original dependency fabricated current proof")
	}
}

func TestEdgeCacheFinalOriginalDependencyFence(t *testing.T) {
	f := newFixture(t)
	edge, err := NewEdgeCache(f.pub, "node-a", func() time.Time {
		// Runs after the original protected capture, before final projection.
		f.blocked.Store(true)
		return f.now
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := edge.Zones(t.Context()); !errors.Is(err, serving.ErrPending) {
		t.Fatal("projection published proof after original dependency changed")
	}
}

func TestEdgeCacheExactToggleExpiryAndRuntimeInstance(t *testing.T) {
	f := newFixture(t)
	until := f.now.Truncate(time.Second).Add(2 * time.Second)
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var settings public.Kudurru
		if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
			return err
		}
		deadline := metav1.NewTime(until)
		settings.Status.Blocking.Disabled = true
		settings.Status.Blocking.DisabledUntil = &deadline
		return tx.UpdateStatus(ctx, &settings)
	})
	f.waitCurrent(t)
	clock := f.now
	edge, err := NewEdgeCache(f.pub, "node-a", func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	before, err := edge.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if before.BlockingEnabled || !before.ValidUntil.Equal(until) || !before.DisabledUntil.Equal(until) {
		t.Fatal("disable lease outlived original timer")
	}
	clock = until
	after, err := edge.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !after.BlockingEnabled || after.Publication != before.Publication || after.Identity != before.Identity || !after.ValidUntil.After(until) {
		t.Fatal("exact timer boundary forged a new publication or failed to enable")
	}
	other, err := NewEdgeCache(f.pub, "node-a", func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := other.State(t.Context())
	if err != nil || restarted.Instance == after.Instance {
		t.Fatal("source runtime reused ABA identity")
	}
}
