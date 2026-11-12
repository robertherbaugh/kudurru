package api

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func groupZone(name, apex, group string) *v1alpha1.DNSZone {
	z := zoneObj(name, apex)
	if group != "" {
		z.Labels = map[string]string{v1alpha1.ZoneGroupLabel: group}
	}
	return z
}

func groupZoneDoc(name, apex, group string) map[string]any {
	doc := zoneDoc(name, apex)
	if group != "" {
		doc["metadata"].(map[string]any)["labels"] = map[string]any{v1alpha1.ZoneGroupLabel: group}
	}
	return doc
}

// TestZoneGroupBindingThroughTheRouter: a zone-admin bound at zonegroup:prod
// reads and writes exactly the zones labeled kudurru.io/zonegroup=prod and
// their record sets, sees only them in lists, cannot move a zone into or out
// of a group, and loses a zone when an administrator removes its label
// (D385). The store-backed router path is the Kubernetes driver's.
func TestZoneGroupBindingThroughTheRouter(t *testing.T) {
	bindings := append(apitest.StandardBindings(), rbac.Binding{Role: "zone-admin", Scope: rbac.Scope{Kind: rbac.ScopeZoneGroup, Name: "prod"}, Subjects: []rbac.Subject{{Kind: rbac.SubjectUser, Name: "grouper"}}})
	h := newHarness(t, func(o *Options) { o.Authorizer = apitest.Authorizer(bindings...) })
	h.seed("api", groupZone("a", "a.example.", "prod"), groupZone("b", "b.example.", "prod"), groupZone("c", "c.example.", ""), groupZone("d", "d.example.", "dev"))
	h.seed("api", recordSetObj("dns", "in-a", "a", ep("www.a.example", "A", "192.0.2.1")), recordSetObj("dns", "in-c", "c", ep("www.c.example", "A", "192.0.2.3")))
	listed := func() []string {
		t.Helper()
		var list struct {
			Items []ZoneSummary `json:"items"`
		}
		h.do(http.MethodGet, "/api/v1/zones", "grouper", nil).want(http.StatusOK).json(&list)
		var names []string
		for _, z := range list.Items {
			if z.ZoneGroup != "prod" {
				t.Fatalf("zone %s listed with group %q", z.Name, z.ZoneGroup)
			}
			names = append(names, z.Name)
		}
		return names
	}
	if got := listed(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("group member lists %v, want [a b]", got)
	}
	h.do(http.MethodGet, "/api/v1/zones/a", "grouper", nil).want(http.StatusOK)
	h.do(http.MethodGet, "/api/v1/zones/c", "grouper", nil).want(http.StatusForbidden)
	h.do(http.MethodGet, "/api/v1/zones/d", "grouper", nil).want(http.StatusForbidden)
	h.do(http.MethodPost, "/api/v1/zones/a/records", "grouper", ep("api", "A", "192.0.2.10")).want(http.StatusCreated)
	h.do(http.MethodPost, "/api/v1/zones/c/records", "grouper", ep("api", "A", "192.0.2.10")).want(http.StatusForbidden)
	h.do(http.MethodGet, "/api/v1/zones/b/records", "grouper", nil).want(http.StatusOK)
	h.do(http.MethodGet, "/api/v1/zones/d/records", "grouper", nil).want(http.StatusForbidden)
	var sets ObjectList
	h.do(http.MethodGet, "/api/v1/objects/dnsrecordsets", "grouper", nil).want(http.StatusOK).json(&sets)
	for _, item := range sets.Items {
		if zone := item["spec"].(map[string]any)["zoneRef"].(map[string]any)["name"]; zone != "a" {
			t.Fatalf("record set list shows a record set of zone %v", zone)
		}
	}
	if len(sets.Items) != 2 {
		t.Fatalf("record set list has %d items, want the two of zone a", len(sets.Items))
	}
	h.do(http.MethodGet, "/api/v1/objects/dnsrecordsets/dns/in-c", "grouper", nil).want(http.StatusForbidden)

	// Editing a member zone keeps it in the group; moving it out, moving
	// another zone in or creating a member zone needs cluster scope.
	edit := groupZoneDoc("a", "a.example.", "prod")
	edit["spec"].(map[string]any)["defaultTTL"] = 120
	h.do(http.MethodPut, "/api/v1/objects/dnszones/a", "grouper", edit).want(http.StatusOK)
	r := h.do(http.MethodPut, "/api/v1/objects/dnszones/a", "grouper", groupZoneDoc("a", "a.example.", "")).want(http.StatusForbidden)
	var e struct {
		Error struct {
			Hint ForbiddenHint `json:"hint"`
		} `json:"error"`
	}
	r.json(&e)
	if e.Error.Hint.Missing != (Missing{Resource: "zones", Verb: "write", Scope: "cluster"}) {
		t.Fatalf("leaving the group: missing %+v", e.Error.Hint.Missing)
	}
	h.do(http.MethodPut, "/api/v1/objects/dnszones/a", "grouper", groupZoneDoc("a", "a.example.", "dev")).want(http.StatusForbidden)
	h.do(http.MethodPut, "/api/v1/objects/dnszones/c", "grouper", groupZoneDoc("c", "c.example.", "prod")).want(http.StatusForbidden)
	h.do(http.MethodPut, "/api/v1/objects/dnszones/e", "grouper", groupZoneDoc("e", "e.example.", "prod")).want(http.StatusForbidden)
	// A zone-admin of one zone cannot put it into a group either.
	h.seed("api", groupZone("example", "example.com.", ""))
	h.do(http.MethodPut, "/api/v1/objects/dnszones/example", "zoneadmin", groupZoneDoc("example", "example.com.", "prod")).want(http.StatusForbidden)
	h.do(http.MethodPut, "/api/v1/objects/dnszones/c", "admin", groupZoneDoc("c", "c.example.", "prod")).want(http.StatusOK)
	if got := listed(); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("after the admin labeled c: %v", got)
	}

	// The administrator removes b's label: the group binding no longer
	// covers b, its records or its record sets.
	h.do(http.MethodPut, "/api/v1/objects/dnszones/b", "admin", groupZoneDoc("b", "b.example.", "")).want(http.StatusOK)
	if got := listed(); !slices.Equal(got, []string{"a", "c"}) {
		t.Fatalf("after the admin unlabeled b: %v", got)
	}
	h.do(http.MethodGet, "/api/v1/zones/b", "grouper", nil).want(http.StatusForbidden)
	h.do(http.MethodPost, "/api/v1/zones/b/records", "grouper", ep("api", "A", "192.0.2.11")).want(http.StatusForbidden)
	h.do(http.MethodPut, "/api/v1/objects/dnsrecordsets/dns/in-b", "grouper", recordSetObj("dns", "in-b", "b", ep("www.b.example", "A", "192.0.2.2"))).want(http.StatusForbidden)
	h.do(http.MethodDelete, "/api/v1/zones/b", "grouper", nil).want(http.StatusForbidden)
	var gone v1alpha1.DNSRecordSet
	if err := h.st.Get(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "dns", Name: "in-b"}, &gone); err == nil {
		t.Fatal("denied record set write reached the store")
	}
}

// TestMeZoneGroupScopedPermissions: GET /auth/me reports what a scoped grant
// allows at its scope, for a zonegroup grant as for a zone grant (D385).
func TestMeZoneGroupScopedPermissions(t *testing.T) {
	bindings := append(apitest.StandardBindings(), rbac.Binding{Role: "zone-admin", Scope: rbac.Scope{Kind: rbac.ScopeZoneGroup, Name: "prod"}, Subjects: []rbac.Subject{{Kind: rbac.SubjectUser, Name: "grouper"}}})
	h := newHarness(t, func(o *Options) {
		o.Authorizer = apitest.Authorizer(bindings...)
		o.Roles = func(_ context.Context, p auth.Principal) []string {
			if p.Name == "grouper" {
				return []string{"zone-admin@zonegroup:prod"}
			}
			return []string{"zone-admin@zone:example"}
		}
	})
	var me Me
	h.do(http.MethodGet, "/api/v1/auth/me", "grouper", nil).want(http.StatusOK).json(&me)
	if len(me.Permissions) != 0 || !slices.Contains(me.ScopedPermissions["zonegroup:prod"]["records"], "write") || !slices.Contains(me.ScopedPermissions["zonegroup:prod"]["zones"], "transfer") || len(me.ScopedPermissions) != 1 {
		t.Fatalf("zone group grant: permissions=%v scoped=%v", me.Permissions, me.ScopedPermissions)
	}
	me = Me{}
	h.do(http.MethodGet, "/api/v1/auth/me", "zoneadmin", nil).want(http.StatusOK).json(&me)
	if !slices.Contains(me.ScopedPermissions["zone:example"]["records"], "write") || len(me.ScopedPermissions) != 1 {
		t.Fatalf("zone grant: scoped=%v", me.ScopedPermissions)
	}
}
