package api

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

func capturedListSnapshot(t *testing.T) *blocking.Snapshot {
	t.Helper()
	var lists []*blocking.List
	for _, spec := range []blocking.Spec{
		{Name: "ns/z-block", Mode: blocking.ModeBlock, Sources: []blocking.Source{blocking.LinesSource("inline", parser.FormatAdblock, []string{"||captured.example^", "@@||except.captured.example^"})}},
		{Name: "ns/a-allow", Mode: blocking.ModeAllow, Sources: []blocking.Source{blocking.LinesSource("inline", parser.FormatDomains, []string{"captured.example"})}},
	} {
		list, err := blocking.Compile(spec, nil, blocking.CompileOptions{})
		if err != nil {
			t.Fatal(err)
		}
		lists = append(lists, list)
	}
	snapshot, err := blocking.NewSnapshot(4, lists...)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func prepareListReadObjects(t *testing.T, f *objectReadFixture, lists *blocking.Snapshot, objects ...store.Object) {
	t.Helper()
	snapshot, err := zonecache.Prepare(zonecache.PrepareOptions{Lists: lists}, objects, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.objects = snapshot.Objects
}

func TestCapturedListSearchUsesOriginalCompiledLists(t *testing.T) {
	h, f := newObjectReadFixture(t)
	compiled := capturedListSnapshot(t)
	// Deliberately competing desired inline data must never be recompiled by
	// the search route, even if no compiled result matches the question.
	inline := &public.BlockList{ObjectMeta: objectMeta("default", "inline-only"), Spec: public.BlockListSpec{Sources: []public.ListSource{{Inline: []string{"inline.example"}}}}}
	prepareListReadObjects(t, f, compiled, inline)
	h.seed(store.ManagerAPI, inline)
	var hits List[ListSearchHit]
	h.do(http.MethodGet, "/api/v1/blocklists/_search?domain=CAPTURED.Example.", "viewer", nil).want(http.StatusOK).json(&hits)
	want := []ListSearchHit{{List: "ns/a-allow", Mode: "allow", Rule: "captured.example"}, {List: "ns/z-block", Mode: "block", Rule: "||captured.example^"}}
	if !slices.Equal(hits.Items, want) {
		t.Fatal("compiled modes/rules/order differ", hits.Items)
	}
	hits.Items[0].List = "response-mutated"
	hits.Items[1].Rule = "response-mutated"
	var again List[ListSearchHit]
	h.do(http.MethodGet, "/api/v1/blocklists/_search?domain=captured.example", "viewer", nil).want(http.StatusOK).json(&again)
	if !slices.Equal(again.Items, want) {
		t.Fatal("response mutated retained list data", again.Items)
	}
	h.do(http.MethodGet, "/api/v1/blocklists/_search?domain=inline.example", "viewer", nil).want(http.StatusOK).json(&again)
	if again.Items == nil || len(again.Items) != 0 {
		t.Fatal("inline fallback leaked into captured search", again.Items)
	}
	h.do(http.MethodGet, "/api/v1/blocklists/_search?domain=except.captured.example", "viewer", nil).want(http.StatusOK).json(&again)
	for _, hit := range again.Items {
		if hit.Mode == "block" {
			t.Fatal("compiled exception was lost", again.Items)
		}
	}
	h.do(http.MethodGet, "/api/v1/blocklists/_search?domain=unmatched.invalid", "viewer", nil).want(http.StatusOK).json(&again)
	if again.Items == nil || len(again.Items) != 0 {
		t.Fatal("no hit must be an empty array", again.Items)
	}
}

func TestCapturedListSearchMissingOwnerOrListsDoesNotFallback(t *testing.T) {
	h, f := newObjectReadFixture(t)
	inline := &public.BlockList{ObjectMeta: objectMeta("default", "live-only"), Spec: public.BlockListSpec{Sources: []public.ListSource{{Inline: []string{"captured.example"}}}}}
	h.seed(store.ManagerAPI, inline)
	prepareListReadObjects(t, f, nil, inline)
	h.do(http.MethodGet, "/api/v1/blocklists/_search?domain=captured.example", "viewer", nil).want(http.StatusServiceUnavailable)
	for _, err := range []error{serving.ErrReleased, serving.ErrComponents, context.Canceled, context.DeadlineExceeded} {
		f.err = err
		h.do(http.MethodGet, "/api/v1/blocklists/_search?domain=captured.example", "viewer", nil).want(http.StatusServiceUnavailable)
	}
	f.err = nil
	prepareListReadObjects(t, f, capturedListSnapshot(t))
	request := h.req(http.MethodGet, "/api/v1/blocklists/_search?domain=captured.example", "viewer", nil)
	ctx, cancel := context.WithDeadline(request.Context(), time.Now().Add(-time.Second))
	defer cancel()
	before := f.calls
	h.serve(request.WithContext(ctx)).want(http.StatusServiceUnavailable)
	if f.calls != before {
		t.Fatal("canceled search called the source")
	}
	h.do(http.MethodGet, "/api/v1/blocklists/_search?domain=.", "viewer", nil).want(http.StatusBadRequest)
	h.do(http.MethodGet, "/api/v1/blocklists/_search?domain=captured.example", "zoneadmin", nil).want(http.StatusForbidden)
}

func TestCapturedListSearchNilCapabilityKeepsLegacyInlineSearch(t *testing.T) {
	h := newHarness(t)
	h.seed(store.ManagerAPI, &public.BlockList{ObjectMeta: objectMeta("default", "legacy"), Spec: public.BlockListSpec{Sources: []public.ListSource{{Inline: []string{"legacy.example"}}}}})
	var hits List[ListSearchHit]
	h.do(http.MethodGet, "/api/v1/blocklists/_search?domain=legacy.example", "viewer", nil).want(http.StatusOK).json(&hits)
	if len(hits.Items) != 1 || hits.Items[0].List != "default/legacy" {
		t.Fatal("nil capability changed legacy inline search", hits.Items)
	}
}
