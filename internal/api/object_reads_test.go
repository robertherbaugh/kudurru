package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// Explicit API capability fixture. Real original-lease acceptance is in
// servingbuild, against native HTTP auth and actual SQLite publication.
type objectReadFixture struct {
	objects zonecache.ObjectSet
	base    replication.Generation
	err     error
	calls   int
}

func (f *objectReadFixture) ObjectSnapshot(context.Context) (zonecache.ObjectSet, replication.Generation, error) {
	f.calls++
	return f.objects, f.base, f.err
}

func newObjectReadFixture(t *testing.T, objects ...store.Object) (*harness, *objectReadFixture) {
	t.Helper()
	snapshot, err := zonecache.Prepare(zonecache.PrepareOptions{}, objects, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &objectReadFixture{objects: snapshot.Objects, base: replication.Generation{Term: replication.Term{ClusterID: "cluster", Epoch: 1, PrimaryID: "primary", PromotionID: "promotion"}, Revision: 12, Digest: strings.Repeat("a", 64)}}
	return newHarness(t, func(o *Options) { o.ObjectReads = f }), f
}

func objectReadNames(items []Object) []string {
	var names []string
	for _, obj := range items {
		m := obj["metadata"].(map[string]any)
		names = append(names, m["name"].(string))
	}
	return names
}

func TestCapturedObjectListsAliasesETagsAndDetachedOutput(t *testing.T) {
	z := zoneObj("example", "original.example.")
	z.ResourceVersion = "opaque-original-rv"
	z.Labels = map[string]string{"tier": "edge", "app": "dns"}
	p := &public.Policy{ObjectMeta: objectMeta("apps", "p")}
	b := &public.BlockList{ObjectMeta: objectMeta("apps", "b")}
	c := &public.ClientGroup{ObjectMeta: objectMeta("apps", "c")}
	h, f := newObjectReadFixture(t, z, p, b, c)
	h.seed(store.ManagerAPI, zoneObj("live-only", "live.example."))
	for _, test := range []struct{ path, name string }{
		{"objects/dnszones", "example"}, {"objects/policies", "p"}, {"policies", "p"},
		{"objects/blocklists", "b"}, {"blocklists", "b"}, {"objects/clientgroups", "c"}, {"clientgroups", "c"},
	} {
		var list ObjectList
		h.do(http.MethodGet, "/api/v1/"+test.path, "viewer", nil).want(http.StatusOK).json(&list)
		if !slices.Equal(objectReadNames(list.Items), []string{test.name}) {
			t.Fatalf("%s read a competing live source: %+v", test.path, list)
		}
	}
	for _, path := range []string{"policies/apps/p", "blocklists/apps/b", "clientgroups/apps/c"} {
		h.do(http.MethodGet, "/api/v1/"+path, "viewer", nil).want(http.StatusOK)
	}
	res := h.do(http.MethodGet, "/api/v1/objects/dnszones/example", "viewer", nil).want(http.StatusOK)
	if res.Header().Get("ETag") != z.ResourceVersion {
		t.Fatal("detail ETag did not retain original opaque RV", res.Header())
	}
	res.m()["metadata"].(map[string]any)["labels"].(map[string]any)["app"] = "response-mutated"
	k, _ := kindByPlural("dnszones")
	m, err := objectMap(k, f.objects.List(k.kind)[0])
	if err != nil {
		t.Fatal(err)
	}
	m["metadata"].(map[string]any)["labels"].(map[string]any)["app"] = "direct-output-mutated"
	if f.objects.Zones()[0].Labels["app"] != "dns" {
		t.Fatal("output map changed immutable owner")
	}
	h.do(http.MethodGet, "/api/v1/objects/dnszones/live-only", "viewer", nil).want(http.StatusNotFound)
}

func TestCapturedObjectPaginationCanonicalQueryAndExactGeneration(t *testing.T) {
	var objects []store.Object
	for _, ns := range []string{"a", "b"} {
		for _, name := range []string{"a", "b", "c"} {
			p := &public.Policy{ObjectMeta: objectMeta(ns, name)}
			p.Labels = map[string]string{"app": "dns", "tier": "edge"}
			objects = append(objects, p)
		}
	}
	h, f := newObjectReadFixture(t, objects...)
	path := "/api/v1/objects/policies?namespace=a&limit=2&labelSelector=" + url.QueryEscape("tier=edge,app==dns")
	var first ObjectList
	h.do(http.MethodGet, path, "viewer", nil).want(http.StatusOK).json(&first)
	if !slices.Equal(objectReadNames(first.Items), []string{"a", "b"}) || first.Continue == "" {
		t.Fatal("first page", first)
	}
	var second ObjectList
	h.do(http.MethodGet, "/api/v1/policies?namespace=a&limit=1&labelSelector="+url.QueryEscape("app=dns, tier = edge")+"&continue="+first.Continue, "viewer", nil).want(http.StatusOK).json(&second)
	if !slices.Equal(objectReadNames(second.Items), []string{"c"}) || second.Continue != "" {
		t.Fatal("canonical selector or last page", second)
	}
	for _, changed := range []string{
		"/api/v1/objects/policies?namespace=b", "/api/v1/objects/policies?namespace=a&labelSelector=app=dns",
		"/api/v1/objects/blocklists?namespace=a", "/api/v1/objects/policies?limit=1",
	} {
		h.do(http.MethodGet, changed+"&continue="+first.Continue, "viewer", nil).want(http.StatusConflict)
	}
	for _, change := range []func(*replication.Generation){
		func(g *replication.Generation) { g.Revision++ }, func(g *replication.Generation) { g.Digest = strings.Repeat("b", 64) },
		func(g *replication.Generation) { g.Term.PromotionID = "successor" }, func(g *replication.Generation) { g.Term.PrimaryID = "successor" },
	} {
		old := f.base
		change(&f.base)
		h.do(http.MethodGet, path+"&continue="+first.Continue, "viewer", nil).want(http.StatusConflict)
		f.base = old
	}
	f.base.Term.PrimaryName = "display-change"
	h.do(http.MethodGet, path+"&continue="+first.Continue, "viewer", nil).want(http.StatusOK)
	// Across all namespaces, keys sort by namespace and then name.
	var names []string
	cont := ""
	for {
		var page ObjectList
		h.do(http.MethodGet, "/api/v1/objects/policies?limit=1&continue="+cont, "viewer", nil).want(http.StatusOK).json(&page)
		for _, obj := range page.Items {
			m := obj["metadata"].(map[string]any)
			names = append(names, m["namespace"].(string)+"/"+m["name"].(string))
		}
		if page.Continue == "" {
			break
		}
		cont = page.Continue
	}
	if !slices.Equal(names, []string{"a/a", "a/b", "a/c", "b/a", "b/b", "b/c"}) {
		t.Fatal("namespace ordering", names)
	}
}

func TestCapturedObjectScopedAuthorizationPrecedesPageKeys(t *testing.T) {
	a := recordSetObj("a", "hidden-before", "other")
	b := recordSetObj("b", "visible-one", "example", ep("www.example.", "A", "192.0.2.1"))
	c := recordSetObj("c", "hidden-between", "other")
	d := recordSetObj("d", "visible-two", "example")
	h, f := newObjectReadFixture(t, a, b, c, d, zoneObj("example", "example."))
	// A live change of zoneRef must not alter captured authorization identity.
	live := b.DeepCopy()
	live.Spec.ZoneRef.Name = "other"
	h.seed(store.ManagerAPI, live)
	var first, second ObjectList
	h.do(http.MethodGet, "/api/v1/objects/dnsrecordsets?limit=1", "zoneadmin", nil).want(http.StatusOK).json(&first)
	cursor, err := decodeObjectReadCursor(first.Continue)
	if err != nil || cursor.NS != "b" || cursor.Name != "visible-one" || !slices.Equal(objectReadNames(first.Items), []string{"visible-one"}) {
		t.Fatal("unauthorized key leaked into page/cursor", first, cursor, err)
	}
	h.do(http.MethodGet, "/api/v1/objects/dnsrecordsets?limit=1&continue="+first.Continue, "zoneadmin", nil).want(http.StatusOK).json(&second)
	if !slices.Equal(objectReadNames(second.Items), []string{"visible-two"}) || second.Continue != "" {
		t.Fatal("authorization after page boundary", second)
	}
	h.do(http.MethodGet, "/api/v1/objects/dnsrecordsets/b/visible-one", "zoneadmin", nil).want(http.StatusOK)
	h.do(http.MethodGet, "/api/v1/objects/dnsrecordsets/a/hidden-before", "zoneadmin", nil).want(http.StatusForbidden)
	h.do(http.MethodGet, "/api/v1/objects/dnsrecordsets/a/missing", "zoneadmin", nil).want(http.StatusForbidden)
	h.do(http.MethodGet, "/api/v1/objects/dnszones/missing", "viewer", nil).want(http.StatusNotFound)
	h.do(http.MethodGet, "/api/v1/objects/dnszones/other", "zoneadmin", nil).want(http.StatusForbidden)
	if f.objects.RecordSets()[1].Spec.ZoneRef.Name != "example" {
		t.Fatal("captured source changed")
	}
}

func TestCapturedObjectCursorRejectsMalformedAndOversize(t *testing.T) {
	h, _ := newObjectReadFixture(t, zoneObj("a", "a."), zoneObj("b", "b."))
	var first ObjectList
	h.do(http.MethodGet, "/api/v1/objects/dnszones?limit=1", "viewer", nil).want(http.StatusOK).json(&first)
	valid, err := base64.RawURLEncoding.DecodeString(first.Continue)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"!", strings.Repeat("a", maxObjectReadCursor+1), base64.RawURLEncoding.EncodeToString(append(valid, []byte(" {}")...)),
		base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(valid), `"v":1`, `"v":2`, 1))),
		base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(valid), `"v":1`, `"extra":1,"v":1`, 1))),
		base64.RawURLEncoding.EncodeToString([]byte(`{}`)),
	} {
		h.do(http.MethodGet, "/api/v1/objects/dnszones?continue="+raw, "viewer", nil).want(http.StatusBadRequest)
	}
	cursor, _ := decodeObjectReadCursor(first.Continue)
	for _, mutate := range []func(*objectReadCursor){
		func(c *objectReadCursor) { c.Name = "" }, func(c *objectReadCursor) { c.NS = strings.Repeat("a", 1025) },
		func(c *objectReadCursor) { c.Base.Digest = "bad" }, func(c *objectReadCursor) { c.Query = strings.Repeat("z", 64) },
	} {
		bad := cursor
		mutate(&bad)
		body, _ := json.Marshal(bad)
		h.do(http.MethodGet, "/api/v1/objects/dnszones?continue="+base64.RawURLEncoding.EncodeToString(body), "viewer", nil).want(http.StatusBadRequest)
	}
	h.do(http.MethodGet, "/api/v1/objects/dnszones?labelSelector="+strings.Repeat("a", 4097)+"=b", "viewer", nil).want(http.StatusBadRequest)
	h.do(http.MethodGet, "/api/v1/objects/dnszones?labelSelector=a!=b", "viewer", nil).want(http.StatusBadRequest)
}

func TestCapturedObjectUnavailableEmptyAndLegacyWrites(t *testing.T) {
	h, f := newObjectReadFixture(t)
	var empty ObjectList
	h.do(http.MethodGet, "/api/v1/objects/dnszones", "viewer", nil).want(http.StatusOK).json(&empty)
	if empty.Items == nil || len(empty.Items) != 0 {
		t.Fatal("explicit empty manifests must remain an empty array", empty)
	}
	for _, err := range []error{serving.ErrReleased, serving.ErrComponents, context.Canceled} {
		f.err = err
		h.do(http.MethodGet, "/api/v1/objects/dnszones", "viewer", nil).want(http.StatusServiceUnavailable)
		h.do(http.MethodGet, "/api/v1/objects/dnszones/missing", "viewer", nil).want(http.StatusServiceUnavailable)
	}
	f.err = nil
	f.objects = zonecache.ObjectSet{}
	h.do(http.MethodGet, "/api/v1/objects/dnszones", "viewer", nil).want(http.StatusServiceUnavailable)
	f.calls = 0
	f.err = serving.ErrPending
	h.do(http.MethodPut, "/api/v1/objects/dnszones/current", "admin", zoneDoc("current", "current.example.")).want(http.StatusCreated)
	h.do(http.MethodDelete, "/api/v1/objects/dnszones/current", "admin", nil).want(http.StatusNoContent)
	if f.calls != 0 {
		t.Fatal("writes used captured read source")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := h.srv.capturedObjects(ctx); err == nil || f.calls != 0 {
		t.Fatal("canceled capture was used", err)
	}
	legacy := newHarness(t)
	legacy.seed(store.ManagerAPI, zoneObj("legacy", "legacy.example."))
	legacy.do(http.MethodGet, "/api/v1/objects/dnszones/legacy", "viewer", nil).want(http.StatusOK)
}
