package rbac

import (
	"context"
	"testing"
)

// TestScopeAdmitsZoneGroup is the D385 table: a zonegroup scope covers a zone
// attribute only when the zone's resolved group equals the scope name, and a
// zone in no group is covered by no zonegroup scope.
func TestScopeAdmitsZoneGroup(t *testing.T) {
	prod := Scope{Kind: ScopeZoneGroup, Name: "prod"}
	zone := func(name, group string) Attributes {
		return Attributes{Resource: "records", Verb: "write", Scope: Scope{Kind: ScopeZone, Name: name}, ZoneGroup: group}
	}
	for _, c := range []struct {
		name  string
		scope Scope
		a     Attributes
		want  bool
	}{
		{"member zone", prod, zone("a", "prod"), true},
		{"zone of another group", prod, zone("a", "dev"), false},
		{"zone in no group", prod, zone("a", ""), false},
		{"group named like the zone", Scope{Kind: ScopeZoneGroup, Name: "a"}, zone("a", ""), false},
		{"same group scope", prod, Attributes{Scope: prod}, true},
		{"other group scope", prod, Attributes{Scope: Scope{Kind: ScopeZoneGroup, Name: "dev"}}, false},
		{"cluster attribute", prod, Attributes{Scope: Scope{Kind: ScopeCluster}, ZoneGroup: "prod"}, false},
		{"group stated without a zone", prod, Attributes{Scope: Scope{Kind: ScopeZone}, ZoneGroup: "prod"}, false},
		{"client group attribute", prod, Attributes{Scope: Scope{Kind: ScopeClientGroup, Name: "prod"}, ZoneGroup: "prod"}, false},
		{"empty group name", Scope{Kind: ScopeZoneGroup}, zone("a", ""), false},
		{"zone scope ignores the group", Scope{Kind: ScopeZone, Name: "a"}, zone("a", "prod"), true},
		{"zone scope of another zone", Scope{Kind: ScopeZone, Name: "b"}, zone("a", "prod"), false},
		{"cluster covers a member zone", Scope{Kind: ScopeCluster}, zone("a", "prod"), true},
		{"client group never covers a zone", Scope{Kind: ScopeClientGroup, Name: "prod"}, zone("a", "prod"), false},
	} {
		if got := c.scope.Admits(c.a); got != c.want {
			t.Errorf("%s: Admits=%v, want %v", c.name, got, c.want)
		}
	}
}

// TestZoneGroupBindingsAndRestrictions runs the same rule through the
// binding authorizer and the engine: a group binding grants its role on
// member zones only, and API token scopes (Restrict) narrow it in both
// directions, at zone and at group scope.
func TestZoneGroupBindingsAndRestrictions(t *testing.T) {
	p, err := LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := NewBindingAuthorizer(p, []Binding{
		{Role: "zone-admin", Scope: Scope{Kind: ScopeZoneGroup, Name: "prod"}, Subjects: []Subject{{Kind: SubjectUser, Name: "carol"}}},
		{Role: "viewer", Scope: Scope{Kind: ScopeZoneGroup, Name: "dev"}, Subjects: []Subject{{Kind: SubjectGroup, Name: "corp:/devs"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewBindingAuthorizer(p, []Binding{{Role: "zone-admin", Scope: Scope{Kind: ScopeZoneGroup, Name: "prod"}}}); err != nil {
		t.Fatal("zone-admin must be bindable at zonegroup scope:", err)
	}
	engine, err := NewEngine(EngineOptions{Permissions: p, Bindings: bindings})
	if err != nil {
		t.Fatal(err)
	}
	carol := Principal{Name: "carol"}
	dave := Principal{Name: "dave", Groups: []string{"corp:/devs"}}
	scoped := func(p Principal, wire ...string) Principal {
		for _, w := range wire {
			g, err := ParseGrant(w)
			if err != nil {
				t.Fatal(err)
			}
			p.Restrict = append(p.Restrict, g)
		}
		return p
	}
	at := func(resource, verb, zone, group string) Attributes {
		return Attributes{Resource: resource, Verb: verb, Scope: Scope{Kind: ScopeZone, Name: zone}, ZoneGroup: group}
	}
	for _, c := range []struct {
		name string
		p    Principal
		a    Attributes
		want bool
	}{
		{"group admin writes records of a member zone", carol, at("records", "write", "a", "prod"), true},
		{"group admin updates a member zone", carol, at("zones", "write", "a", "prod"), true},
		{"group admin cannot touch a zone of another group", carol, at("records", "write", "b", "dev"), false},
		{"group admin cannot touch a zone in no group", carol, at("records", "read", "c", ""), false},
		{"group admin cannot create zones", carol, Attributes{Resource: "zones", Verb: "write", Scope: Scope{Kind: ScopeCluster}}, false},
		{"group viewer through an IdP group reads", dave, at("records", "read", "b", "dev"), true},
		{"group viewer cannot write", dave, at("records", "write", "b", "dev"), false},
		{"group scope of the binding itself", carol, Attributes{Resource: "zones", Verb: "transfer", Scope: Scope{Kind: ScopeZoneGroup, Name: "prod"}}, true},
		{"token narrowed to viewer at the group reads", scoped(carol, "viewer@zonegroup:prod"), at("records", "read", "a", "prod"), true},
		{"token narrowed to viewer at the group cannot write", scoped(carol, "viewer@zonegroup:prod"), at("records", "write", "a", "prod"), false},
		{"token narrowed to one member zone", scoped(carol, "zone-admin@zone:a"), at("records", "write", "a", "prod"), true},
		{"token narrowed to one member zone excludes the others", scoped(carol, "zone-admin@zone:a2"), at("records", "write", "a", "prod"), false},
		{"token of another group grants nothing", scoped(carol, "zone-admin@zonegroup:dev"), at("records", "write", "a", "prod"), false},
		{"token group scope cannot widen the binding", scoped(carol, "zone-admin@zonegroup:dev"), at("records", "write", "b", "dev"), false},
	} {
		for name, az := range map[string]Authorizer{"bindings": bindings, "engine": engine} {
			if name == "bindings" && len(c.p.Restrict) > 0 {
				continue // Restrict is the engine's step.
			}
			d, err := az.Authorize(context.Background(), c.p, c.a)
			if err != nil {
				t.Fatal(err)
			}
			if d.Allowed != c.want {
				t.Errorf("%s (%s): allowed=%v (%s), want %v", c.name, name, d.Allowed, d.Reason, c.want)
			}
		}
	}
	m := p.PermissionMatrix([]Grant{{Role: "zone-admin", Scope: Scope{Kind: ScopeZoneGroup, Name: "prod"}}}, false)
	if len(m["zonegroup:prod"]) == 0 || len(m["cluster"]) != 0 {
		t.Fatalf("permission matrix of a group grant: %v", m)
	}
}
