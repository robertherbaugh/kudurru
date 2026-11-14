package sqliteadapter

import (
	"context"
	"errors"
	"reflect"
	"testing"

	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
)

func TestLifecycleTransitionAcceptsEmptyParentsAfterSQLiteCAS(t *testing.T) {
	f := setup(t)
	next := f.intent(f.zone("empty", "example."))
	next.Spec.Parents = []zk.ParentFence{}
	got, err := f.r.Transition(f.ctx(), nil, next)
	// Read the actual committed row even on failure: the previous result
	// comparison reported ErrInvalid after this exact CAS had succeeded.
	durable, readErr := f.r.ReadLifecycle(f.ctx(), next.Spec.Zone)
	if readErr != nil || durable.Spec.Sequence != 1 || len(durable.Spec.Parents) != 0 {
		t.Fatalf("initial lifecycle was not durably admitted: %v", readErr)
	}
	if err != nil || got == nil {
		t.Fatalf("committed empty parent list was rejected: %v", err)
	}
	if !reflect.DeepEqual(got, durable) || next.Spec.Parents == nil || next.ResourceVersion != "" {
		t.Fatal("result differs from durable row or caller intent was mutated")
	}
	next = got.DeepCopy()
	next.Spec.Sequence++
	next.Spec.Phase = zk.PhaseFencing
	next.Spec.Parents = []zk.ParentFence{}
	got, err = f.r.Transition(f.ctx(), got, next)
	if err != nil || got.Spec.Phase != zk.PhaseFencing || got.Spec.Sequence != 2 {
		t.Fatalf("empty-parent phase transition failed: %v", err)
	}
}

func TestLifecycleTransitionRejectsChangedSQLiteCASResult(t *testing.T) {
	for _, change := range []struct {
		name string
		edit func(*zk.ZoneLifecycle)
	}{
		{"sequence", func(l *zk.ZoneLifecycle) { l.Spec.Sequence++ }},
		{"source", func(l *zk.ZoneLifecycle) { l.Spec.Source.ResourceVersion = "different-source" }},
		{"phase", func(l *zk.ZoneLifecycle) { l.Spec.Phase = zk.PhaseFencing }},
		{"parent-incarnation", func(l *zk.ZoneLifecycle) { l.Spec.Parents[0].Incarnation++ }},
		{"parent-omission", func(l *zk.ZoneLifecycle) { l.Spec.Parents = nil }},
	} {
		t.Run(change.name, func(t *testing.T) {
			f := setup(t)
			parent := f.begin(f.zone("parent", "example."))
			next := f.intent(f.zone("child", "child.example."), parent)
			r, err := pr.New(pr.Options{Store: f.s, Repository: f.repo, LifecycleCAS: func(ctx context.Context, current, proposed *zk.ZoneLifecycle) (*zk.ZoneLifecycle, error) {
				out, err := f.a.LifecycleCAS(ctx, current, proposed)
				if err != nil {
					return nil, err
				}
				change.edit(out)
				if err := out.Validate(); err != nil {
					t.Fatalf("altered result must remain structurally valid: %v", err)
				}
				return out, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			if out, err := r.Transition(f.ctx(), nil, next); out != nil || !errors.Is(err, pr.ErrInvalid) {
				t.Fatalf("changed callback result accepted: %v", err)
			}
			durable, err := f.r.ReadLifecycle(f.ctx(), next.Spec.Zone)
			if err != nil || !reflect.DeepEqual(durable.Spec, next.Spec) {
				t.Fatalf("callback result mutation changed durable intent: %v", err)
			}
		})
	}
}
