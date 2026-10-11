package rbac

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	internalv1alpha1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
)

func mustPerms(t testing.TB) *Permissions {
	t.Helper()
	p, err := LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSubjectMatches(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    Subject
		p    Principal
		want bool
	}{
		{"user", Subject{Kind: SubjectUser, Name: "alice"}, Principal{Name: "alice"}, true},
		{"other user", Subject{Kind: SubjectUser, Name: "alice"}, Principal{Name: "bob"}, false},
		{"empty user subject never matches", Subject{Kind: SubjectUser, Name: ""}, Principal{Name: ""}, false},
		{"empty principal name", Subject{Kind: SubjectUser, Name: "x"}, Principal{}, false},
		{"group", Subject{Kind: SubjectGroup, Name: "corp:/ops"}, Principal{Groups: []string{"a", "corp:/ops"}}, true},
		{"empty group", Subject{Kind: SubjectGroup, Name: ""}, Principal{Groups: []string{""}}, false},
		{"service account", Subject{Kind: SubjectServiceAccount, Name: "grafana", Namespace: "mon"}, Principal{ServiceAccount: "mon/grafana"}, true},
		{"service account other ns", Subject{Kind: SubjectServiceAccount, Name: "grafana", Namespace: "x"}, Principal{ServiceAccount: "mon/grafana"}, false},
		{"service account vs user", Subject{Kind: SubjectServiceAccount, Name: "grafana", Namespace: "mon"}, Principal{Name: "mon/grafana"}, false},
		{"unknown kind", Subject{Kind: "Robot", Name: "x"}, Principal{Name: "x"}, false},
	} {
		if got := SubjectMatches(tc.s, tc.p); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestParseGrantRoundTrip(t *testing.T) {
	for _, s := range []string{"admin", "zone-admin@zone:home-example-com", "viewer@clientgroup:kids"} {
		g, err := ParseGrant(s)
		if err != nil {
			t.Fatal(err)
		}
		if g.String() != s {
			t.Errorf("%q -> %q", s, g.String())
		}
	}
	for _, s := range []string{"", "@zone:x", "x@zone", "x@cluster:y", "x@:y"} {
		if _, err := ParseGrant(s); err == nil {
			t.Errorf("ParseGrant(%q) accepted", s)
		}
	}
}

func TestParseBindings(t *testing.T) {
	got, err := ParseBindings([]map[string]any{
		{"role": "admin", "scope": map[string]any{"kind": "cluster"}, "subjects": []any{map[string]any{"kind": "Group", "name": "corp:/dns-admins"}}},
		{"role": "zone-admin", "scope": map[string]any{"kind": "zone", "name": "home"}, "subjects": []any{map[string]any{"kind": "User", "name": "alice"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Scope.Kind != ScopeCluster || got[1].Scope != (Scope{Kind: ScopeZone, Name: "home"}) || got[1].Subjects[0].Name != "alice" {
		t.Fatalf("parsed %+v", got)
	}
	_, err = ParseBindings([]map[string]any{
		{"role": "admin", "subjects": []any{map[string]any{"kind": "Group", "name": "x", "nmae": "typo"}}},
		{"role": "admin", "subjects": []any{}},
	})
	if err == nil || !strings.Contains(err.Error(), "rbac.bindings[0]") || !strings.Contains(err.Error(), "rbac.bindings[1]") {
		t.Fatalf("errors: %v", err)
	}
}

func TestBindingSourceStaticAndObjects(t *testing.T) {
	p := mustPerms(t)
	if _, err := NewBindingSource(p, []Binding{{Role: "nope", Subjects: []Subject{{Kind: SubjectUser, Name: "x"}}}}, SourceOptions{}); err == nil {
		t.Fatal("unknown static role accepted")
	}
	if _, err := NewBindingSource(p, []Binding{{Role: "zone-admin", Subjects: []Subject{{Kind: SubjectUser, Name: "x"}}}}, SourceOptions{}); err == nil {
		t.Fatal("cluster-scoped zone-admin accepted")
	}
	src, err := NewBindingSource(p, []Binding{
		{Role: "admin", Scope: Scope{Kind: ScopeCluster}, Subjects: []Subject{{Kind: SubjectGroup, Name: "corp:/dns-admins"}}},
	}, SourceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	alice := Principal{Name: "alice"}
	zoneRead := Attributes{Resource: "records", Verb: "write", Scope: Scope{Kind: ScopeZone, Name: "home"}}
	if d, _ := src.Authorize(ctx, alice, zoneRead); d.Allowed || d.Missing == nil || d.Path != PathBinding {
		t.Fatalf("alice before binding: %+v", d)
	}
	rb := &internalv1alpha1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "alice-home", Namespace: "dns"}, Spec: internalv1alpha1.RoleBindingSpec{
		Role: "zone-admin", Scope: internalv1alpha1.BindingScope{Kind: "zone", Name: "home"},
		Subjects: []internalv1alpha1.BindingSubject{{Kind: "User", Name: "alice"}},
	}}
	bad := &internalv1alpha1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "dns"}, Spec: internalv1alpha1.RoleBindingSpec{
		Role: "superuser", Subjects: []internalv1alpha1.BindingSubject{{Kind: "User", Name: "alice"}},
	}}
	src.SetObjects(RoleBindingObjects([]*internalv1alpha1.RoleBinding{rb, bad}))
	if d, _ := src.Authorize(ctx, alice, zoneRead); !d.Allowed || d.Path != PathBinding {
		t.Fatalf("alice with binding: %+v", d)
	}
	if d, _ := src.Authorize(ctx, alice, Attributes{Resource: "records", Verb: "write", Scope: Scope{Kind: ScopeZone, Name: "other"}}); d.Allowed {
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
	ops := Binding{Role: "operator", Scope: Scope{Kind: ScopeCluster}, Subjects: []Subject{{Kind: SubjectGroup, Name: "corp:/ops"}}}
	src.SetObjects(map[string]Binding{"dns/ops": ops})

	// A cookie session carries the grant, not the groups (D48).
	issued := now.Add(-time.Minute)
	sess := Principal{Name: "corp:carol", Grants: []Grant{{Role: "operator", Scope: Scope{Kind: ScopeCluster}}}, GrantsIssuedAt: issued}
	ctx := context.Background()
	toggle := Attributes{Resource: "blocking", Verb: "toggle", Scope: Scope{Kind: ScopeCluster}}
	if d, _ := src.Authorize(ctx, sess, toggle); !d.Allowed {
		t.Fatalf("session grant: %+v", d)
	}
	// A grant no binding provides is never honoured (forged or stale).
	forged := sess
	forged.Grants = []Grant{{Role: "admin", Scope: Scope{Kind: ScopeCluster}}}
	if d, _ := src.Authorize(ctx, forged, Attributes{Resource: "settings", Verb: "write"}); d.Allowed {
		t.Fatal("grant without a binding honoured")
	}
	// Narrowing the binding revokes earlier sessions, even if another
	// binding still provides the role.
	now = now.Add(time.Minute)
	src.SetObjects(map[string]Binding{
		"dns/ops":   {Role: "operator", Scope: Scope{Kind: ScopeCluster}, Subjects: []Subject{{Kind: SubjectGroup, Name: "corp:/ops-leads"}}},
		"dns/other": {Role: "viewer", Scope: Scope{Kind: ScopeCluster}, Subjects: []Subject{{Kind: SubjectUser, Name: "dave"}}},
	})
	if d, _ := src.Authorize(ctx, sess, toggle); d.Allowed {
		t.Fatal("revoked session grant still honoured")
	}
	mu.Lock()
	if len(revoked) != 1 || revoked[0].Subject.Name != "corp:/ops" || revoked[0].Grant.Role != "operator" {
		t.Fatalf("revocations %+v", revoked)
	}
	mu.Unlock()
	// A login after the change resolves the grant again and is honoured.
	fresh := sess
	fresh.GrantsIssuedAt = now.Add(time.Second)
	if d, _ := src.Authorize(ctx, fresh, toggle); !d.Allowed {
		t.Fatalf("fresh session: %+v", d)
	}
	// Provider default roles are valid without a binding.
	src.AllowGrant(Grant{Role: "viewer"})
	def := Principal{Name: "x", Grants: []Grant{{Role: "viewer", Scope: Scope{Kind: ScopeCluster}}}, GrantsIssuedAt: now}
	if d, _ := src.Authorize(ctx, def, Attributes{Resource: "zones", Verb: "read"}); !d.Allowed {
		t.Fatalf("default role: %+v", d)
	}
}

type fixedAuthorizer Decision

func (f fixedAuthorizer) Authorize(context.Context, Principal, Attributes) (Decision, error) {
	return Decision(f), nil
}

func TestEngine(t *testing.T) {
	p := mustPerms(t)
	src, err := NewBindingSource(p, []Binding{
		{Role: "viewer", Scope: Scope{Kind: ScopeCluster}, Subjects: []Subject{{Kind: SubjectUser, Name: "vic"}}},
		{Role: "admin", Scope: Scope{Kind: ScopeCluster}, Subjects: []Subject{{Kind: SubjectUser, Name: "root"}}},
	}, SourceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewEngine(EngineOptions{}); err == nil {
		t.Fatal("engine without bindings accepted")
	}
	sarAllow := fixedAuthorizer{Allowed: true, Path: PathSAR}
	sarDeny := fixedAuthorizer{Allowed: false, Path: PathSAR}
	ctx := context.Background()
	write := Attributes{Resource: "settings", Verb: "write", Scope: Scope{Kind: ScopeCluster}}
	read := Attributes{Resource: "zones", Verb: "read", Scope: Scope{Kind: ScopeCluster}}
	for _, tc := range []struct {
		name     string
		sar      Authorizer
		p        Principal
		a        Attributes
		allowed  bool
		path     string
		hasMiss  bool
		reasonIn string
	}{
		{"builtin admin is local", nil, Principal{Name: "admin", BuiltinAdmin: true}, write, true, PathLocal, false, "builtin"},
		{"binding", nil, Principal{Name: "vic"}, read, true, PathBinding, false, "viewer"},
		{"binding denies", nil, Principal{Name: "vic"}, write, false, PathBinding, true, ""},
		{"nobody", nil, Principal{Name: "eve"}, read, false, PathBinding, true, ""},
		{"sar allows first", sarAllow, Principal{Name: "vic"}, write, true, PathSAR, false, ""},
		{"sar denies, binding allows (D22)", sarDeny, Principal{Name: "vic"}, read, true, PathBinding, false, ""},
		{"token scopes narrow an admin", nil, Principal{Name: "root", Restrict: []Grant{{Role: "viewer", Scope: Scope{Kind: ScopeCluster}}}}, write, false, PathNone, true, "scopes"},
		{"token scopes allow within", nil, Principal{Name: "root", Restrict: []Grant{{Role: "viewer", Scope: Scope{Kind: ScopeCluster}}}}, read, true, PathBinding, false, ""},
		{"token scopes narrow builtin admin", nil, Principal{Name: "admin", BuiltinAdmin: true, Restrict: []Grant{{Role: "viewer"}}}, write, false, PathNone, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := NewEngine(EngineOptions{Bindings: src, SAR: tc.sar})
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
	m := p.PermissionMatrix([]Grant{{Role: "viewer"}, {Role: "zone-admin", Scope: Scope{Kind: ScopeZone, Name: "home"}}}, false)
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
