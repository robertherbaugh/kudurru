package api

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// TestListExceptions: entries switched off in a block list appear on the
// Allowlist (and in an allow list on the Denylist) as exceptions whose
// switch, delete and batch edit the exclusion.
func TestListExceptions(t *testing.T) {
	body := "0.0.0.0 ads.example tracker.example\n||cdn.example^\n"
	h := newHarness(t, func(o *Options) { o.ListBodies = fakeBodies{"https://lists.example/hosts.txt": body} })
	h.seed(store.ManagerAPI,
		&v1alpha1.BlockList{ObjectMeta: objectMeta("default", "feed"), Spec: v1alpha1.BlockListSpec{Mode: "block", Sources: []v1alpha1.ListSource{
			{Name: "remote", URL: "https://lists.example/hosts.txt", Exclude: []string{"tracker.example"}},
			{Name: "mine", Inline: []string{"own.example", "0.0.0.0 cdn.example"}},
		}}},
		&v1alpha1.BlockList{ObjectMeta: objectMeta("default", "good"), Spec: v1alpha1.BlockListSpec{Mode: "allow", Sources: []v1alpha1.ListSource{
			{Name: "ok", Inline: []string{"ok.example"}, Exclude: []string{"ok.example"}},
		}}},
		&v1alpha1.Policy{ObjectMeta: objectMeta("default", "default"), Spec: v1alpha1.PolicySpec{BlockListRefs: []string{"feed"}}},
	)
	list := func(path string) DomainEntries {
		var out DomainEntries
		h.do(http.MethodGet, path, "viewer", nil).want(http.StatusOK).json(&out)
		return out
	}
	allow := list("/api/v1/allowlist")
	if len(allow.Items) != 1 || allow.Total != 1 {
		t.Fatalf("allowlist %+v", allow)
	}
	e := allow.Items[0]
	if e.Kind != "exception" || e.Value != "tracker.example" || e.Type != "exact" || !e.Enabled || e.Origin != "list" || e.Everyone ||
		len(e.Lists) != 1 || e.Lists[0].List != "default/feed" || e.Lists[0].Source != "remote" || !slices.Equal(e.Policies, []string{"default/default"}) ||
		len(e.Overrides) != 1 || e.Overrides[0].List != "default/feed" {
		t.Fatalf("exception %+v", e)
	}
	if d := list("/api/v1/denylist"); len(d.Items) != 1 || d.Items[0].Kind != "exception" || d.Items[0].Lists[0].List != "default/good" {
		t.Fatalf("denylist %+v", d)
	}
	if got := list("/api/v1/domains?kind=entry"); len(got.Items) != 0 {
		t.Fatalf("kind=entry %+v", got.Items)
	}
	if got := list("/api/v1/domains?kind=exception&scope=list:feed"); len(got.Items) != 1 {
		t.Fatalf("list scope %+v", got.Items)
	}
	if got := list("/api/v1/allowlist?q=FEED"); len(got.Items) != 1 {
		t.Fatalf("search by list %+v", got.Items)
	}

	stored := func() []string {
		var bl v1alpha1.BlockList
		if err := h.st.Get(context.Background(), store.Key{Kind: "BlockList", Namespace: "default", Name: "feed"}, &bl); err != nil {
			t.Fatal(err)
		}
		return append(slices.Clone(bl.Spec.Sources[0].Exclude), bl.Spec.Sources[1].Exclude...)
	}
	path := "/api/v1/allowlist/" + e.ID
	h.do(http.MethodPatch, path, "viewer", map[string]any{"enabled": false}).want(http.StatusForbidden)
	h.do(http.MethodPatch, path, "operator", map[string]any{"comment": "x"}).want(http.StatusUnprocessableEntity)
	h.do(http.MethodPatch, "/api/v1/denylist/"+e.ID, "operator", map[string]any{"enabled": false}).want(http.StatusNotFound)
	var res DomainWriteResult
	h.do(http.MethodPatch, path, "operator", map[string]any{"enabled": false}).want(http.StatusOK).json(fresh(&res))
	if len(res.Items) != 1 || res.Items[0].Enabled || res.Items[0].ID != e.ID || len(stored()) != 0 {
		t.Fatalf("switched off: %+v, exclude %v", res.Items, stored())
	}
	var one DomainEntry
	h.do(http.MethodGet, path, "viewer", nil).want(http.StatusOK).json(&one)
	if one.Enabled || one.Kind != "exception" {
		t.Fatalf("get after off %+v", one)
	}
	if got := list("/api/v1/allowlist"); len(got.Items) != 0 {
		t.Fatalf("a switched-back entry is still listed: %+v", got.Items)
	}
	h.do(http.MethodPatch, path, "operator", map[string]any{"enabled": true}).want(http.StatusOK).json(fresh(&res))
	if !res.Items[0].Enabled || !slices.Equal(stored(), []string{"tracker.example"}) {
		t.Fatalf("switched on: %+v %v", res.Items, stored())
	}

	// Add exceptions from the Allowlist: every source that has the name.
	h.do(http.MethodPost, "/api/v1/allowlist", "operator", map[string]any{"values": []string{"cdn.example", "nothere.example"}, "list": "feed"}).
		want(http.StatusOK).json(fresh(&res))
	if len(res.Processed.Success) != 1 || len(res.Processed.Errors) != 1 || len(res.Items) != 2 {
		t.Fatalf("create %+v", res)
	}
	if got := stored(); !slices.Equal(got, []string{"tracker.example", "||cdn.example^", "cdn.example"}) {
		t.Fatalf("exclude after create %v", got)
	}
	h.do(http.MethodPost, "/api/v1/allowlist", "operator", map[string]any{"values": []string{"cdn.example"}, "list": "good"}).want(http.StatusUnprocessableEntity)
	h.do(http.MethodPost, "/api/v1/allowlist", "operator", map[string]any{"values": []string{"cdn.example"}, "list": "feed", "groups": []string{"kids"}}).want(http.StatusBadRequest)

	// Batch off, then delete.
	ids := []string{}
	for _, it := range list("/api/v1/allowlist").Items {
		ids = append(ids, it.ID)
	}
	h.do(http.MethodPost, "/api/v1/allowlist:batch", "operator", map[string]any{"ids": ids, "op": "disable"}).want(http.StatusOK).json(fresh(&res))
	if len(res.Processed.Success) != 3 || len(stored()) != 0 {
		t.Fatalf("batch %+v %v", res.Processed, stored())
	}
	h.do(http.MethodPatch, path, "operator", map[string]any{"enabled": true}).want(http.StatusOK)
	h.do(http.MethodDelete, path, "operator", nil).want(http.StatusNoContent)
	if len(stored()) != 0 {
		t.Fatalf("delete left %v", stored())
	}
}
