package sqliteadapter

import (
	"context"
	"errors"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

var errCaptured = errors.New("captured, not admitted")

// statusWrite advances a DNSZone's resourceVersion as the committed-status
// controller does, without changing its intent.
func (f *fixture) statusWrite(name string) {
	f.t.Helper()
	var z public.DNSZone
	if err := f.s.Get(f.ctx(), store.Key{Kind: "DNSZone", Name: name}, &z); err != nil {
		f.t.Fatal(err)
	}
	z.Status.Serial++
	if err := f.s.UpdateStatus(f.ctx(), &z); err != nil {
		f.t.Fatal(err)
	}
}

// boundaryFixture is an active primary parent and a Fencing child below it
// whose obligation is not yet fenced, with both lifecycle receipts behind
// their DNSZones after a status write. It returns the parent-fence command
// the runtime would admit, captured before the adapter sees it.
func boundaryFixture(t *testing.T) (*fixture, *zk.ZoneLifecycle, pr.Command) {
	t.Helper()
	f := setup(t)
	ctx := f.ctx()
	parent := f.active("parent", "example.")
	child := f.begin(f.zone("child", "child.example."), parent)
	next := child.DeepCopy()
	next.Spec.Sequence++
	next.Spec.Phase = zk.PhaseFencing
	child, err := f.r.Transition(ctx, child, next)
	if err != nil {
		t.Fatal(err)
	}
	f.statusWrite("parent")
	f.statusWrite("child")
	for _, l := range []*zk.ZoneLifecycle{parent, child} {
		var z public.DNSZone
		if err := f.s.Get(ctx, store.Key{Kind: "DNSZone", Name: l.Spec.Zone.ObjectName}, &z); err != nil {
			t.Fatal(err)
		}
		token, err := primaryzone.SourceToken(&z)
		if err != nil {
			t.Fatal(err)
		}
		current, err := f.r.ReadLifecycle(ctx, l.Spec.Zone)
		if err != nil {
			t.Fatal(err)
		}
		if current.Spec.Source.ResourceVersion == token.ResourceVersion {
			t.Fatal("fixture did not put the receipt behind its DNSZone", l.Spec.Zone.ObjectName)
		}
	}
	var captured *pr.Command
	recorder, err := pr.New(pr.Options{Store: f.s, Repository: f.repo, Reader: primaryzone.NewReader(f.s, primaryzone.ReaderOptions{PageSize: 1}), LifecycleCAS: f.a.LifecycleCAS, Admit: func(_ context.Context, c pr.Command) (*zonestate.View, error) {
		if c.Reason == "parent-fence" && captured == nil {
			captured = &c
		}
		return nil, errCaptured
	}, Proofs: issuer{}, Publish: func(context.Context, pr.Cut, pr.Member) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.FenceParents(ctx, child.Spec.Zone); !errors.Is(err, errCaptured) || captured == nil {
		t.Fatal("parent-fence command was not captured", err)
	}
	return f, child, *captured
}

// The obligation-only child CAS that follows a committed parent fence.
func fenced(child *zk.ZoneLifecycle, committed *zonestate.View) *zk.ZoneLifecycle {
	next := child.DeepCopy()
	next.Spec.Sequence++
	next.Spec.Parents[0].CommitSequence = committed.Head.Spec.CommitSequence
	next.Spec.Parents[0].RootDigest = committed.Head.Spec.RootDigest
	return next
}

// D404 relaxes only the receipt's resourceVersion and intent digest for the
// boundary admissions and an obligation-only lifecycle CAS. Through the real
// SQLite command transaction, with both receipts behind their DNSZones:
//   - the parent-fence admission refuses a replaced, retyped or deleted
//     DNSZone of either zone;
//   - the child's obligation-only CAS refuses a replaced, retyped or deleted
//     child DNSZone;
//   - the same zones with a newer resourceVersion are admitted.
//
// The CAS reads the child's own source only, as it did with the exact
// receipt check before D404 (fix-10-*.log runs this test on the base
// adapter: a parent retyped or deleted after its fence committed does not
// refuse the child's CAS there either). A changed parent is handled by the
// parent's retirement, which makes the child's obligation obsolete
// (driver obsoleteParent). An apex never changes in place: the store
// refuses a zoneName edit, so a moved apex is a replaced UID.
func TestBoundarySourceRefusesAnotherZoneThroughTheTransaction(t *testing.T) {
	refused := func(err error) bool { return errors.Is(err, pr.ErrConflict) || errors.Is(err, store.ErrNotFound) }
	mutations := map[string]func(f *fixture, name, apex string){
		"replaced UID": func(f *fixture, name, apex string) {
			if err := f.s.Delete(f.ctx(), store.Key{Kind: "DNSZone", Name: name}, store.DeleteOptions{Manager: "api"}); err != nil {
				f.t.Fatal(err)
			}
			f.zone(name, apex)
		},
		"retyped": func(f *fixture, name, _ string) {
			f.edit(name, func(z *public.DNSZone) {
				z.Spec.Type = public.ZoneTypeSecondary
				z.Spec.Secondary = &public.SecondarySpec{Primaries: []string{"192.0.2.53"}}
			})
		},
		"deleted": func(f *fixture, name, _ string) {
			if err := f.s.Delete(f.ctx(), store.Key{Kind: "DNSZone", Name: name}, store.DeleteOptions{Manager: "api"}); err != nil {
				f.t.Fatal(err)
			}
		},
	}
	zones := map[string]string{"parent": "example.", "child": "child.example."}

	t.Run("apex is immutable", func(t *testing.T) {
		f := setup(t)
		z := f.zone("child", "child.example.")
		z.Spec.ZoneName = "moved.child.example."
		if _, err := f.s.Apply(f.ctx(), z, store.ApplyOptions{Manager: "api"}); err == nil {
			t.Fatal("the store let a DNSZone change its apex in place")
		}
	})
	t.Run("same zone", func(t *testing.T) {
		f, child, command := boundaryFixture(t)
		committed, err := f.a.Admission(f.ctx(), command)
		if err != nil {
			t.Fatal("parent-fence refused for the same zones behind their receipts", err)
		}
		if _, err := f.a.LifecycleCAS(f.ctx(), child, fenced(child, committed)); err != nil {
			t.Fatal("obligation-only CAS refused for the same zone behind its receipt", err)
		}
	})
	for mutation, mutate := range mutations {
		for name, apex := range zones {
			t.Run(name+" "+mutation+"/parent-fence", func(t *testing.T) {
				f, _, command := boundaryFixture(t)
				mutate(f, name, apex)
				if _, err := f.a.Admission(f.ctx(), command); !refused(err) {
					t.Fatal("parent-fence admitted for another zone", err)
				}
			})
			if name != "child" {
				continue
			}
			t.Run(name+" "+mutation+"/obligation CAS", func(t *testing.T) {
				f, child, command := boundaryFixture(t)
				committed, err := f.a.Admission(f.ctx(), command)
				if err != nil {
					t.Fatal(err)
				}
				mutate(f, name, apex)
				if _, err := f.a.LifecycleCAS(f.ctx(), child, fenced(child, committed)); !refused(err) {
					t.Fatal("obligation-only CAS admitted for another zone", err)
				}
			})
		}
	}
}

func (f *fixture) edit(name string, mutate func(*public.DNSZone)) {
	f.t.Helper()
	var z public.DNSZone
	if err := f.s.Get(f.ctx(), store.Key{Kind: "DNSZone", Name: name}, &z); err != nil {
		f.t.Fatal(err)
	}
	mutate(&z)
	if _, err := f.s.Apply(f.ctx(), &z, store.ApplyOptions{Manager: "api"}); err != nil {
		f.t.Fatal(err)
	}
}
