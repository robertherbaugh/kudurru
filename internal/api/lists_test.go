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

func TestBlockListCRUD(t *testing.T) {
	h := newHarness(t)
	doc := map[string]any{
		"apiVersion": "kudurru.io/v1alpha1", "kind": "BlockList",
		"metadata": map[string]any{"name": "ads", "namespace": "home"},
		"spec":     map[string]any{"sources": []any{map[string]any{"inline": []any{"ads.example"}}}},
	}
	r := h.do(http.MethodPost, "/api/v1/blocklists", "operator", doc).want(http.StatusCreated)
	if r.Header().Get("Location") != "/api/v1/objects/blocklists/home/ads" {
		t.Fatalf("Location %q", r.Header().Get("Location"))
	}
	h.do(http.MethodPost, "/api/v1/blocklists", "operator", doc).want(http.StatusConflict)
	h.do(http.MethodGet, "/api/v1/blocklists/home/ads", "viewer", nil).want(http.StatusOK)
	var list ObjectList
	h.do(http.MethodGet, "/api/v1/blocklists?namespace=home", "viewer", nil).want(http.StatusOK).json(&list)
	if len(list.Items) != 1 {
		t.Fatalf("list %+v", list)
	}
	h.do(http.MethodPut, "/api/v1/blocklists/home/ads", "viewer", doc).want(http.StatusForbidden)
	h.do(http.MethodDelete, "/api/v1/blocklists/home/ads", "operator", nil).want(http.StatusNoContent)

	// Client groups and policies have the same routes.
	cg := map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "ClientGroup", "metadata": map[string]any{"name": "kids"},
		"spec": map[string]any{"clients": []any{map[string]any{"cidr": "192.168.1.0/24"}}}}
	h.do(http.MethodPost, "/api/v1/clientgroups", "operator", cg).want(http.StatusCreated)
	h.do(http.MethodGet, "/api/v1/clientgroups/default/kids", "viewer", nil).want(http.StatusOK)
	var p Processed
	h.do(http.MethodPost, "/api/v1/clientgroups:batchDelete", "operator", map[string]any{"names": []string{"kids"}}).want(http.StatusOK).json(&p)
	if len(p.Processed.Success) != 1 || p.Processed.Success[0].Item != "default/kids" {
		t.Fatalf("batch %+v", p)
	}
}

func TestAddDomains(t *testing.T) {
	h := newHarness(t)
	// The default policy (as bootstrap seeds it) gets the UI lists.
	h.seed(store.ManagerBootstrap, &v1alpha1.Policy{ObjectMeta: objectMeta("default", "default"), Spec: v1alpha1.PolicySpec{}})
	var p Processed
	h.do(http.MethodPost, "/api/v1/domains/deny/exact", "operator",
		map[string]any{"domains": []string{"Ads.Example.com.", "not a domain", "tracker.example"}}).want(http.StatusOK).json(&p)
	if len(p.Processed.Success) != 2 || len(p.Processed.Errors) != 1 || p.Processed.Errors[0].Item != "not a domain" {
		t.Fatalf("processed %+v", p)
	}
	h.do(http.MethodPost, "/api/v1/domains/deny/regex", "operator",
		map[string]any{"domains": []string{`^ad[0-9]+\.`, `(`}}).want(http.StatusOK).json(&p)
	if len(p.Processed.Success) != 1 || len(p.Processed.Errors) != 1 {
		t.Fatalf("regex processed %+v", p)
	}
	h.do(http.MethodPost, "/api/v1/domains/allow/exact", "operator", map[string]any{"domains": []string{"ok.example"}}).want(http.StatusOK)

	var deny v1alpha1.BlockList
	if err := h.st.Get(context.Background(), store.Key{Kind: "BlockList", Namespace: "default", Name: UIDenyList}, &deny); err != nil {
		t.Fatal(err)
	}
	got := deny.Spec.Sources[0].Inline
	if deny.Spec.Mode != "block" || !slices.Equal(got, []string{"ads.example.com", "tracker.example", `/^ad[0-9]+\./`}) {
		t.Fatalf("ui-deny %+v", deny.Spec)
	}
	var allow v1alpha1.BlockList
	if err := h.st.Get(context.Background(), store.Key{Kind: "BlockList", Namespace: "default", Name: UIAllowList}, &allow); err != nil || allow.Spec.Mode != "allow" {
		t.Fatalf("ui-allow %v %+v", err, allow.Spec)
	}
	var pol v1alpha1.Policy
	if err := h.st.Get(context.Background(), store.Key{Kind: "Policy", Namespace: "default", Name: "default"}, &pol); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pol.Spec.BlockListRefs, UIDenyList) || !slices.Contains(pol.Spec.AllowListRefs, UIAllowList) {
		t.Fatalf("default policy refs %+v", pol.Spec)
	}
	// Re-adding is idempotent.
	h.do(http.MethodPost, "/api/v1/domains/deny/exact", "operator", map[string]any{"domains": []string{"ads.example.com"}}).want(http.StatusOK)
	_ = h.st.Get(context.Background(), store.Key{Kind: "BlockList", Namespace: "default", Name: UIDenyList}, &deny)
	if len(deny.Spec.Sources[0].Inline) != 3 {
		t.Fatalf("duplicate entry added: %v", deny.Spec.Sources[0].Inline)
	}

	// Search falls back to inline entries without a compiled snapshot.
	var hits struct {
		Items []ListSearchHit `json:"items"`
	}
	h.do(http.MethodGet, "/api/v1/blocklists/_search?domain=ads.example.com", "viewer", nil).want(http.StatusOK).json(&hits)
	if len(hits.Items) != 1 || hits.Items[0].List != "default/ui-deny" {
		t.Fatalf("search %+v", hits)
	}
	h.do(http.MethodGet, "/api/v1/blocklists/_search?domain=ad42.example.com", "viewer", nil).want(http.StatusOK).json(&hits)
	if len(hits.Items) != 1 || hits.Items[0].Rule != `/^ad[0-9]+\./` {
		t.Fatalf("regex search %+v", hits)
	}
}

func TestRefreshList(t *testing.T) {
	var refreshed []string
	h := newHarness(t, func(o *Options) {
		o.Refresher = RefreshFunc(func(_ context.Context, ns, name string) error {
			refreshed = append(refreshed, ns+"/"+name)
			return nil
		})
	})
	h.seed("api", &v1alpha1.BlockList{ObjectMeta: objectMeta("default", "ads"), Spec: v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{URL: "https://lists.example/ads.txt"}}}})
	h.do(http.MethodPost, "/api/v1/blocklists/default/ads/refresh", "operator", nil).want(http.StatusAccepted)
	h.do(http.MethodPost, "/api/v1/blocklists/default/missing/refresh", "operator", nil).want(http.StatusNotFound)
	h.do(http.MethodPost, "/api/v1/blocklists/default/ads/refresh", "viewer", nil).want(http.StatusForbidden)
	if len(refreshed) != 1 || refreshed[0] != "default/ads" {
		t.Fatalf("refreshed %v", refreshed)
	}
}

func TestRefreshListOnReplica(t *testing.T) {
	leader := "node-b"
	h := newHarness(t, func(o *Options) {
		o.Refresher = RefreshFunc(func(context.Context, string, string) error { return ErrNotLeader(leader) })
	})
	h.seed("api", &v1alpha1.BlockList{ObjectMeta: objectMeta("default", "ads"), Spec: v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{URL: "https://lists.example/ads.txt"}}}})
	r := h.do(http.MethodPost, "/api/v1/blocklists/default/ads/refresh", "operator", nil).want(http.StatusConflict)
	if r.errKey() != KeyConflict || !strings.Contains(r.Body.String(), "node-b") {
		t.Fatalf("replica: %s", r.Body.String())
	}
	leader = ""
	h.do(http.MethodPost, "/api/v1/blocklists/default/ads/refresh", "operator", nil).want(http.StatusServiceUnavailable)
}
