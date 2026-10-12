package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func domainsHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.seed(store.ManagerAPI,
		&v1alpha1.ClientGroup{ObjectMeta: objectMeta("default", "kids"), Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{CIDR: "10.0.20.0/24"}}}},
		&v1alpha1.ClientGroup{ObjectMeta: objectMeta("default", "guests"), Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{CIDR: "10.0.30.0/24"}}}},
		&v1alpha1.Policy{ObjectMeta: objectMeta("default", "kids"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"kids"}}},
	)
	h.seed(store.ManagerBootstrap,
		&v1alpha1.ClientGroup{ObjectMeta: objectMeta("default", "default"), Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{CIDR: "0.0.0.0/0"}}}},
		&v1alpha1.Policy{ObjectMeta: objectMeta("default", "default"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}}})
	h.seed("file", &v1alpha1.Policy{ObjectMeta: objectMeta("default", "gitops"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}}})
	return h
}

func TestAllowlistAPI(t *testing.T) {
	h := domainsHarness(t)
	var res DomainWriteResult
	h.do(http.MethodPost, "/api/v1/allowlist", "viewer", map[string]any{"values": []string{"a.example"}}).want(http.StatusForbidden)
	h.do(http.MethodPost, "/api/v1/allowlist", "operator", map[string]any{
		"values": []string{"Ads.Example.net", "*.bad.example"}, "comment": "needed for work", "origin": "querylog",
	}).want(http.StatusOK).json(fresh(&res))
	if len(res.Processed.Success) != 1 || len(res.Processed.Errors) != 1 || !strings.Contains(res.Processed.Errors[0].Error, "wildcard") {
		t.Fatalf("processed %+v", res.Processed)
	}
	if !slices.Equal(res.Attached, []string{"default/default", "default/kids"}) || !slices.Equal(res.Skipped, []string{"default/gitops"}) {
		t.Fatalf("attached %v skipped %v", res.Attached, res.Skipped)
	}
	if len(res.Items) != 1 {
		t.Fatalf("items %+v", res.Items)
	}
	e := res.Items[0]
	if e.Value != "ads.example.net" || e.Type != "exact" || e.Rule != "ads.example.net" || !e.Enabled || !e.Everyone || e.Comment != "needed for work" ||
		e.Origin != "querylog" || e.CreatedBy == "" || e.Created == nil || len(e.Lists) != 1 || e.Lists[0].List != "default/ui-allow" ||
		!slices.Equal(e.Policies, []string{"default/default", "default/kids"}) || e.ReadOnly || e.Action != "allow" {
		t.Fatalf("entry %+v", e)
	}
	id := e.ID

	var list DomainEntries
	h.do(http.MethodGet, "/api/v1/allowlist", "viewer", nil).want(http.StatusOK).json(fresh(&list))
	if len(list.Items) != 1 || list.Total != 1 || len(list.Lists) != 1 || len(list.Unattached) != 1 || list.Unattached[0].Policy != "default/gitops" {
		t.Fatalf("list %+v", list)
	}
	var one DomainEntry
	h.do(http.MethodGet, "/api/v1/allowlist/"+id, "viewer", nil).want(http.StatusOK).json(&one)
	if one.ID != id {
		t.Fatalf("get %+v", one)
	}
	h.do(http.MethodGet, "/api/v1/denylist/"+id, "viewer", nil).want(http.StatusNotFound)
	h.do(http.MethodGet, "/api/v1/allowlist/garbage!", "viewer", nil).want(http.StatusNotFound)

	// Deny: a wildcard for one group; regex for everyone.
	h.do(http.MethodPost, "/api/v1/denylist", "operator", map[string]any{"values": []string{"games.example"}, "type": "wildcard", "groups": []string{"default/kids"}}).
		want(http.StatusOK).json(fresh(&res))
	if len(res.Items) != 1 || res.Items[0].Value != "*.games.example" || res.Items[0].Everyone || !slices.Equal(res.Items[0].Groups, []string{"default/kids"}) ||
		!slices.Equal(res.Attached, []string{"default/kids"}) {
		t.Fatalf("deny group %+v", res)
	}
	wild := res.Items[0].ID
	h.do(http.MethodPost, "/api/v1/denylist", "operator", map[string]any{"values": []string{`/^ads?[0-9]*\./`}, "type": "regex"}).want(http.StatusOK).json(fresh(&res))
	if res.Items[0].Value != `^ads?[0-9]*\.` || res.Items[0].Rule != `/^ads?[0-9]*\./` {
		t.Fatalf("regex %+v", res.Items[0])
	}
	if r := h.do(http.MethodPost, "/api/v1/denylist", "operator", map[string]any{"values": []string{"x.example"}, "groups": []string{"guests"}}).want(http.StatusUnprocessableEntity); r.errKey() != KeyUnprocessable {
		t.Fatalf("uncovered group: %s", r.errKey())
	}
	h.do(http.MethodPost, "/api/v1/denylist", "operator", map[string]any{"values": []string{"x.example"}, "groups": []string{"nope"}}).want(http.StatusUnprocessableEntity)

	h.do(http.MethodGet, "/api/v1/denylist?scope=kids", "viewer", nil).want(http.StatusOK).json(fresh(&list))
	if len(list.Items) != 1 || list.Items[0].ID != wild || list.Matched != 1 || list.Total != 2 {
		t.Fatalf("scope filter %+v", list)
	}
	h.do(http.MethodGet, "/api/v1/denylist?scope=everyone&type=regex", "viewer", nil).want(http.StatusOK).json(fresh(&list))
	if len(list.Items) != 1 || list.Items[0].Type != "regex" {
		t.Fatalf("everyone filter %+v", list)
	}
	h.do(http.MethodGet, "/api/v1/domains?limit=2", "viewer", nil).want(http.StatusOK).json(fresh(&list))
	if list.Total != 3 || len(list.Items) != 2 || list.Continue == "" || len(list.Lists) != 3 {
		t.Fatalf("domains %+v", list)
	}
	h.do(http.MethodGet, "/api/v1/domains?continue="+list.Continue, "viewer", nil).want(http.StatusOK).json(fresh(&list))
	if len(list.Items) != 1 || list.Continue != "" {
		t.Fatalf("domains page 2 %+v", list)
	}
	h.do(http.MethodGet, "/api/v1/domains?action=allow&q=WORK", "viewer", nil).want(http.StatusOK).json(fresh(&list))
	if len(list.Items) != 1 || list.Items[0].Action != "allow" {
		t.Fatalf("comment search %+v", list)
	}

	// Switch off, comment, then move the scope.
	h.do(http.MethodPatch, "/api/v1/allowlist/"+id, "operator", map[string]any{"enabled": false, "comment": "paused"}).want(http.StatusOK).json(fresh(&res))
	if len(res.Items) != 1 || res.Items[0].Enabled || res.Items[0].Comment != "paused" || res.Items[0].Modified == nil {
		t.Fatalf("patch %+v", res.Items)
	}
	h.do(http.MethodGet, "/api/v1/allowlist?enabled=false", "viewer", nil).want(http.StatusOK).json(fresh(&list))
	if len(list.Items) != 1 {
		t.Fatalf("enabled filter %+v", list)
	}
	h.do(http.MethodPatch, "/api/v1/allowlist/"+id, "operator", map[string]any{"scope": "groups"}).want(http.StatusBadRequest)
	h.do(http.MethodPatch, "/api/v1/allowlist/"+id, "operator", map[string]any{"scope": "groups", "groups": []string{"kids"}}).want(http.StatusOK).json(fresh(&res))
	if it := res.Items[0]; it.Everyone || !slices.Equal(it.Groups, []string{"default/kids"}) || it.Lists[0].List != "default/ui-allow-kids" || it.Comment != "paused" {
		t.Fatalf("scope move %+v", it)
	}
	var pol v1alpha1.Policy
	if err := h.st.Get(context.Background(), store.Key{Kind: "Policy", Namespace: "default", Name: "default"}, &pol); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(pol.Spec.AllowListRefs, "ui-allow") {
		t.Fatalf("the emptied ui-allow is still referenced: %v", pol.Spec.AllowListRefs)
	}
	h.do(http.MethodPatch, "/api/v1/allowlist/"+id, "operator", map[string]any{"type": "wildcard"}).want(http.StatusOK).json(fresh(&res))
	if res.Items[0].Value != "*.ads.example.net" {
		t.Fatalf("type change %+v", res.Items[0])
	}
	id = res.Items[0].ID

	// Batch.
	var batch DomainWriteResult
	h.do(http.MethodPost, "/api/v1/allowlist:batch", "operator", map[string]any{"ids": []string{id, "nope"}, "op": "enable"}).want(http.StatusOK).json(fresh(&batch))
	if len(batch.Processed.Success) != 1 || len(batch.Processed.Errors) != 1 || len(batch.Items) != 1 || !batch.Items[0].Enabled {
		t.Fatalf("batch enable %+v", batch)
	}
	h.do(http.MethodPost, "/api/v1/allowlist:batch", "operator", map[string]any{"ids": []string{id}, "op": "delete"}).want(http.StatusOK).json(fresh(&batch))
	h.do(http.MethodGet, "/api/v1/allowlist/"+id, "viewer", nil).want(http.StatusNotFound)
	h.do(http.MethodDelete, "/api/v1/denylist/"+wild, "operator", nil).want(http.StatusNoContent)
	h.do(http.MethodDelete, "/api/v1/denylist/"+wild, "operator", nil).want(http.StatusNotFound)

	found := false
	for _, ev := range h.audit.Events() {
		if ev.Action == "allowlist.add" && ev.Resource.Name == "ui-allow" {
			found = true
		}
	}
	if !found {
		t.Error("no allowlist.add audit event")
	}
}

// TestDomainsReadOnly: entries of a file-managed list are listed read-only
// and refuse changes.
func TestDomainsReadOnly(t *testing.T) {
	h := domainsHarness(t)
	h.seed("file", &v1alpha1.BlockList{ObjectMeta: objectMeta("default", "ui-deny"), Spec: v1alpha1.BlockListSpec{Mode: "block", Sources: []v1alpha1.ListSource{
		{Name: "ui", Inline: []string{"legacy.example"}},
	}}})
	var list DomainEntries
	h.do(http.MethodGet, "/api/v1/denylist", "viewer", nil).want(http.StatusOK).json(fresh(&list))
	if len(list.Items) != 1 || !list.Items[0].ReadOnly || list.Items[0].Origin != "migrated" || list.Items[0].Lists[0].ManagedBy != "file" {
		t.Fatalf("list %+v", list.Items)
	}
	if r := h.do(http.MethodPatch, "/api/v1/denylist/"+list.Items[0].ID, "operator", map[string]any{"enabled": false}).want(http.StatusConflict); r.errKey() != KeyManagedBy {
		t.Fatalf("error key %s", r.errKey())
	}
}

// fresh zeroes *v before a JSON decode reuses it (decoding into a used
// slice keeps fields the new document omits).
func fresh[T any](v *T) *T {
	var zero T
	*v = zero
	return v
}
