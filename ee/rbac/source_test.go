package rbac

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	internalv1alpha1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"

	"github.com/robertherbaugh/kudurru/internal/rbac"
)

func mustPerms(t testing.TB) *rbac.Permissions {
	t.Helper()
	p, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSubjectMatches(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    rbac.Subject
		p    rbac.Principal
		want bool
	}{
		{"user", rbac.Subject{Kind: rbac.SubjectUser, Name: "alice"}, rbac.Principal{Name: "alice"}, true},
		{"other user", rbac.Subject{Kind: rbac.SubjectUser, Name: "alice"}, rbac.Principal{Name: "bob"}, false},
		{"empty user subject never matches", rbac.Subject{Kind: rbac.SubjectUser, Name: ""}, rbac.Principal{Name: ""}, false},
		{"empty principal name", rbac.Subject{Kind: rbac.SubjectUser, Name: "x"}, rbac.Principal{}, false},
		{"group", rbac.Subject{Kind: rbac.SubjectGroup, Name: "corp:/ops"}, rbac.Principal{Groups: []string{"a", "corp:/ops"}}, true},
		{"empty group", rbac.Subject{Kind: rbac.SubjectGroup, Name: ""}, rbac.Principal{Groups: []string{""}}, false},
		{"service account", rbac.Subject{Kind: rbac.SubjectServiceAccount, Name: "grafana", Namespace: "mon"}, rbac.Principal{ServiceAccount: "mon/grafana"}, true},
		{"service account other ns", rbac.Subject{Kind: rbac.SubjectServiceAccount, Name: "grafana", Namespace: "x"}, rbac.Principal{ServiceAccount: "mon/grafana"}, false},
		{"service account vs user", rbac.Subject{Kind: rbac.SubjectServiceAccount, Name: "grafana", Namespace: "mon"}, rbac.Principal{Name: "mon/grafana"}, false},
		{"unknown kind", rbac.Subject{Kind: "Robot", Name: "x"}, rbac.Principal{Name: "x"}, false},
	} {
		if got := rbac.SubjectMatches(tc.s, tc.p); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestParseGrantRoundTrip(t *testing.T) {
	for _, s := range []string{"admin", "zone-admin@zone:home-example-com", "viewer@clientgroup:kids"} {
		g, err := rbac.ParseGrant(s)
		if err != nil {
			t.Fatal(err)
		}
		if g.String() != s {
			t.Errorf("%q -> %q", s, g.String())
		}
	}
	for _, s := range []string{"", "@zone:x", "x@zone", "x@cluster:y", "x@:y"} {
		if _, err := rbac.ParseGrant(s); err == nil {
			t.Errorf("ParseGrant(%q) accepted", s)
		}
	}
}

func TestParseBindings(t *testing.T) {
	got, err := rbac.ParseBindings([]map[string]any{
		{"role": "admin", "scope": map[string]any{"kind": "cluster"}, "subjects": []any{map[string]any{"kind": "Group", "name": "corp:/dns-admins"}}},
		{"role": "zone-admin", "scope": map[string]any{"kind": "zone", "name": "home"}, "subjects": []any{map[string]any{"kind": "User", "name": "alice"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Scope.Kind != rbac.ScopeCluster || got[1].Scope != (rbac.Scope{Kind: rbac.ScopeZone, Name: "home"}) || got[1].Subjects[0].Name != "alice" {
		t.Fatalf("parsed %+v", got)
	}
	_, err = rbac.ParseBindings([]map[string]any{
		{"role": "admin", "subjects": []any{map[string]any{"kind": "Group", "name": "x", "namespacee": "typo"}}},
		{"role": "admin", "subjects": []any{}},
	})
	if err == nil || !strings.Contains(err.Error(), "rbac.bindings[0]") || !strings.Contains(err.Error(), "rbac.bindings[1]") {
		t.Fatalf("errors: %v", err)
	}
}

func TestBindingSourceStaticAndObjects(t *testing.T) {
	p := mustPerms(t)
	if _, err := NewBindingSource(p, []rbac.Binding{{Role: "nope", Subjects: []rbac.Subject{{Kind: rbac.SubjectUser, Name: "x"}}}}, SourceOptions{}); err == nil {
		t.Fatal("unknown static role accepted")
	}
	if _, err := NewBindingSource(p, []rbac.Binding{{Role: "zone-admin", Subjects: []rbac.Subject{{Kind: rbac.SubjectUser, Name: "x"}}}}, SourceOptions{}); err == nil {
		t.Fatal("cluster-scoped zone-admin accepted")
	}
	src, err := NewBindingSource(p, []rbac.Binding{
		{Role: "admin", Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Subjects: []rbac.Subject{{Kind: rbac.SubjectGroup, Name: "corp:/dns-admins"}}},
	}, SourceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	alice := rbac.Principal{Name: "alice"}
	zoneRead := rbac.Attributes{Resource: "records", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: "home"}}
	if d, _ := src.Authorize(ctx, alice, zoneRead); d.Allowed || d.Missing == nil || d.Path != rbac.PathBinding {
		t.Fatalf("alice before binding: %+v", d)
	}
	rb := &internalv1alpha1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "alice-home", Namespace: "dns"}, Spec: internalv1alpha1.RoleBindingSpec{
		Role: "zone-admin", Scope: internalv1alpha1.BindingScope{Kind: "zone", Name: "home"},
		Subjects: []internalv1alpha1.BindingSubject{{Kind: "User", Name: "alice"}},
	}}
	bad := &internalv1alpha1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "dns"}, Spec: internalv1alpha1.RoleBindingSpec{
		Role: "superuser", Subjects: []internalv1alpha1.BindingSubject{{Kind: "User", Name: "alice"}},
	}}
	src.SetObjects(rbac.RoleBindingObjects([]*internalv1alpha1.RoleBinding{rb, bad}))
	if d, _ := src.Authorize(ctx, alice, zoneRead); !d.Allowed || d.Path != rbac.PathBinding {
		t.Fatalf("alice with binding: %+v", d)
	}
	if d, _ := src.Authorize(ctx, alice, rbac.Attributes{Resource: "records", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: "other"}}); d.Allowed {
		t.Fatal("binding leaked to another zone")
	}
	if diag := src.Diagnoses(); len(diag) != 1 || !strings.Contains(diag[0], "dns/bad") {
		t.Fatalf("diagnoses %v", diag)
	}
	if n := len(src.Bindings()); n != 2 {
		t.Fatalf("bindings %d, want static + valid object", n)
	}
	if g := src.GrantsFor(alice); len(g) != 1 || g[0].String() != "zone-admin@zone:home" {
		t.Fatalf("GrantsFor %v", g)
	}
}

func TestBindingSourceSessionGrantsAndRevocation(t *testing.T) {
	p := mustPerms(t)
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { return now }
	src, err := NewBindingSource(p, nil, SourceOptions{Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var revoked []Revocation
	src.OnRevoke(func(r []Revocation) { mu.Lock(); revoked = append(revoked, r...); mu.Unlock() })
	ops := rbac.Binding{Role: "operator", Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Subjects: []rbac.Subject{{Kind: rbac.SubjectGroup, Name: "corp:/ops"}}}
	src.SetObjects(map[string]rbac.Binding{"dns/ops": ops})

	// A cookie session carries the grant, not the groups (D48).
	issued := now.Add(-time.Minute)
	sess := rbac.Principal{Name: "corp:carol", Grants: []rbac.Grant{{Role: "operator", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}}, GrantsIssuedAt: issued}
	ctx := context.Background()
	toggle := rbac.Attributes{Resource: "blocking", Verb: "toggle", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}
	if d, _ := src.Authorize(ctx, sess, toggle); !d.Allowed {
		t.Fatalf("session grant: %+v", d)
	}
	// A grant no binding provides is never honored (forged or stale).
	forged := sess
	forged.Grants = []rbac.Grant{{Role: "admin", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}}
	if d, _ := src.Authorize(ctx, forged, rbac.Attributes{Resource: "settings", Verb: "write"}); d.Allowed {
		t.Fatal("grant without a binding honored")
	}
	// Narrowing the binding revokes earlier sessions, even if another
	// binding still provides the role.
	now = now.Add(time.Minute)
	src.SetObjects(map[string]rbac.Binding{
		"dns/ops":   {Role: "operator", Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Subjects: []rbac.Subject{{Kind: rbac.SubjectGroup, Name: "corp:/ops-leads"}}},
		"dns/other": {Role: "viewer", Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Subjects: []rbac.Subject{{Kind: rbac.SubjectUser, Name: "dave"}}},
	})
	if d, _ := src.Authorize(ctx, sess, toggle); d.Allowed {
		t.Fatal("revoked session grant still honored")
	}
	mu.Lock()
	if len(revoked) != 1 || revoked[0].Subject.Name != "corp:/ops" || revoked[0].Grant.Role != "operator" {
		t.Fatalf("revocations %+v", revoked)
	}
	mu.Unlock()
	// A login after the change resolves the grant again and is honored.
	fresh := sess
	fresh.GrantsIssuedAt = now.Add(time.Second)
	if d, _ := src.Authorize(ctx, fresh, toggle); !d.Allowed {
		t.Fatalf("fresh session: %+v", d)
	}
	// Provider default roles are valid without a binding.
	src.AllowGrant(rbac.Grant{Role: "viewer"})
	def := rbac.Principal{Name: "x", Grants: []rbac.Grant{{Role: "viewer", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}}, GrantsIssuedAt: now}
	if d, _ := src.Authorize(ctx, def, rbac.Attributes{Resource: "zones", Verb: "read"}); !d.Allowed {
		t.Fatalf("default role: %+v", d)
	}
}

type fixedAuthorizer rbac.Decision

func (f fixedAuthorizer) Authorize(context.Context, rbac.Principal, rbac.Attributes) (rbac.Decision, error) {
	return rbac.Decision(f), nil
}

func TestEngine(t *testing.T) {
	p := mustPerms(t)
	src, err := NewBindingSource(p, []rbac.Binding{
		{Role: "viewer", Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Subjects: []rbac.Subject{{Kind: rbac.SubjectUser, Name: "vic"}}},
		{Role: "admin", Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Subjects: []rbac.Subject{{Kind: rbac.SubjectUser, Name: "root"}}},
	}, SourceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rbac.NewEngine(rbac.EngineOptions{}); err == nil {
		t.Fatal("engine without bindings accepted")
	}
	sarAllow := fixedAuthorizer{Allowed: true, Path: rbac.PathSAR}
	sarDeny := fixedAuthorizer{Allowed: false, Path: rbac.PathSAR}
	ctx := context.Background()
	write := rbac.Attributes{Resource: "settings", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}
	read := rbac.Attributes{Resource: "zones", Verb: "read", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}
	for _, tc := range []struct {
		name     string
		sar      rbac.Authorizer
		p        rbac.Principal
		a        rbac.Attributes
		allowed  bool
		path     string
		hasMiss  bool
		reasonIn string
	}{
		{"builtin admin is local", nil, rbac.Principal{Name: "admin", BuiltinAdmin: true}, write, true, rbac.PathLocal, false, "builtin"},
		{"binding", nil, rbac.Principal{Name: "vic"}, read, true, rbac.PathBinding, false, "viewer"},
		{"binding denies", nil, rbac.Principal{Name: "vic"}, write, false, rbac.PathBinding, true, ""},
		{"nobody", nil, rbac.Principal{Name: "eve"}, read, false, rbac.PathBinding, true, ""},
		{"sar allows first", sarAllow, rbac.Principal{Name: "vic"}, write, true, rbac.PathSAR, false, ""},
		{"sar denies, binding allows (D22)", sarDeny, rbac.Principal{Name: "vic"}, read, true, rbac.PathBinding, false, ""},
		{"token scopes narrow an admin", nil, rbac.Principal{Name: "root", Restrict: []rbac.Grant{{Role: "viewer", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}}}, write, false, rbac.PathNone, true, "scopes"},
		{"token scopes allow within", nil, rbac.Principal{Name: "root", Restrict: []rbac.Grant{{Role: "viewer", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}}}, read, true, rbac.PathBinding, false, ""},
		{"token scopes narrow builtin admin", nil, rbac.Principal{Name: "admin", BuiltinAdmin: true, Restrict: []rbac.Grant{{Role: "viewer"}}}, write, false, rbac.PathNone, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := rbac.NewEngine(rbac.EngineOptions{Permissions: src.Permissions(), Bindings: src, SAR: tc.sar})
			if err != nil {
				t.Fatal(err)
			}
			d, err := e.Authorize(ctx, tc.p, tc.a)
			if err != nil {
				t.Fatal(err)
			}
			if d.Allowed != tc.allowed || d.Path != tc.path || (d.Missing != nil) != tc.hasMiss || !strings.Contains(d.Reason, tc.reasonIn) {
				t.Fatalf("decision %+v", d)
			}
		})
	}
}

func TestPermissionMatrix(t *testing.T) {
	p := mustPerms(t)
	m := p.PermissionMatrix([]rbac.Grant{{Role: "viewer"}, {Role: "zone-admin", Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: "home"}}}, false)
	if !containsStr(m["cluster"], "zones.read") || containsStr(m["cluster"], "zones.write") {
		t.Errorf("cluster %v", m["cluster"])
	}
	if !containsStr(m["zone:home"], "zones.transfer") {
		t.Errorf("zone:home %v", m["zone:home"])
	}
	adm := p.PermissionMatrix(nil, true)
	if !containsStr(adm["cluster"], "cluster.promote") {
		t.Errorf("builtin admin %v", adm["cluster"])
	}
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Without the license the built-in administrator is the only role: bindings
// stay configured but grant nothing, token scopes still narrow, and the
// SubjectAccessReview path is closed too.
func TestEngineNeedsALicense(t *testing.T) {
	p := mustPerms(t)
	src, err := NewBindingSource(p, []rbac.Binding{
		{Role: "admin", Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Subjects: []rbac.Subject{{Kind: rbac.SubjectUser, Name: "root"}}},
	}, SourceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	licensed := false
	e, err := rbac.NewEngine(rbac.EngineOptions{
		Permissions: p, Bindings: src, SAR: fixedAuthorizer{Allowed: true, Path: rbac.PathSAR},
		Licensed: func() bool { return licensed },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	write := rbac.Attributes{Resource: "settings", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}
	node := rbac.Principal{Name: "node", Implicit: []rbac.Grant{{Role: "admin", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}}}

	for name, tc := range map[string]struct {
		p    rbac.Principal
		want bool
	}{
		"builtin admin":        {rbac.Principal{Name: "admin", BuiltinAdmin: true}, true},
		"peer node":            {node, true},
		"bound admin":          {rbac.Principal{Name: "root"}, false},
		"kubernetes identity":  {rbac.Principal{Name: "sa", Kind: KindKubernetesServiceAccount}, false},
		"scoped builtin admin": {rbac.Principal{Name: "admin", BuiltinAdmin: true, Restrict: []rbac.Grant{{Role: "viewer"}}}, false},
		"anonymous":            {rbac.Principal{}, false},
	} {
		d, err := e.Authorize(ctx, tc.p, write)
		if err != nil || d.Allowed != tc.want {
			t.Errorf("unlicensed %s: allowed=%v (%s) err=%v, want %v", name, d.Allowed, d.Reason, err, tc.want)
		}
		if !tc.want && d.Missing == nil {
			t.Errorf("unlicensed %s: a denial names nothing missing", name)
		}
	}
	if e.Licensed() {
		t.Error("Licensed() is true")
	}

	licensed = true
	if d, _ := e.Authorize(ctx, rbac.Principal{Name: "root"}, write); !d.Allowed || d.Path != rbac.PathSAR && d.Path != rbac.PathBinding {
		t.Errorf("licensed bound admin: %+v", d)
	}
}
