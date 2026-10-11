package api

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

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

	var page SourceEntries
	r := h.do(http.MethodGet, base, "viewer", nil).want(http.StatusOK)
	r.json(&page)
	if len(page.Items) != 100 || page.Total != 250 || page.Matched != 250 || page.Continue == "" || r.Header().Get("ETag") == "" {
		t.Fatalf("first page: %d items, total %d, matched %d, continue %q", len(page.Items), page.Total, page.Matched, page.Continue)
	}
	if page.Items[0] != (SourceEntry{Value: "host0000.example", Line: 1, Enabled: true}) {
		t.Fatalf("first item %+v", page.Items[0])
	}
	var all []SourceEntry
	all = append(all, page.Items...)
	for page.Continue != "" {
		var next SourceEntries
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

	// A URL source without a body on this node (no ListBodies here).
	if r := h.do(http.MethodGet, "/api/v1/blocklists/default/starter/sources/feed/entries", "viewer", nil).want(http.StatusServiceUnavailable); r.errKey() != KeyUnavailable {
		t.Fatalf("error key %q", r.errKey())
	}
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

	var res SourceEntriesResult
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

// fakeBodies serves URL source bodies from memory.
type fakeBodies map[string]string

func (f fakeBodies) OpenSourceBody(_ string, src v1alpha1.ListSource) (io.ReadCloser, SourceBody, error) {
	body, ok := f[src.URL]
	if !ok {
		return nil, SourceBody{}, fs.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(body)), SourceBody{Digest: "d1", Fetched: time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC), Size: int64(len(body))}, nil
}

// TestURLSourceEntries: the entries of a fetched body are listed from the
// node's cache, switched off and on through spec.sources[].exclude, and
// stale exclusions are reported.
func TestURLSourceEntries(t *testing.T) {
	body := "# a hosts list\n0.0.0.0 ads.example tracker.example\n||cdn.example^\n0.0.0.0 more.example\n"
	h := newHarness(t, func(o *Options) { o.ListBodies = fakeBodies{"https://lists.example/hosts.txt": body} })
	bl := starterList(2)
	bl.Spec.Sources[0].Exclude = []string{"gone.example"}
	h.seed(store.ManagerBootstrap, bl)
	var seeded v1alpha1.BlockList
	if err := h.st.Get(context.Background(), store.KeyOf(bl), &seeded); err != nil {
		t.Fatal(err)
	}
	seeded.Status.Sources = []v1alpha1.SourceStatus{{Name: "feed", Digest: "d2"}}
	if err := h.st.UpdateStatus(context.Background(), &seeded); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/blocklists/default/starter/sources/feed/entries"

	var page SourceEntries
	h.do(http.MethodGet, base, "viewer", nil).want(http.StatusOK).json(&page)
	want := []SourceEntry{{"ads.example", 2, true}, {"tracker.example", 2, true}, {"||cdn.example^", 3, true}, {"more.example", 4, true}}
	if !slices.Equal(page.Items, want) || page.Total != 4 || page.Kind != "url" || page.Digest != "d1" || page.Fetched == nil ||
		!page.Stale || !slices.Equal(page.StaleExcludes, []string{"gone.example"}) {
		t.Fatalf("page %+v", page)
	}

	var res SourceEntriesResult
	h.do(http.MethodPatch, base, "viewer", map[string]any{"disable": []string{"tracker.example"}}).want(http.StatusForbidden)
	h.do(http.MethodPatch, base, "operator", map[string]any{"add": []string{"x.example"}}).want(http.StatusUnprocessableEntity)
	h.do(http.MethodPatch, base, "operator", map[string]any{"disable": []string{"not a rule!!"}}).want(http.StatusBadRequest)
	h.do(http.MethodPatch, base, "operator", map[string]any{"disable": []string{"tracker.example", "||cdn.example^"}, "enable": []string{"gone.example"}}).
		want(http.StatusOK).json(&res)
	if !slices.Equal(res.Disabled, []string{"tracker.example", "||cdn.example^"}) || !slices.Equal(res.Enabled, []string{"gone.example"}) || res.Excluded != 2 {
		t.Fatalf("patch %+v", res)
	}
	var stored v1alpha1.BlockList
	if err := h.st.Get(context.Background(), store.Key{Kind: "BlockList", Namespace: "default", Name: "starter"}, &stored); err != nil {
		t.Fatal(err)
	}
	// The starter list is bootstrap-managed: the API takes it over (D84).
	if !slices.Equal(stored.Spec.Sources[0].Exclude, []string{"tracker.example", "||cdn.example^"}) || store.ManagedBy(&stored) != store.ManagerAPI {
		t.Fatalf("stored exclude %v, managed by %q", stored.Spec.Sources[0].Exclude, store.ManagedBy(&stored))
	}

	h.do(http.MethodGet, base+"?enabled=false", "viewer", nil).want(http.StatusOK).json(&page)
	if len(page.Items) != 2 || page.Matched != 2 || page.Excluded != 2 || page.Items[0].Value != "tracker.example" || page.Items[0].Enabled || len(page.StaleExcludes) != 0 {
		t.Fatalf("disabled only %+v", page)
	}
	h.do(http.MethodGet, base+"?q=CDN", "viewer", nil).want(http.StatusOK).json(&page)
	if len(page.Items) != 1 || page.Items[0].Enabled {
		t.Fatalf("search %+v", page)
	}
	h.do(http.MethodPatch, base, "operator", map[string]any{"enable": []string{"tracker.example"}}).want(http.StatusOK).json(&res)
	if res.Excluded != 1 {
		t.Fatalf("enable %+v", res)
	}

	// Inline sources: a line is off when all its rules are.
	h.do(http.MethodPatch, "/api/v1/blocklists/default/starter/sources/stevenblack-adhoc/entries", "operator", map[string]any{"disable": []string{"host0001.example"}}).
		want(http.StatusOK)
	h.do(http.MethodGet, "/api/v1/blocklists/default/starter/sources/stevenblack-adhoc/entries", "viewer", nil).want(http.StatusOK).json(&page)
	if len(page.Items) != 2 || !page.Items[0].Enabled || page.Items[1].Enabled || page.Excluded != 1 || page.Kind != "inline" {
		t.Fatalf("inline %+v", page)
	}
}

// TestStructuredSourceEntries: entries sources list their rules and
// switch through enabled; add and remove belong to the Allowlist API.
func TestStructuredSourceEntries(t *testing.T) {
	h := newHarness(t)
	bl := &v1alpha1.BlockList{ObjectMeta: objectMeta("default", "ui-allow"), Spec: v1alpha1.BlockListSpec{Mode: "allow", Sources: []v1alpha1.ListSource{
		{Name: "ui", Entries: []v1alpha1.ListEntry{{Value: "a.example"}, {Value: "*.b.example", Type: v1alpha1.EntryWildcard}}},
	}}}
	h.seed(store.ManagerAPI, bl)
	base := "/api/v1/blocklists/default/ui-allow/sources/ui/entries"
	var res SourceEntriesResult
	h.do(http.MethodPatch, base, "operator", map[string]any{"disable": []string{"||b.example^"}}).want(http.StatusOK).json(&res)
	if !slices.Equal(res.Disabled, []string{"||b.example^"}) || res.Excluded != 1 || res.Total != 2 {
		t.Fatalf("patch %+v", res)
	}
	var page SourceEntries
	h.do(http.MethodGet, base, "viewer", nil).want(http.StatusOK).json(&page)
	if want := []SourceEntry{{"a.example", 1, true}, {"*.b.example", 2, false}}; !slices.Equal(page.Items, want) || page.Kind != "entries" {
		t.Fatalf("page %+v", page)
	}
	h.do(http.MethodPatch, base, "operator", map[string]any{"remove": []string{"a.example"}}).want(http.StatusUnprocessableEntity)
	h.do(http.MethodPatch, base, "operator", map[string]any{"disable": []string{"c.example"}}).want(http.StatusBadRequest)
}
