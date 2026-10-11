package oidc

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/rbac"
)

func testBindings() []rbac.Binding {
	return []rbac.Binding{
		{Role: "admin", Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Subjects: []rbac.Subject{{Kind: rbac.SubjectGroup, Name: "corp:/dns-admins"}}},
		{Role: "viewer", Subjects: []rbac.Subject{{Kind: rbac.SubjectGroup, Name: "corp:/staff"}}},
		{Role: "zone-admin", Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: "home-example-com"}, Subjects: []rbac.Subject{{Kind: rbac.SubjectUser, Name: "corp:alice"}}},
		{Role: "operator", Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Subjects: []rbac.Subject{{Kind: rbac.SubjectServiceAccount, Name: "corp:alice", Namespace: "x"}}},
		// Duplicate grant through a second binding.
		{Role: "viewer", Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Subjects: []rbac.Subject{{Kind: rbac.SubjectUser, Name: "corp:alice"}}},
	}
}

func TestBindingResolver(t *testing.T) {
	r := BindingResolver{Bindings: testBindings}
	cases := []struct {
		name string
		p    auth.Principal
		want []string
	}{
		{"group + user + dedupe", auth.Principal{Name: "corp:alice", Groups: []string{"corp:/staff", "corp:/dns-admins"}},
			[]string{"admin", "viewer", "zone-admin@zone:home-example-com"}},
		{"group only", auth.Principal{Name: "corp:bob", Groups: []string{"corp:/staff"}}, []string{"viewer"}},
		{"nothing", auth.Principal{Name: "corp:eve", Groups: []string{"corp:/contractors"}}, nil},
		{"unprefixed group does not match", auth.Principal{Name: "eve", Groups: []string{"/dns-admins"}}, nil},
		{"service account subject never matches a user", auth.Principal{Name: "corp:carol"}, nil},
		{"empty name matches no user subject", auth.Principal{}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			grants, err := r.ResolveRoles(context.Background(), c.p)
			if err != nil {
				t.Fatal(err)
			}
			if got := auth.GrantStrings(grants); !reflect.DeepEqual(got, c.want) {
				t.Errorf("grants = %v, want %v", got, c.want)
			}
		})
	}
}

// TestBindingResolverAgreesWithAuthorizer pins BindingResolver's subject
// matching to rbac.BindingAuthorizer's: for every binding and principal,
// the resolver yields the binding's grant exactly when the authorizer,
// holding only that binding, allows a permission the role has at its scope.
func TestBindingResolverAgreesWithAuthorizer(t *testing.T) {
	perms, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	probe := map[string][2]string{ // role -> a permission it grants
		"admin": {"settings", "write"}, "viewer": {"zones", "read"},
		"operator": {"records", "write"}, "zone-admin": {"zones", "transfer"}, "auditor": {"audit", "read"},
	}
	principals := []auth.Principal{
		{Name: "corp:alice", Groups: []string{"corp:/staff", "corp:/dns-admins"}},
		{Name: "corp:bob", Groups: []string{"corp:/staff"}},
		{Name: "corp:eve"},
		{Name: "x", Groups: []string{"corp:alice"}}, // group named like a user
	}
	for i, b := range testBindings() {
		az, err := rbac.NewBindingAuthorizer(perms, []rbac.Binding{b})
		if err != nil {
			t.Fatal(err)
		}
		res := BindingResolver{Bindings: func() []rbac.Binding { return []rbac.Binding{b} }}
		pr := probe[b.Role]
		for _, p := range principals {
			grants, _ := res.ResolveRoles(context.Background(), p)
			d, err := az.Authorize(context.Background(), rbac.Principal{Name: p.Name, Groups: p.Groups},
				rbac.Attributes{Resource: pr[0], Verb: pr[1], Scope: b.Scope})
			if err != nil {
				t.Fatal(err)
			}
			if (len(grants) == 1) != d.Allowed {
				t.Errorf("binding %d (%s) principal %s: resolver %v, authorizer %v", i, b.Role, p.Name, grants, d.Allowed)
			}
		}
	}
}

func TestRoleResolverFunc(t *testing.T) {
	var r RoleResolver = RoleResolverFunc(func(context.Context, auth.Principal) ([]auth.Grant, error) {
		return nil, fmt.Errorf("boom")
	})
	if _, err := r.ResolveRoles(context.Background(), auth.Principal{}); err == nil {
		t.Fatal("error not propagated")
	}
}
