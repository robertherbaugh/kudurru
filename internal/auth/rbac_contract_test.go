package auth_test

import (
	"testing"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/rbac"
)

// The binding subject deliberately names a namespace/name across both
// service-account providers. Authentication and account lifecycle remain
// provider-specific; this tests the shared authorization contract after them.
func TestServiceAccountProviderOverlapAndTokenScope(t *testing.T) {
	perms, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := rbac.NewBindingAuthorizer(perms, []rbac.Binding{{
		Role: "zone-admin", Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: "home"},
		Subjects: []rbac.Subject{{Kind: rbac.SubjectServiceAccount, Namespace: "dns", Name: "automation"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := rbac.NewEngine(rbac.EngineOptions{Permissions: perms, Bindings: bindings})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []auth.PrincipalKind{auth.KindServiceAccount, auth.KindKubernetesServiceAccount} {
		t.Run(string(kind), func(t *testing.T) {
			for _, test := range []struct {
				name, account, zone, verb string
				scopes                    []string
				allowed                   bool
			}{
				{name: "same account", account: "dns/automation", zone: "home", verb: "write", allowed: true},
				{name: "other namespace", account: "other/automation", zone: "home", verb: "write"},
				{name: "other name", account: "dns/other", zone: "home", verb: "write"},
				{name: "display name alone", zone: "home", verb: "write"},
				{name: "other zone", account: "dns/automation", zone: "other", verb: "write"},
				{name: "scope narrows grant", account: "dns/automation", zone: "home", verb: "write", scopes: []string{"viewer"}},
				{name: "scope permits read", account: "dns/automation", zone: "home", verb: "read", scopes: []string{"viewer"}, allowed: true},
				{name: "scope cannot create grant", account: "dns/automation", zone: "other", verb: "write", scopes: []string{"admin"}},
				{name: "malformed scope", account: "dns/automation", zone: "home", verb: "read", scopes: []string{"@"}},
			} {
				t.Run(test.name, func(t *testing.T) {
					principal := auth.RBACPrincipal(auth.Principal{Kind: kind, Name: "system:serviceaccount:dns:automation", ServiceAccount: test.account, Scopes: test.scopes})
					decision, err := engine.Authorize(t.Context(), principal, rbac.Attributes{Resource: "records", Verb: test.verb, Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: test.zone}})
					if err != nil || decision.Allowed != test.allowed || (test.allowed && decision.Path != rbac.PathBinding) {
						t.Fatalf("binding/scope contract: allowed=%v path=%s error=%v", decision.Allowed, decision.Path, err)
					}
				})
			}
		})
	}
}

// TestZoneGroupBindingTokenScopeContract: the shared contract after
// authentication for a zonegroup binding (D385). The binding covers the zones
// whose resolved group names it; token scopes (Restrict) intersect with it at
// zone or group scope and never widen it to another group.
func TestZoneGroupBindingTokenScopeContract(t *testing.T) {
	perms, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := rbac.NewBindingAuthorizer(perms, []rbac.Binding{{
		Role: "zone-admin", Scope: rbac.Scope{Kind: rbac.ScopeZoneGroup, Name: "prod"},
		Subjects: []rbac.Subject{{Kind: rbac.SubjectUser, Name: "frank"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := rbac.NewEngine(rbac.EngineOptions{Permissions: perms, Bindings: bindings})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, zone, group, verb string
		scopes                  []string
		allowed                 bool
	}{
		{name: "member zone", zone: "a", group: "prod", verb: "write", allowed: true},
		{name: "zone in another group", zone: "b", group: "dev", verb: "write"},
		{name: "zone in no group", zone: "c", verb: "read"},
		{name: "token at the group", zone: "a", group: "prod", verb: "write", scopes: []string{"zone-admin@zonegroup:prod"}, allowed: true},
		{name: "token narrows the role", zone: "a", group: "prod", verb: "write", scopes: []string{"viewer@zonegroup:prod"}},
		{name: "token narrowed role reads", zone: "a", group: "prod", verb: "read", scopes: []string{"viewer@zonegroup:prod"}, allowed: true},
		{name: "token at one member zone", zone: "a", group: "prod", verb: "write", scopes: []string{"zone-admin@zone:a"}, allowed: true},
		{name: "token at another member zone", zone: "a", group: "prod", verb: "write", scopes: []string{"zone-admin@zone:a2"}},
		{name: "token at another group", zone: "a", group: "prod", verb: "write", scopes: []string{"zone-admin@zonegroup:dev"}},
		{name: "cluster token cannot widen", zone: "b", group: "dev", verb: "write", scopes: []string{"admin"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			principal := auth.RBACPrincipal(auth.Principal{Kind: auth.KindUser, Name: "frank", Scopes: test.scopes})
			decision, err := engine.Authorize(t.Context(), principal, rbac.Attributes{Resource: "records", Verb: test.verb, Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: test.zone}, ZoneGroup: test.group})
			if err != nil || decision.Allowed != test.allowed {
				t.Fatalf("zone group contract: allowed=%v path=%s error=%v", decision.Allowed, decision.Path, err)
			}
		})
	}
}
