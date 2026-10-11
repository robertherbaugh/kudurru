package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func starterList(n int) *v1alpha1.BlockList {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("host%04d.example", i)
	}
	return &v1alpha1.BlockList{ObjectMeta: objectMeta("default", "starter"), Spec: v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{
		{Name: "feed", URL: "https://lists.example/hosts.txt"},
		{Name: "stevenblack-adhoc", Format: "domains", Inline: lines},
	}}}
}

func TestInlineEntriesPaging(t *testing.T) {
	h := newHarness(t)
	h.seed(store.ManagerAPI, starterList(250))
	base := "/api/v1/blocklists/default/starter/sources/stevenblack-adhoc/entries"

	var page InlineEntries
	r := h.do(http.MethodGet, base, "viewer", nil).want(http.StatusOK)
	r.json(&page)
	if len(page.Items) != 100 || page.Total != 250 || page.Matched != 250 || page.Continue == "" || r.Header().Get("ETag") == "" {
		t.Fatalf("first page: %d items, total %d, matched %d, continue %q", len(page.Items), page.Total, page.Matched, page.Continue)
	}
	if page.Items[0] != (InlineEntry{Value: "host0000.example", Line: 1}) {
		t.Fatalf("first item %+v", page.Items[0])
	}
	var all []InlineEntry
	all = append(all, page.Items...)
	for page.Continue != "" {
		var next InlineEntries
		h.do(http.MethodGet, base+"?continue="+page.Continue, "viewer", nil).want(http.StatusOK).json(&next)
		all = append(all, next.Items...)
		page = next
	}
	if len(all) != 250 || all[249].Line != 250 {
		t.Fatalf("paged %d entries, last %+v", len(all), all[len(all)-1])
	}

	// Search, and the source by index.
	h.do(http.MethodGet, "/api/v1/blocklists/default/starter/sources/1/entries?q=HOST01&limit=5", "viewer", nil).want(http.StatusOK).json(&page)
	if page.Matched != 100 || len(page.Items) != 5 || page.Items[0].Value != "host0100.example" || page.Items[0].Line != 101 || page.Continue == "" {
		t.Fatalf("search page %+v", page)
	}

	h.do(http.MethodGet, "/api/v1/blocklists/default/starter/sources/feed/entries", "viewer", nil).want(http.StatusUnprocessableEntity)
	h.do(http.MethodGet, "/api/v1/blocklists/default/starter/sources/nope/entries", "viewer", nil).want(http.StatusNotFound)
	h.do(http.MethodGet, "/api/v1/blocklists/default/missing/sources/x/entries", "viewer", nil).want(http.StatusNotFound)
	h.do(http.MethodGet, base+"?continue=!!", "viewer", nil).want(http.StatusBadRequest)
}

func TestInlineEntriesPatch(t *testing.T) {
	h := newHarness(t)
	h.seed(store.ManagerAPI, starterList(3))
	base := "/api/v1/blocklists/default/starter/sources/stevenblack-adhoc/entries"
	get := func() *v1alpha1.BlockList {
		var bl v1alpha1.BlockList
		if err := h.st.Get(context.Background(), store.Key{Kind: "BlockList", Namespace: "default", Name: "starter"}, &bl); err != nil {
			t.Fatal(err)
		}
		return &bl
	}
	rv := get().ResourceVersion

	h.do(http.MethodPatch, base, "viewer", map[string]any{"add": []string{"x.example"}}).want(http.StatusForbidden)
	h.do(http.MethodPatch, base, "operator", map[string]any{}).want(http.StatusBadRequest)
	h.do(http.MethodPatch, base, "operator", map[string]any{"add": []string{"x.example"}}, "If-Match", "999999").want(http.StatusPreconditionFailed)
	h.do(http.MethodPatch, base, "operator", map[string]any{"add": []string{"not a domain!"}}).want(http.StatusBadRequest)
	h.do(http.MethodPatch, base, "operator", map[string]any{"add": []string{"a.example\nb.example"}}).want(http.StatusBadRequest)

	var res InlineEntriesResult
	r := h.do(http.MethodPatch, base, "operator", map[string]any{
		"add": []string{"new.example", "host0001.example", "new.example"}, "remove": []string{"host0000.example", "absent.example"},
	}, "If-Match", rv).want(http.StatusOK)
	r.json(&res)
	if !slices.Equal(res.Added, []string{"new.example"}) || !slices.Equal(res.Removed, []string{"host0000.example"}) || res.Total != 3 ||
		res.ResourceVersion == rv || r.Header().Get("ETag") != res.ResourceVersion {
		t.Fatalf("patch result %+v (ETag %q)", res, r.Header().Get("ETag"))
	}
	bl := get()
	if got := bl.Spec.Sources[1].Inline; !slices.Equal(got, []string{"host0001.example", "host0002.example", "new.example"}) {
		t.Fatalf("inline = %v", got)
	}
	if bl.Spec.Sources[0].URL == "" || store.ManagedBy(bl) != store.ManagerAPI {
		t.Fatalf("the rest of the object changed: %+v", bl.Spec.Sources[0])
	}

	// A no-op patch writes nothing.
	h.do(http.MethodPatch, base, "operator", map[string]any{"add": []string{"new.example"}}).want(http.StatusOK).json(&res)
	if len(res.Added) != 0 || res.ResourceVersion != bl.ResourceVersion {
		t.Fatalf("no-op %+v", res)
	}
	// Removing every entry is refused: switch the source off instead.
	h.do(http.MethodPatch, base, "operator", map[string]any{"remove": []string{"host0001.example", "host0002.example", "new.example"}}).
		want(http.StatusUnprocessableEntity)
	h.do(http.MethodPatch, "/api/v1/blocklists/default/starter/sources/feed/entries", "operator", map[string]any{"add": []string{"x.example"}}).
		want(http.StatusUnprocessableEntity)
}

func TestInlineEntriesManagedBy(t *testing.T) {
	h := newHarness(t)
	h.seed("file", starterList(2))
	r := h.do(http.MethodPatch, "/api/v1/blocklists/default/starter/sources/stevenblack-adhoc/entries", "operator",
		map[string]any{"add": []string{"x.example"}}).want(http.StatusConflict)
	if r.errKey() != KeyManagedBy {
		t.Fatalf("error key %q", r.errKey())
	}
	// Bootstrap defaults are taken over, as by PUT.
	h2 := newHarness(t)
	h2.seed(store.ManagerBootstrap, starterList(2))
	h2.do(http.MethodPatch, "/api/v1/blocklists/default/starter/sources/stevenblack-adhoc/entries", "operator",
		map[string]any{"add": []string{"x.example"}}).want(http.StatusOK)
}
