package api

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

// TestPermissionVerbsAreUsedOrReserved is the reverse permission lint
// (D393): every resource.verb in permissions.yaml is declared by a mounted
// route, checked by a handler, or reserved there with its target release.
// Both sides are read from data, not from a list in this test: the route
// table of the server, and the handler checks found in the production
// sources, so rows that other packages add are covered when they land.
func TestPermissionVerbsAreUsedOrReserved(t *testing.T) {
	perms, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	used := map[string][]string{}
	for _, r := range newHarness(t).srv.Routes() {
		resources := r.Resources
		if r.Resource != "" {
			resources = append([]string{r.Resource}, resources...)
		}
		for _, res := range resources {
			used[res+"."+r.Verb] = append(used[res+"."+r.Verb], "route "+r.Method+" "+r.Path)
		}
	}
	for perm, where := range handlerPermissionChecks(t, perms, filepath.Join("..", ".."), "internal", "cmd", "ee") {
		used[perm] = append(used[perm], where...)
	}
	for _, err := range rbac.LintVerbUse(perms, used) {
		t.Error(err)
	}
	if testing.Verbose() {
		keys := make([]string, 0, len(used))
		for k := range used {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			t.Logf("%s: %d uses, first %s", k, len(used[k]), used[k][0])
		}
	}
}

func TestLintVerbUseCatchesUnusedAndStale(t *testing.T) {
	perms := &rbac.Permissions{
		Resources: map[string][]string{"zones": {"read", "sign", "share"}},
		Reserved:  map[string]string{"zones.sign": "v1.0: online signing", "zones.read": "v1.0: stale"},
	}
	var msgs []string
	for _, e := range rbac.LintVerbUse(perms, map[string][]string{
		"zones.read": {"route GET /zones"}, "zones.fly": {"handler x.go:1"},
	}) {
		msgs = append(msgs, e.Error())
	}
	want := []string{"zones.fly is checked by handler x.go:1 but", "zones.read is reserved but is checked by route GET /zones", "zones.share is declared but no route or handler checks it"}
	if len(msgs) != len(want) {
		t.Fatalf("lint errors:\n%s", strings.Join(msgs, "\n"))
	}
	for i, w := range want {
		if !strings.Contains(msgs[i], w) {
			t.Errorf("error %d %q does not mention %q", i, msgs[i], w)
		}
	}
}

// handlerPermissionChecks finds the permissions that production code
// checks outside the route table: a composite literal with string-literal
// Resource and Verb fields (rbac.Attributes, perm, auth stack routes), or
// a call with two adjacent string-literal arguments naming a declared
// resource and one of its verbs (s.require, s.allowed, s.authorize,
// zoneRequirement, h.permission, servingHTTPCluster and the like). Test
// files are skipped, so a check only a test performs does not count.
func handlerPermissionChecks(t *testing.T, perms *rbac.Permissions, root string, dirs ...string) map[string][]string {
	t.Helper()
	found := map[string][]string{}
	fset := token.NewFileSet()
	str := func(e ast.Expr) (string, bool) {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(lit.Value)
		return v, err == nil
	}
	for _, dir := range dirs {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			add := func(res, verb string, at token.Pos) {
				if perms.Has(res, verb) {
					p := fset.Position(at)
					rel, _ := filepath.Rel(root, p.Filename)
					found[res+"."+verb] = append(found[res+"."+verb], "handler "+filepath.ToSlash(rel)+":"+strconv.Itoa(p.Line))
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CompositeLit:
					var res, verb string
					for _, el := range n.Elts {
						kv, ok := el.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						key, ok := kv.Key.(*ast.Ident)
						if !ok {
							continue
						}
						switch v, _ := str(kv.Value); strings.ToLower(key.Name) {
						case "resource":
							res = v
						case "verb":
							verb = v
						}
					}
					add(res, verb, n.Pos())
				case *ast.CallExpr:
					for i := 0; i+1 < len(n.Args); i++ {
						res, ok1 := str(n.Args[i])
						verb, ok2 := str(n.Args[i+1])
						if ok1 && ok2 {
							add(res, verb, n.Args[i].Pos())
						}
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return found
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
