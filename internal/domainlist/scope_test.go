package domainlist

import (
	"context"
	"slices"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestGroupScopeMaterializedForLegacyAndApproval(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	legacy := &public.BlockList{ObjectMeta: meta("ui-deny-kids"), Spec: public.BlockListSpec{Mode: "block", Sources: []public.ListSource{{Name: "ui", Entries: []public.ListEntry{{Value: "manual.example", Comment: "preserve"}}}}}}
	if _, err := f.st.Apply(ctx, legacy, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	n, err := f.svc.Migrate(ctx)
	if err != nil || n != 1 {
		t.Fatal("legacy structured scope not materialized", n, err)
	}
	got := f.list(legacy.Name)
	if !slices.Equal(got.Spec.ClientGroupRefs, []string{"kids"}) || got.Spec.Sources[0].Entries[0].Comment != "preserve" {
		t.Fatal("scope migration lost entry")
	}
	if n, err := f.svc.Migrate(ctx); err != nil || n != 0 {
		t.Fatal("scope migration not idempotent", n, err)
	}
	key := store.Key{Kind: "BlockList", Namespace: "default", Name: "ui-allow-kids"}
	approval := public.ListEntry{Value: "approved.example", Origin: public.OriginUnblock, Request: "default/request"}
	edit, err := PlanApprovalEntry(nil, key, approval, false)
	if err != nil || !edit.Added || !edit.Changed || !slices.Equal(edit.Desired.Spec.ClientGroupRefs, []string{"kids"}) {
		t.Fatal("current approval omitted explicit exact scope", err)
	}
	revoked, err := PlanApprovalEntry(edit.Desired, key, approval, true)
	if err != nil || !revoked.Changed || revoked.Desired != nil {
		t.Fatal("exact scope changed approval removal ownership", err)
	}
}
