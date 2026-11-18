package api

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"os"
	pathpkg "path"
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
// scan credits a verb only for the approved check helpers and permission
// literal types of their own packages, and reports a resource/verb pair of
// the same shape anywhere else instead of counting it as a use. The
// negative controls are the point: an unrelated helper or type that shares
// an approved bare name must not bypass permission-use accounting.
func TestHandlerPermissionScanCreditsOnlyChecks(t *testing.T) {
	perms := &rbac.Permissions{Resources: map[string][]string{
		"zones":   {"read", "write", "delete", "transfer", "sign"},
		"records": {"read", "write"},
		"cache":   {"flush"},
		"tokens":  {"revoke"},
		"users":   {"disable"},
	}}
	const rbacImport = `import "github.com/robertherbaugh/kudurru/internal/rbac"
`
	for _, tc := range []struct {
		name         string
		files        map[string]string
		found        string
		unrecognized string
		where        map[string]string // permission to a substring of its first location
	}{{
		name: "approved shapes in their own package",
		files: map[string]string{"internal/api/x.go": `package api

` + rbacImport + `
var routes = []perm{{Resource: "zones", Verb: "transfer"}}

var hint = Missing{Resource: "zones", Verb: "sign"}

func (s *Server) require(ctx context.Context, resource, verb string) error { return nil }

func f(s *Server) {
	_ = s.require(ctx, "zones", "write", clusterScope, "")
	_ = rbac.Attributes{Resource: "records", Verb: "read"}
	_ = perm{Resource: "zones", Verb: "read"}
	cacheGet("zones", "delete")
	_ = Missing{Resource: "zones", Verb: dynamic}
}
`},
		found:        "records.read,zones.read,zones.transfer,zones.write",
		unrecognized: "zones.delete,zones.sign",
		where: map[string]string{
			"zones.sign":   "internal/api/x.go:7 (literal Missing{})",
			"zones.delete": "call cacheGet()",
		},
	}, {
		// The bare names require, allowed and authorize are approved only
		// for internal/api's Server (and permission for cmd/kudurru's
		// servingHTTP); the same names elsewhere are unrelated helpers.
		name: "same bare name in another package",
		files: map[string]string{
			"internal/other/y.go": `package other

func (h *H) require(ctx context.Context, resource, verb string) error { return nil }

func (h *H) permission(ctx context.Context, resource, verb string) {}

func g(h *H) {
	_ = h.require(ctx, "cache", "flush")
	h.permission(ctx, "tokens", "revoke")
}
`,
			"cmd/kudurru/z.go": `package kudurru

func (h *servingHTTP) permission(ctx context.Context, resource, verb string) {}

func (s *Server) require(ctx context.Context, resource, verb string) error { return nil }

func k(h *servingHTTP, s *Server) {
	h.permission(ctx, "records", "write")
	_ = s.require(ctx, "users", "disable")
}
`},
		found:        "records.write",
		unrecognized: "cache.flush,tokens.revoke,users.disable",
		where:        map[string]string{"cache.flush": "internal/other/y.go:8 (call require())"},
	}, {
		// A second declaration of an approved name in the approved package
		// is ambiguous for a scan that does not resolve receiver types.
		name: "approved name declared twice in its package",
		files: map[string]string{"internal/api/a.go": `package api

func (s *Server) allowed(ctx context.Context, resource, verb string) bool { return true }

func (c *cacheView) allowed(resource, verb string) bool { return true }

func h(s *Server) {
	_ = s.allowed(ctx, "zones", "write")
}
`},
		found:        "",
		unrecognized: "zones.write",
	}, {
		name: "approved name declared on the wrong receiver",
		files: map[string]string{"internal/api/a.go": `package api

func (c *cacheView) authorize(resource, verb string) bool { return true }

func h(c *cacheView) {
	_ = c.authorize("zones", "sign")
}
`},
		found:        "",
		unrecognized: "zones.sign",
	}, {
		name: "same type name from another package or a local type",
		files: map[string]string{
			"internal/other/y.go": `package other

import fake "example.com/fake/rbac"

type perm struct{ Resource, Verb string }

var a = perm{Resource: "tokens", Verb: "revoke"}
var b = fake.Attributes{Resource: "users", Verb: "disable"}
var c = []fake.Route{{Resource: "cache", Verb: "flush"}}
`,
			"internal/auth/stack/r.go": `package stack

import "github.com/robertherbaugh/kudurru/internal/rbac"

type Route struct{ Resource, Verb string }

var routes = []Route{{Resource: "zones", Verb: "read"}}
var attrs = map[string]rbac.Attributes{"x": {Resource: "records", Verb: "write"}}
`},
		found:        "records.write,zones.read",
		unrecognized: "cache.flush,tokens.revoke,users.disable",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for name, src := range tc.files {
				path := filepath.Join(root, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			found, unrecognized := handlerPermissionChecks(t, perms, root, ".")
			keys := func(m map[string][]string) string {
				var out []string
				for k := range m {
					out = append(out, k)
				}
				sort.Strings(out)
				return strings.Join(out, ",")
			}
			if got := keys(found); got != tc.found {
				t.Errorf("credited %q, want %q", got, tc.found)
			}
			if got := keys(unrecognized); got != tc.unrecognized {
				t.Errorf("unrecognized %q, want %q", got, tc.unrecognized)
			}
			for perm, want := range tc.where {
				if w := strings.Join(append(found[perm], unrecognized[perm]...), ""); !strings.Contains(w, want) {
					t.Errorf("%s location %q, want it to contain %q", perm, w, want)
				}
			}
		})
	}
}

// checkSymbol names one approved permission-check helper by the package
// directory that declares it (slash-separated, relative to the repository
// root), its receiver type ("" for a function) and its name. The scan
// credits a call only inside that package, and only when the package
// declares exactly one symbol of that name: a same-named helper in another
// package, or a second declaration of the name in the same package, is
// ambiguous and is reported instead of credited.
type checkSymbol struct{ pkg, recv, name string }

// permissionCheckCallees are the functions and methods whose adjacent
// string-literal resource and verb arguments are authorization checks:
// Server.require, Server.allowed, Server.listAllowed and Server.authorize
// (internal/api), zoneRequirement (zone command plans),
// servingHTTP.permission and servingHTTPCluster (cmd/kudurru). A method
// call is matched by name on any receiver inside its declaring package; the
// scan reads syntax only, so it cannot tell two receivers of one package
// apart, which is why a second declaration of the name there is refused.
var permissionCheckCallees = []checkSymbol{
	{"internal/api", "Server", "require"},
	{"internal/api", "Server", "allowed"},
	{"internal/api", "Server", "listAllowed"},
	{"internal/api", "Server", "authorize"},
	{"internal/api", "", "zoneRequirement"},
	{"cmd/kudurru", "servingHTTP", "permission"},
	{"cmd/kudurru", "", "servingHTTPCluster"},
}

// permissionCheckTypes are the composite literal types, named by declaring
// package directory, whose string-literal Resource and Verb fields are a
// permission check or a mounted route declaration: perm (internal/api route
// permissions), rbac.Attributes (an authorization request), rbac.Route and
// stack.Route (the route tables of internal/rbac and the auth stack). A
// type is written unqualified inside its own package and qualified by an
// import of that package elsewhere; the scan resolves the qualifier through
// the file's imports, so a local variable or another package's type of the
// same name is not credited. An elided literal counts when the slice or map
// it sits in has one of these element types.
var permissionCheckTypes = []checkSymbol{
	{"internal/api", "", "perm"},
	{"internal/rbac", "", "Attributes"},
	{"internal/rbac", "", "Route"},
	{"internal/auth/stack", "", "Route"},
}

// handlerPermissionChecks finds the permissions that production code
// checks outside the route table: a composite literal of a type in
// permissionCheckTypes with string-literal Resource and Verb fields, or a
// call to a function in permissionCheckCallees with two adjacent
// string-literal arguments naming a declared resource and one of its
// verbs. Both allowlists are qualified by package and symbol: a helper or
// type of the same bare name in another package, or a helper name declared
// twice in its own package, is not credited. Test files are skipped, so a
// check only a test performs does not count. A literal or call of the same
// shape outside the allowlists (a Missing{} hint, an unrelated
// f("cache", "read"), an unrelated allowed("zones", "write")) is not
// credited and is returned in unrecognized, so the caller fails until the
// new helper or type is added here on purpose. Limitations: the scan does
// not prove the checking code is reachable (a perm{} in a registration
// function nobody calls still counts), a check whose resource or verb is
// computed is not seen and needs a route declaration or a reservation, and
// it reads syntax, not types, so a method is identified by its name within
// its package.
func handlerPermissionChecks(t *testing.T, perms *rbac.Permissions, root string, dirs ...string) (found, unrecognized map[string][]string) {
	t.Helper()
	const modulePrefix = "github.com/robertherbaugh/kudurru/"
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
	// typeRef splits a type expression into its package qualifier (empty
	// for an unqualified name) and name.
	var typeRef func(e ast.Expr) (qual, name string)
	typeRef = func(e ast.Expr) (string, string) {
		switch e := e.(type) {
		case *ast.Ident:
			return "", e.Name
		case *ast.SelectorExpr:
			if x, ok := e.X.(*ast.Ident); ok {
				return x.Name, e.Sel.Name
			}
		case *ast.StarExpr:
			return typeRef(e.X)
		}
		return "", ""
	}
	// elementType is the type an elided literal inside lit has.
	elementType := func(lit *ast.CompositeLit) (string, string) {
		switch tt := lit.Type.(type) {
		case *ast.ArrayType:
			return typeRef(tt.Elt)
		case *ast.MapType:
			return typeRef(tt.Value)
		}
		return "", ""
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
	// receiver is the receiver type name of a method declaration, or ""
	// for a function.
	receiver := func(fd *ast.FuncDecl) string {
		if fd.Recv == nil || len(fd.Recv.List) != 1 {
			return ""
		}
		e := fd.Recv.List[0].Type
		for {
			switch x := e.(type) {
			case *ast.StarExpr:
				e = x.X
			case *ast.IndexExpr:
				e = x.X
			case *ast.IndexListExpr:
				e = x.X
			case *ast.Ident:
				return x.Name
			default:
				return "?"
			}
		}
	}

	type source struct {
		rel, pkg string
		file     *ast.File
	}
	var sources []source
	// declared[pkg][name] lists the receivers of every top-level function
	// or method of that name in the package ("" for a function).
	declared := map[string]map[string][]string{}
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
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			pkg := pathpkg.Dir(rel)
			sources = append(sources, source{rel, pkg, f})
			for _, decl := range f.Decls {
				if fd, ok := decl.(*ast.FuncDecl); ok {
					if declared[pkg] == nil {
						declared[pkg] = map[string][]string{}
					}
					declared[pkg][fd.Name.Name] = append(declared[pkg][fd.Name.Name], receiver(fd))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// approvedCallee: the package declares exactly one symbol of the name
	// and it is the approved one.
	approvedCallee := func(pkg, name string) bool {
		recvs := declared[pkg][name]
		if len(recvs) != 1 {
			return false
		}
		for _, c := range permissionCheckCallees {
			if c.pkg == pkg && c.name == name && c.recv == recvs[0] {
				return true
			}
		}
		return false
	}

	for _, src := range sources {
		// importPkg maps a file-local import name to the module-relative
		// package directory of a module import ("" for any other import).
		importPkg := map[string]string{}
		for _, imp := range src.file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			name := pathpkg.Base(path)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			if rest, ok := strings.CutPrefix(path, modulePrefix); ok {
				importPkg[name] = rest
			} else {
				importPkg[name] = ""
			}
		}
		approvedType := func(qual, name string) bool {
			pkg := src.pkg
			if qual != "" {
				var ok bool
				if pkg, ok = importPkg[qual]; !ok {
					return false
				}
			}
			for _, c := range permissionCheckTypes {
				if c.pkg == pkg && c.name == name {
					return true
				}
			}
			return false
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
		ast.Inspect(src.file, func(n ast.Node) bool {
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
				var qual, name string
				if n.Type != nil {
					qual, name = typeRef(n.Type)
				} else {
					name = "elided"
					for i := len(stack) - 1; i >= 0; i-- {
						if outer, ok := stack[i].(*ast.CompositeLit); ok && outer.Type != nil {
							qual, name = elementType(outer)
							break
						}
					}
				}
				shown := name
				if qual != "" {
					shown = qual + "." + name
				}
				add(res, verb, n.Pos(), "literal "+shown+"{}", approvedType(qual, name))
			case *ast.CallExpr:
				name := callee(n.Fun)
				for i := 0; i+1 < len(n.Args); i++ {
					res, ok1 := str(n.Args[i])
					verb, ok2 := str(n.Args[i+1])
					if ok1 && ok2 {
						add(res, verb, n.Args[i].Pos(), "call "+name+"()", approvedCallee(src.pkg, name))
					}
				}
			}
			stack = append(stack, n)
			return true
		})
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
