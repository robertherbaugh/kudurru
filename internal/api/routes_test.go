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
	found, unrecognized := handlerPermissionChecks(t, perms, filepath.Join("..", ".."), "internal", "cmd", "ee")
	for perm, where := range found {
		used[perm] = append(used[perm], where...)
	}
	for perm, where := range unrecognized {
		t.Errorf("%s appears as a resource/verb pair in a call or literal the scan does not know as a permission check: %s; add the helper to permissionCheckCallees or the type to permissionCheckTypes if it checks, or write it differently if it does not", perm, strings.Join(where, ", "))
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

// TestHandlerPermissionScanCreditsOnlyChecks: the reverse lint's source
// scan credits a verb only for the known check helpers and permission
// literal types, and reports a resource/verb pair of the same shape
// anywhere else instead of counting it as a use.
func TestHandlerPermissionScanCreditsOnlyChecks(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o750); err != nil {
		t.Fatal(err)
	}
	src := `package pkg

var routes = []perm{{Resource: "zones", Verb: "transfer"}}

var hint = Missing{Resource: "zones", Verb: "sign"}

func f(s *Server) {
	_ = s.require(ctx, "zones", "write", clusterScope, "")
	_ = rbac.Attributes{Resource: "records", Verb: "read"}
	_ = perm{Resource: "zones", Verb: "read"}
	cacheGet("zones", "delete")
	_ = Missing{Resource: "zones", Verb: dynamic}
}
`
	if err := os.WriteFile(filepath.Join(root, "pkg", "x.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	perms := &rbac.Permissions{Resources: map[string][]string{
		"zones":   {"read", "write", "delete", "transfer", "sign"},
		"records": {"read"},
	}}
	found, unrecognized := handlerPermissionChecks(t, perms, root, "pkg")
	keys := func(m map[string][]string) string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	if got, want := keys(found), "records.read,zones.read,zones.transfer,zones.write"; got != want {
		t.Errorf("credited %s, want %s", got, want)
	}
	if got, want := keys(unrecognized), "zones.delete,zones.sign"; got != want {
		t.Errorf("unrecognized %s, want %s", got, want)
	}
	if w := strings.Join(unrecognized["zones.sign"], ""); !strings.Contains(w, "pkg/x.go:5") || !strings.Contains(w, "literal Missing{}") {
		t.Errorf("zones.sign location %q", w)
	}
	if w := strings.Join(unrecognized["zones.delete"], ""); !strings.Contains(w, "call cacheGet()") {
		t.Errorf("zones.delete location %q", w)
	}
}

// permissionCheckCallees are the functions and methods whose adjacent
// string-literal resource and verb arguments are authorization checks:
// Server.require, Server.allowed, Server.listAllowed and Server.authorize
// (internal/api),
// zoneRequirement (zone command plans), servingHTTP.permission and
// servingHTTPCluster (cmd/kudurru). A method is matched by its name on any
// receiver.
var permissionCheckCallees = map[string]bool{
	"require": true, "allowed": true, "listAllowed": true, "authorize": true, "zoneRequirement": true,
	"permission": true, "servingHTTPCluster": true,
}

// permissionCheckTypes are the composite literal types whose
// string-literal Resource and Verb fields are a permission check or a
// mounted route declaration: perm (internal/api route permissions),
// rbac.Attributes (an authorization request) and stack.Route (the auth
// stack's route table, written as Route inside its package). An elided
// literal counts when the slice or map it sits in has one of these
// element types.
var permissionCheckTypes = map[string]bool{
	"perm": true, "rbac.Attributes": true, "Attributes": true, "Route": true, "stack.Route": true,
}

// handlerPermissionChecks finds the permissions that production code
// checks outside the route table: a composite literal of a type in
// permissionCheckTypes with string-literal Resource and Verb fields, or a
// call to a function in permissionCheckCallees with two adjacent
// string-literal arguments naming a declared resource and one of its
// verbs. Test files are skipped, so a check only a test performs does not
// count. A literal or call of the same shape outside the allowlists (a
// Missing{} hint, an unrelated f("cache", "read")) is not credited and is
// returned in unrecognized, so the caller fails until the new helper or
// type is added here on purpose. Limitations: the scan does not prove the
// checking code is reachable (a perm{} in a registration function nobody
// calls still counts), and a check whose resource or verb is computed is
// not seen and needs a route declaration or a reservation.
func handlerPermissionChecks(t *testing.T, perms *rbac.Permissions, root string, dirs ...string) (found, unrecognized map[string][]string) {
	t.Helper()
	found, unrecognized = map[string][]string{}, map[string][]string{}
	fset := token.NewFileSet()
	str := func(e ast.Expr) (string, bool) {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(lit.Value)
		return v, err == nil
	}
	var typeName func(e ast.Expr) string
	typeName = func(e ast.Expr) string {
		switch e := e.(type) {
		case *ast.Ident:
			return e.Name
		case *ast.SelectorExpr:
			if x, ok := e.X.(*ast.Ident); ok {
				return x.Name + "." + e.Sel.Name
			}
		case *ast.StarExpr:
			return typeName(e.X)
		}
		return ""
	}
	// elementType is the type an elided literal inside lit has.
	elementType := func(lit *ast.CompositeLit) string {
		switch tt := lit.Type.(type) {
		case *ast.ArrayType:
			return typeName(tt.Elt)
		case *ast.MapType:
			return typeName(tt.Value)
		}
		return ""
	}
	callee := func(e ast.Expr) string {
		switch e := e.(type) {
		case *ast.Ident:
			return e.Name
		case *ast.SelectorExpr:
			return e.Sel.Name
		}
		return ""
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
			add := func(res, verb string, at token.Pos, shape string, ok bool) {
				if !perms.Has(res, verb) {
					return
				}
				p := fset.Position(at)
				rel, _ := filepath.Rel(root, p.Filename)
				where := "handler " + filepath.ToSlash(rel) + ":" + strconv.Itoa(p.Line)
				if ok {
					found[res+"."+verb] = append(found[res+"."+verb], where)
				} else {
					unrecognized[res+"."+verb] = append(unrecognized[res+"."+verb], where+" ("+shape+")")
				}
			}
			var stack []ast.Node
			ast.Inspect(f, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
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
					name := typeName(n.Type)
					if n.Type == nil {
						name = "elided"
						for i := len(stack) - 1; i >= 0; i-- {
							if outer, ok := stack[i].(*ast.CompositeLit); ok && outer.Type != nil {
								name = elementType(outer)
								break
							}
						}
					}
					add(res, verb, n.Pos(), "literal "+name+"{}", permissionCheckTypes[name])
				case *ast.CallExpr:
					name := callee(n.Fun)
					for i := 0; i+1 < len(n.Args); i++ {
						res, ok1 := str(n.Args[i])
						verb, ok2 := str(n.Args[i+1])
						if ok1 && ok2 {
							add(res, verb, n.Args[i].Pos(), "call "+name+"()", permissionCheckCallees[name])
						}
					}
				}
				stack = append(stack, n)
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return found, unrecognized
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
