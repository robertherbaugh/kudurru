package currentguard

import (
	"context"
	"errors"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/sqliteadapter"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// advance edits a zone's DNSZone so that its resourceVersion and intent
// digest move past the revision its lifecycle recorded.
func (f *sqliteFixture) advance(name string) *public.DNSZone {
	f.t.Helper()
	var z public.DNSZone
	if err := f.s.Get(f.ctx(), store.Key{Kind: "DNSZone", Name: name}, &z); err != nil {
		f.t.Fatal(err)
	}
	z.Spec.DefaultTTL++
	if z.Spec.DefaultTTL < 120 {
		z.Spec.DefaultTTL = 120
	}
	saved, err := f.s.Apply(f.ctx(), &z, store.ApplyOptions{Manager: "api"})
	if err != nil {
		f.t.Fatal(err)
	}
	return saved.(*public.DNSZone)
}

// With the production background guard (CORE-OPEN4): a late child is fenced
// into an Active parent whose DNSZone moved on since the parent's last
// reconcile. Before the guard agreed with the adapter's boundary source check
// the pair deadlocked for good: the parent's reconcile needs every desired
// child fenced, and the guard refused the child's fence because the parent's
// recorded revision was not its latest.
func TestActualLateChildFencesAParentWhoseSourceAdvanced(t *testing.T) {
	for _, step := range []string{"AdmitAncestor", "FenceParents"} {
		t.Run(step, func(t *testing.T) {
			f := newSQLiteFixture(t)
			ctx := f.ctx()
			parent := f.active("parent", "example.")
			edited := f.advance("parent")
			child := f.begin(f.source("child", "child.example."), parent)
			if _, err := f.r.Reconcile(ctx, parent.Spec.Zone); !errors.Is(err, pr.ErrConflict) {
				t.Fatal("parent reconciled with an unfenced desired child:", err)
			}
			var err error
			if step == "AdmitAncestor" {
				_, err = f.r.AdmitAncestor(ctx, parent.Spec.Zone, child.Spec.Zone)
			} else {
				_, err = f.r.FenceParents(ctx, child.Spec.Zone)
			}
			if err != nil {
				t.Fatal("late child not fenced into a parent whose source advanced:", err)
			}
			if _, err := f.r.Reconcile(ctx, parent.Spec.Zone); err != nil {
				t.Fatal("parent did not reconcile once its child was fenced:", err)
			}
			refreshed, err := f.r.ReadLifecycle(ctx, parent.Spec.Zone)
			if err != nil {
				t.Fatal(err)
			}
			if refreshed.Spec.Source.ResourceVersion != edited.ResourceVersion {
				t.Fatalf("parent source not refreshed: %+v, want resourceVersion %s", refreshed.Spec.Source, edited.ResourceVersion)
			}
			if _, err := f.r.Reconcile(ctx, child.Spec.Zone); err != nil {
				t.Fatal("child did not activate:", err)
			}
			if l, err := f.r.ReadLifecycle(ctx, child.Spec.Zone); err != nil || l.Spec.Phase != zk.PhaseActive {
				t.Fatal("child not active:", err)
			}
		})
	}
}

// Both zones' DNSZones moved on after the child's lifecycle was created.
func TestActualLateChildFencesWhenBothSourcesAdvanced(t *testing.T) {
	f := newSQLiteFixture(t)
	ctx := f.ctx()
	parent := f.active("parent", "example.")
	child := f.begin(f.source("child", "child.example."), parent)
	f.advance("parent")
	childNow := f.advance("child")
	if _, err := f.r.AdmitAncestor(ctx, parent.Spec.Zone, child.Spec.Zone); err != nil {
		t.Fatal("late child not fenced when both sources advanced:", err)
	}
	fenced, err := f.r.ReadLifecycle(ctx, child.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	// The fence never advances the receipt (D404); the child's own exact
	// realignment records the current source below.
	if fenced.Spec.Source != child.Spec.Source || len(fenced.Spec.Parents) != 1 || fenced.Spec.Parents[0].CommitSequence == 0 {
		t.Fatalf("the fence changed more than the child's obligation: source %+v (receipt %+v), parents %+v", fenced.Spec.Source, child.Spec.Source, fenced.Spec.Parents)
	}
	if _, err := f.r.Reconcile(ctx, parent.Spec.Zone); err != nil {
		t.Fatal("parent did not reconcile once its child was fenced:", err)
	}
	if _, err := f.r.Reconcile(ctx, child.Spec.Zone); err != nil {
		t.Fatal("child did not activate:", err)
	}
	if l, err := f.r.ReadLifecycle(ctx, child.Spec.Zone); err != nil || l.Spec.Phase != zk.PhaseActive || l.Spec.Source.ResourceVersion != childNow.ResourceVersion {
		t.Fatalf("child not active on its current source (resourceVersion %s): %v %+v", childNow.ResourceVersion, err, l)
	}
}

// A parent whose DNSZone was deleted and created again under the same name
// is a different source: the child is still not fenced into it.
func TestActualLateChildIsNotFencedIntoAReplacedParent(t *testing.T) {
	f := newSQLiteFixture(t)
	ctx := f.ctx()
	parent := f.active("parent", "example.")
	child := f.begin(f.source("child", "child.example."), parent)
	if err := f.s.Delete(ctx, store.Key{Kind: "DNSZone", Name: "parent"}, store.DeleteOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	f.source("parent", "example.")
	if _, err := f.r.AdmitAncestor(ctx, parent.Spec.Zone, child.Spec.Zone); err == nil {
		t.Fatal("a late child was fenced into a replaced parent")
	}
}

// The guard relaxes the revision check only for a check the adapter marked
// Boundary (D404) with a boundary reason, and keeps the source identity
// check for it: an unmarked check with the same reason, and every other
// reason, still need the recorded revision, and a boundary check still
// needs the same, undeleted source zone of the same type and apex.
func TestActualBackgroundGuardRelaxesOnlyBoundaryRevisions(t *testing.T) {
	f := newSQLiteFixture(t)
	parent := f.active("parent", "example.")
	recorded := parent.DeepCopy()
	edited := f.advance("parent")
	policy := f.view(parent).Aggregate.Policy
	checkMarked := func(reason string, source *public.DNSZone, withPolicy, boundary bool) error {
		var err error
		commandErr := f.s.Command(f.ctx(), f.fence.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			c := sqliteadapter.BackgroundCheck{Lifecycle: recorded.DeepCopy(), Source: source, Reason: reason, Boundary: boundary}
			if withPolicy {
				p := policy
				c.Policy = &p
			}
			err = f.g.checkBackground(ctx, tx, f.h.current, c)
			return errors.New("roll back")
		})
		if commandErr == nil {
			t.Fatal("probe command committed")
		}
		return err
	}
	check := func(reason string, source *public.DNSZone, withPolicy bool) error {
		return checkMarked(reason, source, withPolicy, true)
	}
	for _, reason := range []string{"parent-fence", "late-parent-fence", "parent-release"} {
		if err := checkMarked(reason, edited.DeepCopy(), true, false); !errors.Is(err, pr.ErrConflict) {
			t.Errorf("%s without the adapter's boundary mark accepted a source past the recorded revision: %v", reason, err)
		}
		if err := check(reason, edited.DeepCopy(), true); err != nil {
			t.Errorf("%s refused a parent whose source only advanced: %v", reason, err)
		}
		replaced := edited.DeepCopy()
		replaced.UID = "another-uid"
		if err := check(reason, replaced, true); !errors.Is(err, pr.ErrConflict) {
			t.Errorf("%s accepted a replaced source: %v", reason, err)
		}
		retyped := edited.DeepCopy()
		retyped.Spec.Type = public.ZoneTypeSecondary
		if err := check(reason, retyped, true); !errors.Is(err, pr.ErrConflict) {
			t.Errorf("%s accepted a retyped source: %v", reason, err)
		}
		moved := edited.DeepCopy()
		moved.Spec.ZoneName = "other.example."
		if err := check(reason, moved, true); !errors.Is(err, pr.ErrConflict) {
			t.Errorf("%s accepted a moved source: %v", reason, err)
		}
		deleting := edited.DeepCopy()
		now := metav1.Now()
		deleting.DeletionTimestamp = &now
		if err := check(reason, deleting, true); !errors.Is(err, pr.ErrConflict) {
			t.Errorf("%s accepted a deleting source: %v", reason, err)
		}
	}
	if err := checkMarked("source-reconcile", edited.DeepCopy(), true, false); !errors.Is(err, pr.ErrConflict) {
		t.Error("source-reconcile accepted a source past the recorded revision:", err)
	}
	if err := check("source-reconcile", edited.DeepCopy(), true); !errors.Is(err, pr.ErrUnavailable) {
		t.Error("the boundary mark was honored for source-reconcile:", err)
	}
	if err := checkMarked("lifecycle", edited.DeepCopy(), false, false); !errors.Is(err, pr.ErrConflict) {
		t.Error("lifecycle accepted a source past the recorded revision:", err)
	}
	if err := check("lifecycle", edited.DeepCopy(), false); err != nil {
		t.Error("an obligation-only lifecycle check refused the same zone:", err)
	}
	deleting := edited.DeepCopy()
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	if err := check("lifecycle", deleting, false); !errors.Is(err, pr.ErrConflict) {
		t.Error("an obligation-only lifecycle check accepted a deleting source:", err)
	}
}
