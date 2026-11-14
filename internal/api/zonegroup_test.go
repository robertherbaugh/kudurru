package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/audit"
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

// zoneGetHook is a store whose DNSZone reads can be observed, delayed into
// an interleaving or failed, for the zone-group tests below.
type zoneGetHook struct {
	store.Store
	mu     sync.Mutex
	reads  map[string]int
	before func(name string, n int) error
}

func (z *zoneGetHook) Get(ctx context.Context, key store.Key, into store.Object) error {
	if key.Kind == "DNSZone" {
		z.mu.Lock()
		if z.reads == nil {
			z.reads = map[string]int{}
		}
		z.reads[key.Name]++
		n, before := z.reads[key.Name], z.before
		z.mu.Unlock()
		if before != nil {
			if err := before(key.Name, n); err != nil {
				return err
			}
		}
	}
	return z.Store.Get(ctx, key, into)
}

func (z *zoneGetHook) reset(before func(name string, n int) error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.reads, z.before = nil, before
}

func (z *zoneGetHook) total() int {
	z.mu.Lock()
	defer z.mu.Unlock()
	n := 0
	for _, c := range z.reads {
		n += c
	}
	return n
}

func zoneGroupBindings() []rbac.Binding {
	return append(apitest.StandardBindings(), rbac.Binding{Role: "zone-admin", Scope: rbac.Scope{Kind: rbac.ScopeZoneGroup, Name: "prod"}, Subjects: []rbac.Subject{{Kind: rbac.SubjectUser, Name: "grouper"}}})
}

// TestZoneGroupLabelRuleBindsToTheWrittenRead: the cluster-scope rule for
// relabeling is decided on the read the write is compare-and-swapped
// against (D385). Whichever DNSZone read of a group member's PUT the
// administrator's label removal lands before, the PUT that still carries
// the label never puts it back: it is refused, and the zone stays out of
// the group.
func TestZoneGroupLabelRuleBindsToTheWrittenRead(t *testing.T) {
	hook := &zoneGetHook{}
	h := newHarness(t, func(o *Options) {
		hook.Store = o.Store
		o.Store = hook
		o.Authorizer = apitest.Authorizer(zoneGroupBindings()...)
	})
	edit := groupZoneDoc("b", "b.example.", "prod")
	edit["spec"].(map[string]any)["defaultTTL"] = 120
	for at := 1; ; at++ {
		hook.reset(nil)
		h.seed("api", groupZone("b", "b.example.", "prod"))
		fired := false
		hook.reset(func(name string, n int) error {
			if name == "b" && n == at {
				fired = true
				var z v1alpha1.DNSZone
				if err := h.st.Get(context.Background(), store.Key{Kind: "DNSZone", Name: "b"}, &z); err != nil {
					return err
				}
				delete(z.Labels, v1alpha1.ZoneGroupLabel)
				if _, err := h.st.Apply(context.Background(), &z, store.ApplyOptions{Manager: "api"}); err != nil {
					return err
				}
			}
			return nil
		})
		r := h.do(http.MethodPut, "/api/v1/objects/dnszones/b", "grouper", edit)
		hook.reset(nil)
		if !fired {
			if at == 1 {
				t.Fatal("the PUT read zone b no time")
			}
			break
		}
		var z v1alpha1.DNSZone
		if err := h.st.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "b"}, &z); err != nil {
			t.Fatal(err)
		}
		if g := v1alpha1.ZoneGroupOf(&z); g != "" || r.Code != http.StatusForbidden {
			t.Fatalf("label removed before read %d of the PUT: HTTP %d, zone b in group %q: %s", at, r.Code, g, r.Body.String())
		}
	}
}

// TestZoneGroupResolutionReadsEachZoneOnce: resolving zone groups costs no
// store read for a principal that is already allowed, none for a zone list
// (decided on the listed objects) and one per distinct zone for a record
// set list or GET /auth/me, not one per item or per verb (D385).
func TestZoneGroupResolutionReadsEachZoneOnce(t *testing.T) {
	hook := &zoneGetHook{}
	h := newHarness(t, func(o *Options) {
		hook.Store = o.Store
		o.Store = hook
		o.Authorizer = apitest.Authorizer(zoneGroupBindings()...)
		o.Roles = func(context.Context, auth.Principal) []string { return []string{"zone-admin@zone:example"} }
	})
	h.seed("api", groupZone("a", "a.example.", "prod"), groupZone("c", "c.example.", ""), groupZone("example", "example.com.", ""))
	var seeded []store.Object
	for i := range 40 {
		seeded = append(seeded, recordSetObj("dns", fmt.Sprintf("a-%d", i), "a", ep("www.a.example", "A", "192.0.2.1")), recordSetObj("dns", fmt.Sprintf("c-%d", i), "c", ep("www.c.example", "A", "192.0.2.3")))
	}
	h.seed("api", seeded...)

	hook.reset(nil)
	var sets ObjectList
	h.do(http.MethodGet, "/api/v1/objects/dnsrecordsets", "grouper", nil).want(http.StatusOK).json(&sets)
	if len(sets.Items) != 40 {
		t.Fatalf("group member lists %d record sets, want the 40 of zone a", len(sets.Items))
	}
	if n := hook.total(); n > 2 {
		t.Fatalf("record set list read DNSZones %d times, want at most one per zone (2)", n)
	}
	hook.reset(nil)
	h.do(http.MethodGet, "/api/v1/zones", "grouper", nil).want(http.StatusOK)
	h.do(http.MethodGet, "/api/v1/objects/dnszones", "grouper", nil).want(http.StatusOK)
	if n := hook.total(); n != 0 {
		t.Fatalf("zone lists read DNSZones %d times, want 0", n)
	}
	hook.reset(nil)
	h.do(http.MethodGet, "/api/v1/objects/dnsrecordsets", "admin", nil).want(http.StatusOK)
	h.do(http.MethodGet, "/api/v1/zones/a", "admin", nil).want(http.StatusOK)
	if n := hook.total(); n > 1 {
		t.Fatalf("administrator reads read DNSZones %d times, want only the handler's read of a", n)
	}
	hook.reset(nil)
	h.do(http.MethodGet, "/api/v1/auth/me", "zoneadmin", nil).want(http.StatusOK)
	if n := hook.total(); n > 1 {
		t.Fatalf("GET /auth/me with one zone grant read DNSZones %d times, want at most 1", n)
	}
}

// TestZoneGroupUnreadableZoneIsUnavailable: a zone whose group cannot be
// read answers 503 like an unavailable authorizer, not an audited 403
// (D385); an absent zone is still in no group.
func TestZoneGroupUnreadableZoneIsUnavailable(t *testing.T) {
	hook := &zoneGetHook{}
	h := newHarness(t, func(o *Options) {
		hook.Store = o.Store
		o.Store = hook
		o.Authorizer = apitest.Authorizer(zoneGroupBindings()...)
	})
	h.seed("api", groupZone("a", "a.example.", "prod"))
	hook.reset(func(name string, n int) error {
		if name == "a" && n == 1 {
			return errors.New("apiserver unavailable")
		}
		return nil
	})
	before := len(h.audit.Events())
	h.do(http.MethodGet, "/api/v1/zones/a", "grouper", nil).want(http.StatusServiceUnavailable)
	for _, ev := range h.audit.Events()[before:] {
		if ev.Result == audit.ResultDenied {
			t.Fatalf("an unreadable zone group was audited as a denial: %+v", ev)
		}
	}
	hook.reset(nil)
	h.do(http.MethodGet, "/api/v1/zones/a", "grouper", nil).want(http.StatusOK)
	h.do(http.MethodGet, "/api/v1/zones/missing", "grouper", nil).want(http.StatusForbidden)
}

// TestZoneGroupUnreadableZoneFailsLists: a list item, or a GET /auth/me
// verb, whose decision needs the group of a zone that cannot be read fails
// the whole answer with 503 (D385). A list never answers 200 with the
// items of that zone silently left out, and the permission matrix never
// reports the outage as a missing permission.
func TestZoneGroupUnreadableZoneFailsLists(t *testing.T) {
	hook := &zoneGetHook{}
	h := newHarness(t, func(o *Options) {
		hook.Store = o.Store
		o.Store = hook
		o.Authorizer = apitest.Authorizer(zoneGroupBindings()...)
		o.Roles = func(context.Context, auth.Principal) []string { return []string{"zone-admin@zone:example"} }
	})
	h.seed("api", groupZone("a", "a.example.", "prod"), groupZone("c", "c.example.", ""), groupZone("example", "example.com.", ""))
	h.seed("api", recordSetObj("dns", "in-a", "a", ep("www.a.example", "A", "192.0.2.1")), recordSetObj("dns", "in-c", "c", ep("www.c.example", "A", "192.0.2.3")))
	down := func(zone string) {
		hook.reset(func(name string, _ int) error {
			if name == zone {
				return errors.New("apiserver unavailable")
			}
			return nil
		})
	}
	for _, path := range []string{"/api/v1/objects/dnsrecordsets", "/api/v1/objects/dnsrecordsets?namespace=dns"} {
		down("a")
		h.do(http.MethodGet, path, "grouper", nil).want(http.StatusServiceUnavailable)
	}
	down("example")
	h.do(http.MethodGet, "/api/v1/auth/me", "zoneadmin", nil).want(http.StatusServiceUnavailable)

	hook.reset(nil)
	var sets ObjectList
	h.do(http.MethodGet, "/api/v1/objects/dnsrecordsets", "grouper", nil).want(http.StatusOK).json(&sets)
	if len(sets.Items) != 1 {
		t.Fatalf("group member lists %d record sets once the zone is readable, want the one of zone a", len(sets.Items))
	}
	var me Me
	h.do(http.MethodGet, "/api/v1/auth/me", "zoneadmin", nil).want(http.StatusOK).json(&me)
	if !slices.Contains(me.ScopedPermissions["zone:example"]["records"], "write") {
		t.Fatalf("zone grant once the zone is readable: scoped=%v", me.ScopedPermissions)
	}
}

// TestZoneDeletedDuringPutIsNotRecreated: a zone that an administrator
// deletes while a zone-group member's or a zone-admin's PUT of it runs is
// not created again by that PUT, whichever DNSZone read of the PUT the
// deletion lands before (D385). Creating a zone needs zones.write at
// cluster scope, and the write decides that on its own read: before it,
// the authorization read saw the zone and admitted the PUT at zone scope.
func TestZoneDeletedDuringPutIsNotRecreated(t *testing.T) {
	hook := &zoneGetHook{}
	h := newHarness(t, func(o *Options) {
		hook.Store = o.Store
		o.Store = hook
		o.Authorizer = apitest.Authorizer(zoneGroupBindings()...)
	})
	for _, tc := range []struct{ user, zone, apex, group string }{
		{"grouper", "b", "b.example.", "prod"},
		{"zoneadmin", "example", "example.com.", ""},
	} {
		edit := groupZoneDoc(tc.zone, tc.apex, tc.group)
		edit["spec"].(map[string]any)["defaultTTL"] = 120
		for at := 1; ; at++ {
			hook.reset(nil)
			h.seed("api", groupZone(tc.zone, tc.apex, tc.group))
			fired := false
			hook.reset(func(name string, n int) error {
				if name == tc.zone && n == at {
					fired = true
					if err := h.st.Delete(context.Background(), store.Key{Kind: "DNSZone", Name: tc.zone}, store.DeleteOptions{Manager: "api"}); err != nil {
						return err
					}
				}
				return nil
			})
			r := h.do(http.MethodPut, "/api/v1/objects/dnszones/"+tc.zone, tc.user, edit)
			hook.reset(nil)
			if !fired {
				if at == 1 {
					t.Fatalf("%s: the PUT read zone %s no time", tc.user, tc.zone)
				}
				break
			}
			var z v1alpha1.DNSZone
			err := h.st.Get(t.Context(), store.Key{Kind: "DNSZone", Name: tc.zone}, &z)
			if !errors.Is(err, store.ErrNotFound) || (r.Code != http.StatusForbidden && r.Code != http.StatusConflict) {
				t.Fatalf("%s: zone %s deleted before read %d of the PUT: HTTP %d, read after: %v: %s", tc.user, tc.zone, at, r.Code, err, r.Body.String())
			}
		}
	}
	// An administrator still creates the zone with the same PUT.
	if err := h.st.Delete(t.Context(), store.Key{Kind: "DNSZone", Name: "b"}, store.DeleteOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	h.do(http.MethodPut, "/api/v1/objects/dnszones/b", "admin", groupZoneDoc("b", "b.example.", "prod")).want(http.StatusCreated)
}
