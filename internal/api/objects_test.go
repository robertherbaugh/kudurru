package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestVersionIsPublic(t *testing.T) {
	h := newHarness(t)
	r := h.do(http.MethodGet, "/api/v1/version", "", nil).want(http.StatusOK)
	m := r.m()
	if m["version"] != "v0.1.0-test" || m["node"] != "node-a" || m["api"] != "v1" {
		t.Fatalf("version body %v", m)
	}
	if _, ok := m["took_ms"]; !ok {
		t.Fatalf("no took_ms in %v", m)
	}
	for _, hdr := range []string{"X-Request-Id", "Content-Security-Policy", "X-Frame-Options", "X-Content-Type-Options"} {
		if r.Header().Get(hdr) == "" {
			t.Errorf("missing %s", hdr)
		}
	}
}

func TestUnauthenticated(t *testing.T) {
	h := newHarness(t)
	r := h.do(http.MethodGet, "/api/v1/zones", "", nil).want(http.StatusUnauthorized)
	if r.errKey() != KeyUnauthenticated {
		t.Fatalf("key %q", r.errKey())
	}
	if r.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("no WWW-Authenticate")
	}
	// Bad credentials are refused by the authenticator itself.
	h.do(http.MethodGet, "/api/v1/zones", "", nil, "Authorization", "Bearer kdr_nope").want(http.StatusUnauthorized)
}

func TestObjectCRUD(t *testing.T) {
	h := newHarness(t)
	r := h.do(http.MethodPut, "/api/v1/objects/dnszones/example", "admin", zoneDoc("example", "example.com.")).want(http.StatusCreated)
	if loc := r.Header().Get("Location"); loc != "/api/v1/objects/dnszones/example" {
		t.Fatalf("Location %q", loc)
	}
	obj := r.m()
	meta := obj["metadata"].(map[string]any)
	rv := meta["resourceVersion"].(string)
	if obj["kind"] != "DNSZone" || obj["apiVersion"] != "kudurru.io/v1alpha1" {
		t.Fatalf("typemeta %v", obj)
	}
	if meta["annotations"].(map[string]any)[v1alpha1.ManagedByAnnotation] != "api" {
		t.Fatalf("managedBy %v", meta)
	}
	// Defaults were applied (D70: the validator writes them back).
	if spec := obj["spec"].(map[string]any); spec["defaultTTL"] == nil {
		t.Fatalf("defaultTTL not defaulted: %v", spec)
	}
	if r.Header().Get("ETag") != rv {
		t.Fatalf("ETag %q, rv %q", r.Header().Get("ETag"), rv)
	}

	// An identical re-apply is unchanged and keeps the resourceVersion.
	r = h.do(http.MethodPut, "/api/v1/objects/dnszones/example", "admin", zoneDoc("example", "example.com.")).want(http.StatusOK)
	if got := r.m()["metadata"].(map[string]any)["resourceVersion"]; got != rv {
		t.Fatalf("unchanged re-apply moved rv %v → %v", rv, got)
	}

	r = h.do(http.MethodGet, "/api/v1/objects/dnszones/example", "viewer", nil).want(http.StatusOK)
	if r.m()["spec"].(map[string]any)["zoneName"] != "example.com." {
		t.Fatal("get returned wrong object")
	}
	list := h.do(http.MethodGet, "/api/v1/objects/dnszones", "viewer", nil).want(http.StatusOK).m()
	if items := list["items"].([]any); len(items) != 1 {
		t.Fatalf("list %v", list)
	}

	h.do(http.MethodDelete, "/api/v1/objects/dnszones/example", "admin", nil).want(http.StatusNoContent)
	h.do(http.MethodGet, "/api/v1/objects/dnszones/example", "admin", nil).want(http.StatusNotFound)
}

func TestObjectValidation(t *testing.T) {
	h := newHarness(t)
	doc := zoneDoc("example", "example.com.")
	doc["spec"].(map[string]any)["bogus"] = true
	r := h.do(http.MethodPut, "/api/v1/objects/dnszones/example", "admin", doc).want(http.StatusBadRequest)
	if r.errKey() != KeyInvalid || !strings.Contains(r.Body.String(), "bogus") {
		t.Fatalf("unknown field: %s", r.Body.String())
	}
	doc = zoneDoc("example", "example.com.")
	doc["spec"].(map[string]any)["type"] = "nonsense"
	r = h.do(http.MethodPut, "/api/v1/objects/dnszones/example", "admin", doc).want(http.StatusBadRequest)
	if !strings.Contains(r.Body.String(), "spec.type") {
		t.Fatalf("enum violation should name spec.type: %s", r.Body.String())
	}
	// Name mismatch between URL and body.
	h.do(http.MethodPut, "/api/v1/objects/dnszones/other", "admin", zoneDoc("example", "example.com.")).want(http.StatusBadRequest)
	// Unknown kind.
	h.do(http.MethodGet, "/api/v1/objects/widgets", "admin", nil).want(http.StatusNotFound)
	// Namespaced kind addressed without a namespace.
	h.do(http.MethodGet, "/api/v1/objects/blocklists/x", "admin", nil).want(http.StatusNotFound)
}

func TestIfMatch(t *testing.T) {
	h := newHarness(t)
	h.do(http.MethodPut, "/api/v1/objects/dnszones/example", "admin", zoneDoc("example", "example.com.")).want(http.StatusCreated)
	doc := zoneDoc("example", "example.com.")
	doc["spec"].(map[string]any)["defaultTTL"] = 60
	r := h.do(http.MethodPut, "/api/v1/objects/dnszones/example", "admin", doc, "If-Match", "999").want(http.StatusPreconditionFailed)
	if r.errKey() != KeyPrecondition {
		t.Fatalf("key %q", r.errKey())
	}
	cur := h.do(http.MethodGet, "/api/v1/objects/dnszones/example", "admin", nil).want(http.StatusOK)
	rv := cur.Header().Get("ETag")
	h.do(http.MethodPut, "/api/v1/objects/dnszones/example", "admin", doc, "If-Match", rv).want(http.StatusOK)
	// The old version no longer matches.
	doc["spec"].(map[string]any)["defaultTTL"] = 120
	h.do(http.MethodPut, "/api/v1/objects/dnszones/example", "admin", doc, "If-Match", rv).want(http.StatusPreconditionFailed)
	h.do(http.MethodDelete, "/api/v1/objects/dnszones/example", "admin", nil, "If-Match", rv).want(http.StatusPreconditionFailed)
}

func TestManagedByConflict(t *testing.T) {
	h := newHarness(t)
	h.seed(store.ManagerFile("objects.d/zones.yaml"), zoneObj("example", "example.com."))
	r := h.do(http.MethodPut, "/api/v1/objects/dnszones/example", "admin", zoneDoc("example", "example.com.")).want(http.StatusConflict)
	m := r.m()["error"].(map[string]any)
	if m["key"] != KeyManagedBy || m["hint"] != "file:objects.d/zones.yaml" {
		t.Fatalf("managed_by envelope %v", m)
	}
	h.do(http.MethodDelete, "/api/v1/objects/dnszones/example", "admin", nil).want(http.StatusConflict)

	// First-boot defaults are taken over by the API.
	h.seed(store.ManagerBootstrap, zoneObj("boot", "boot.example."))
	r = h.do(http.MethodPut, "/api/v1/objects/dnszones/boot", "admin", zoneDoc("boot", "boot.example.")).want(http.StatusOK)
	if r.m()["metadata"].(map[string]any)["annotations"].(map[string]any)[v1alpha1.ManagedByAnnotation] != "api" {
		t.Fatal("bootstrap object not taken over")
	}
}

func TestListPagination(t *testing.T) {
	h := newHarness(t)
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		h.seed("api", zoneObj("z-"+n, n+".example."))
	}
	var names []string
	cont := ""
	pages := 0
	for {
		path := "/api/v1/objects/dnszones?limit=2"
		if cont != "" {
			path += "&continue=" + cont
		}
		var page struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"items"`
			Continue string `json:"continue"`
		}
		h.do(http.MethodGet, path, "viewer", nil).want(http.StatusOK).json(&page)
		pages++
		for _, it := range page.Items {
			names = append(names, it.Metadata.Name)
		}
		if page.Continue == "" {
			break
		}
		cont = page.Continue
	}
	if pages != 3 || strings.Join(names, ",") != "z-a,z-b,z-c,z-d,z-e" {
		t.Fatalf("pages %d names %v", pages, names)
	}
	h.do(http.MethodGet, "/api/v1/objects/dnszones?continue=!!!", "viewer", nil).want(http.StatusBadRequest)
}

func TestApply(t *testing.T) {
	h := newHarness(t)
	h.seed(store.ManagerFile("objects.d/lists.yaml"), &v1alpha1.BlockList{
		ObjectMeta: objectMeta("default", "filed"),
		Spec:       v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{Inline: []string{"a.example"}}}},
	})
	body := `apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: {name: example}
spec: {zoneName: example.com., type: primary, nameservers: [ns1.example.com.]}
---
apiVersion: kudurru.io/v1alpha1
kind: BlockList
metadata: {name: ads}
spec:
  sources: [{inline: [ads.example]}]
---
apiVersion: kudurru.io/v1alpha1
kind: BlockList
metadata: {name: filed}
spec:
  sources: [{inline: [b.example]}]
---
apiVersion: kudurru.io/v1alpha1
kind: Widget
metadata: {name: nope}
`
	var out struct {
		Items []ApplyResult `json:"items"`
	}
	h.do(http.MethodPost, "/api/v1/apply", "admin", body, "Content-Type", "application/yaml").want(http.StatusOK).json(&out)
	want := []string{"created", "created", "error", "error"}
	if len(out.Items) != len(want) {
		t.Fatalf("results %+v", out.Items)
	}
	for i, w := range want {
		if out.Items[i].Result != w {
			t.Errorf("doc %d: %+v, want %s", i, out.Items[i], w)
		}
	}
	if out.Items[1].Namespace != "default" || out.Items[2].ErrorKey != KeyManagedBy || out.Items[2].Hint != "file:objects.d/lists.yaml" {
		t.Errorf("results %+v", out.Items)
	}
	// Re-apply as JSON: unchanged.
	jsonBody := `[{"apiVersion":"kudurru.io/v1alpha1","kind":"DNSZone","metadata":{"name":"example"},"spec":{"zoneName":"example.com.","type":"primary","nameservers":["ns1.example.com."]}}]`
	h.do(http.MethodPost, "/api/v1/apply", "admin", jsonBody, "Content-Type", "application/json").want(http.StatusOK).json(&out)
	if len(out.Items) != 1 || out.Items[0].Result != "unchanged" {
		t.Fatalf("re-apply %+v", out.Items)
	}
	// A viewer gets a per-document forbidden result.
	h.do(http.MethodPost, "/api/v1/apply", "viewer", jsonBody, "Content-Type", "application/json").want(http.StatusOK).json(&out)
	if out.Items[0].Result != "error" || out.Items[0].ErrorKey != KeyForbidden {
		t.Fatalf("viewer apply %+v", out.Items)
	}
	h.do(http.MethodPost, "/api/v1/apply", "admin", "", "Content-Type", "application/yaml").want(http.StatusBadRequest)
}

func TestBatchDelete(t *testing.T) {
	h := newHarness(t)
	h.seed("api", zoneObj("a", "a.example."), zoneObj("b", "b.example."))
	var out Processed
	h.do(http.MethodPost, "/api/v1/dnszones:batchDelete", "admin", map[string]any{"names": []string{"a", "b", "c"}}).want(http.StatusOK).json(&out)
	if len(out.Processed.Success) != 2 || len(out.Processed.Errors) != 1 || out.Processed.Errors[0].Item != "c" {
		t.Fatalf("processed %+v", out)
	}
}

func TestReadOnlyShape(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Objects = staticLoader(t, o.Store) })
	r := h.do(http.MethodPut, "/api/v1/objects/dnszones/example", "admin", zoneDoc("example", "example.com.")).want(http.StatusServiceUnavailable)
	if r.errKey() != KeyReadOnly {
		t.Fatalf("key %q", r.errKey())
	}
	h.do(http.MethodGet, "/api/v1/objects/dnszones", "admin", nil).want(http.StatusOK)
}
