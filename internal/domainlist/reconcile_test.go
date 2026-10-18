package domainlist

import (
	"context"
	"slices"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// TestReconcile: a policy created after the entries gets the everyone
// lists; a policy that stops naming a group loses the group's list; file
// policies are left alone.
func TestReconcile(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Add(ctx, AddRequest{Action: Allow, Namespace: "default", Type: v1alpha1.EntryExact, Values: []string{"a.example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Add(ctx, AddRequest{Action: Deny, Namespace: "default", Type: v1alpha1.EntryExact, Values: []string{"b.example"}, Scope: Scope{Groups: []string{"kids"}}}); err != nil {
		t.Fatal(err)
	}
	late := &v1alpha1.Policy{ObjectMeta: meta("late"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"kids", "lonely"}}}
	if _, err := f.st.Apply(ctx, late, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	kids := f.policy("kids")
	kids.Spec.ClientGroupRefs = []string{"default"}
	if _, err := f.st.Apply(ctx, kids, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: kids.ResourceVersion}); err != nil {
		t.Fatal(err)
	}
	n, err := f.svc.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// late: ui-allow and ui-deny-kids; kids: loses ui-deny-kids.
	if n != 3 {
		t.Errorf("Reconcile changed %d references, want 3", n)
	}
	if p := f.policy("late"); !slices.Contains(p.Spec.AllowListRefs, AllowList) || !slices.Contains(p.Spec.BlockListRefs, "ui-deny-kids") {
		t.Errorf("late policy refs %+v", p.Spec)
	}
	if p := f.policy("kids"); slices.Contains(p.Spec.BlockListRefs, "ui-deny-kids") || !slices.Contains(p.Spec.AllowListRefs, AllowList) {
		t.Errorf("kids policy refs %+v", p.Spec)
	}
	if p := f.policy("gitops"); len(p.Spec.AllowListRefs)+len(p.Spec.BlockListRefs) != 0 {
		t.Errorf("a file policy was edited: %+v", p.Spec)
	}
	if n, err := f.svc.Reconcile(ctx); err != nil || n != 0 {
		t.Errorf("second Reconcile = %d, %v", n, err)
	}
}
