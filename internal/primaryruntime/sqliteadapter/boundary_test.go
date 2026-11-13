package sqliteadapter

import (
	"testing"

	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
)

// Only a transition that changes nothing but Sequence and Parents, and only
// the parent boundary admissions, read their source by zone identity
// instead of the exact receipt (D404).
func TestBoundaryTransitionAndCommandAreNarrow(t *testing.T) {
	base := &zk.ZoneLifecycle{Spec: zk.ZoneLifecycleSpec{Zone: zk.ZoneID{ClusterID: "c", ObjectName: "child", UID: "u", Apex: "child.example."}, Type: "primary", Incarnation: 1, Sequence: 4, Phase: zk.PhasePreparing, Source: zk.LifecycleSource{UID: "u", ResourceVersion: "7", IntentDigest: "d"}}}
	obligation := base.DeepCopy()
	obligation.Spec.Sequence++
	obligation.Spec.Parents = []zk.ParentFence{{Zone: zk.ZoneID{ClusterID: "c", ObjectName: "parent", UID: "p", Apex: "example."}, Incarnation: 1}}
	if !boundaryTransition(base, obligation) {
		t.Fatal("an obligation-only transition is not a boundary transition")
	}
	for name, mutate := range map[string]func(*zk.ZoneLifecycle){
		"phase":       func(l *zk.ZoneLifecycle) { l.Spec.Phase = zk.PhaseFencing },
		"source":      func(l *zk.ZoneLifecycle) { l.Spec.Source.ResourceVersion = "8" },
		"incarnation": func(l *zk.ZoneLifecycle) { l.Spec.Incarnation++ },
		"removed":     func(l *zk.ZoneLifecycle) { l.Spec.Source.Removed = true },
	} {
		next := obligation.DeepCopy()
		mutate(next)
		if boundaryTransition(base, next) {
			t.Fatal("a transition changing", name, "was treated as boundary-only")
		}
	}
	parent := base.DeepCopy()
	for _, reason := range []string{"parent-fence", "late-parent-fence", "parent-release"} {
		if !boundaryCommand(pr.Command{Reason: reason, Lifecycle: base, Parent: parent}) || boundaryCommand(pr.Command{Reason: reason, Lifecycle: base}) {
			t.Fatal("boundary command classification", reason)
		}
	}
	for _, reason := range []string{"source-reconcile", "tombstone", "update", "notify-ack", "replacement-membership"} {
		if boundaryCommand(pr.Command{Reason: reason, Lifecycle: base, Parent: parent}) {
			t.Fatal("non-boundary command classified as boundary", reason)
		}
	}
}
