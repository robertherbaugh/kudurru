package rbac

import (
	"context"
	"testing"
)

func TestPermissionsTableIsConsistent(t *testing.T) {
	p, err := LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"viewer", "auditor", "operator", "zone-admin", "admin"} {
		if _, ok := p.Roles[role]; !ok {
			t.Errorf("built-in role %s missing", role)
		}
	}
	// The matrix in docs/architecture.md §8.5, spot-checked.
	cases := []struct {
		role, res, verb string
		want            bool
	}{
		{"viewer", "zones", "read", true},
		{"viewer", "records", "write", false},
		{"viewer", "querylog", "read", false},
		{"auditor", "querylog", "export", true},
		{"auditor", "querylog", "purge", false},
		{"auditor", "audit", "read", true},
		{"operator", "records", "write", true},
		{"operator", "blocking", "toggle", true},
		{"operator", "zones", "write", false},
		{"operator", "settings", "write", false},
		{"zone-admin", "zones", "transfer", true},
		{"zone-admin", "blocklists", "read", false},
		{"admin", "cluster", "promote", true},
		{"admin", "node", "config", true},
	}
	for _, c := range cases {
		if got := p.Grants(c.role, c.res, c.verb); got != c.want {
			t.Errorf("%s %s.%s = %v, want %v", c.role, c.res, c.verb, got, c.want)
		}
	}
}

func TestBindingAuthorizer(t *testing.T) {
	p, err := LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	az, err := NewBindingAuthorizer(p, []Binding{
		{Role: "admin", Scope: Scope{Kind: ScopeCluster}, Subjects: []Subject{{Kind: SubjectGroup, Name: "corp:/dns-admins"}}},
		{Role: "zone-admin", Scope: Scope{Kind: ScopeZone, Name: "home-example-com"}, Subjects: []Subject{{Kind: SubjectUser, Name: "alice"}}},
		{Role: "viewer", Scope: Scope{Kind: ScopeCluster}, Subjects: []Subject{{Kind: SubjectServiceAccount, Name: "grafana", Namespace: "monitoring"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	alice := Principal{Name: "alice", Groups: []string{"corp:/staff"}}
	bob := Principal{Name: "bob", Groups: []string{"corp:/dns-admins"}}
	grafana := Principal{Name: "system:serviceaccount:monitoring:grafana", ServiceAccount: "monitoring/grafana"}

	cases := []struct {
		name string
		p    Principal
		a    Attributes
		want bool
	}{
		{"alice writes records in her zone", alice, Attributes{Resource: "records", Verb: "write", Scope: Scope{Kind: ScopeZone, Name: "home-example-com"}}, true},
		{"alice cannot write records in another zone", alice, Attributes{Resource: "records", Verb: "write", Scope: Scope{Kind: ScopeZone, Name: "other"}}, false},
		{"alice cannot create zones cluster-wide", alice, Attributes{Resource: "zones", Verb: "write", Scope: Scope{Kind: ScopeCluster}}, false},
		{"alice cannot toggle blocking", alice, Attributes{Resource: "blocking", Verb: "toggle", Scope: Scope{Kind: ScopeCluster}}, false},
		{"bob is admin via group", bob, Attributes{Resource: "settings", Verb: "write", Scope: Scope{Kind: ScopeCluster}}, true},
		{"bob admin covers zone scope", bob, Attributes{Resource: "records", Verb: "delete", Scope: Scope{Kind: ScopeZone, Name: "any"}}, true},
		{"grafana reads stats", grafana, Attributes{Resource: "stats", Verb: "read", Scope: Scope{Kind: ScopeCluster}}, true},
		{"grafana cannot read query log", grafana, Attributes{Resource: "querylog", Verb: "read", Scope: Scope{Kind: ScopeCluster}}, false},
	}
	for _, c := range cases {
		d, err := az.Authorize(ctx, c.p, c.a)
		if err != nil {
			t.Fatal(err)
		}
		if d.Allowed != c.want {
			t.Errorf("%s: allowed=%v (reason %q), want %v", c.name, d.Allowed, d.Reason, c.want)
		}
		if !d.Allowed && d.Missing == nil {
			t.Errorf("%s: denied decisions must carry Missing for the 403 body", c.name)
		}
	}

	if _, err := NewBindingAuthorizer(p, []Binding{{Role: "zone-admin", Scope: Scope{Kind: ScopeCluster}}}); err == nil {
		t.Error("zone-admin at cluster scope must be rejected")
	}
	if _, err := NewBindingAuthorizer(p, []Binding{{Role: "root"}}); err == nil {
		t.Error("unknown role must be rejected")
	}
}

func TestChainFirstAllowWins(t *testing.T) {
	deny := authorizerFunc(func(Principal, Attributes) Decision { return Decision{Path: "sar"} })
	allow := authorizerFunc(func(Principal, Attributes) Decision { return Decision{Allowed: true, Path: "binding"} })
	d, err := Chain{deny, allow}.Authorize(context.Background(), Principal{}, Attributes{})
	if err != nil || !d.Allowed || d.Path != "binding" {
		t.Fatalf("chain should report the granting path: %+v %v", d, err)
	}
	d, _ = Chain{deny}.Authorize(context.Background(), Principal{}, Attributes{})
	if d.Allowed {
		t.Fatal("all-deny chain must deny")
	}
}

type authorizerFunc func(Principal, Attributes) Decision

func (f authorizerFunc) Authorize(_ context.Context, p Principal, a Attributes) (Decision, error) {
	return f(p, a), nil
}
