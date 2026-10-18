package api

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/robertherbaugh/kudurru/internal/rbac"
)

// TestRoutePermissionLint is the D31 lint: every registered route
// declares a permission that permissions.yaml defines, and every
// operation in the OpenAPI document went through that declaration.
func TestRoutePermissionLint(t *testing.T) {
	h := newHarness(t)
	perms, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	routes := h.srv.Routes()
	for _, err := range rbac.LintRoutes(perms, routes) {
		t.Error(err)
	}
	declared := map[string]bool{}
	for _, r := range routes {
		declared[r.Method+" "+r.Path] = true
	}
	for path, item := range h.srv.OpenAPI().Paths {
		for method, op := range map[string]*huma.Operation{
			http.MethodGet: item.Get, http.MethodPost: item.Post, http.MethodPut: item.Put,
			http.MethodPatch: item.Patch, http.MethodDelete: item.Delete,
		} {
			if op == nil {
				continue
			}
			if !declared[method+" "+BasePath+path] {
				t.Errorf("%s %s has no permission declaration", method, path)
			}
		}
	}
}

func TestLintRoutesCatchesMissingDeclaration(t *testing.T) {
	perms, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	errs := rbac.LintRoutes(perms, []rbac.Route{
		{Method: "GET", Path: "/a"},
		{Method: "GET", Path: "/b", Resource: "zones", Verb: "fly"},
		{Method: "GET", Path: "/c", Resource: "zones", Verb: "read"},
		{Method: "GET", Path: "/c", Resource: "zones", Verb: "read"},
		{Method: "GET", Path: "/d", Public: true, Resource: "zones"},
		{Method: "GET", Path: "/e", Resources: []string{"zones", "nope"}, Verb: "read"},
	})
	var msgs []string
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	sort.Strings(msgs)
	want := []string{"GET /a declares no permission", "GET /b declares zones.fly", "GET /c is declared twice", "GET /d is public", "GET /e declares nope.read"}
	if len(msgs) != len(want) {
		t.Fatalf("lint errors:\n%s", strings.Join(msgs, "\n"))
	}
	for i, w := range want {
		if !strings.Contains(msgs[i], w) {
			t.Errorf("error %d %q does not mention %q", i, msgs[i], w)
		}
	}
}

// TestOpenAPIDrift fails when api/openapi.json or api/openapi-3.0.json
// differ from the registered routes; `make openapi` rewrites them.
func TestOpenAPIDrift(t *testing.T) {
	for _, f := range []struct {
		name string
		v30  bool
	}{{OpenAPIFile, false}, {OpenAPI30File, true}} {
		want, err := OpenAPIDocument(f.v30)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join("..", "..", "api", f.name))
		if err != nil {
			t.Fatalf("%v (run make openapi)", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("api/%s is out of date: run make openapi", f.name)
		}
	}
}

func TestOpenAPIServedAndDocs(t *testing.T) {
	h := newHarness(t)
	r := h.do(http.MethodGet, "/api/v1/openapi.json", "", nil).want(http.StatusOK)
	if !strings.Contains(r.Body.String(), `"/zones/{zone}/records"`) {
		t.Fatal("openapi.json misses routes")
	}
	r = h.do(http.MethodGet, "/api/v1/docs", "", nil).want(http.StatusOK)
	if !strings.Contains(strings.ToLower(r.Body.String()), "scalar") {
		t.Fatalf("docs page is not Scalar: %.200s", r.Body.String())
	}
}

func TestUnknownAPIPathIsEnvelope(t *testing.T) {
	h := newHarness(t)
	r := h.do(http.MethodGet, "/api/v1/nope", "admin", nil).want(http.StatusNotFound)
	if r.errKey() != KeyNotFound {
		t.Fatalf("key %q", r.errKey())
	}
}
