package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func zoneCommandHarness(t *testing.T, authorize func([]rbac.Attributes) error) (*harness, *sqlite.Store) {
	t.Helper()
	h, st := objectCommandHarness(t, nil)
	h.srv.opts.ZoneCommand = func(ctx context.Context, target ZoneCommandTarget, prepare ZoneCommandPrepare) error {
		return st.Command(ctx, st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			plan, err := prepare(ctx, tx)
			if plan != nil {
				defer plan.Release()
			}
			if err != nil {
				return err
			}
			if !plan.PreparedFor(tx, target) {
				return errUnavailable("bad plan")
			}
			if authorize != nil {
				if err := authorize(plan.Requirements()); err != nil {
					return err
				}
			}
			return plan.Apply(ctx, tx)
		})
	}
	return h, st
}

func zoneSeed(t *testing.T, st *sqlite.Store, owner string, objects ...store.Object) {
	t.Helper()
	for _, obj := range objects {
		if _, err := st.Apply(t.Context(), obj, store.ApplyOptions{Manager: owner, Force: true}); err != nil {
			t.Fatal(err)
		}
	}
}

func zoneRecord(t *testing.T, st *sqlite.Store, namespace, name string) *public.DNSRecordSet {
	t.Helper()
	var rs public.DNSRecordSet
	if err := st.Get(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: namespace, Name: name}, &rs); err != nil {
		t.Fatal(err)
	}
	return &rs
}

func TestZoneCommandEndpointUsesOnlyCurrentSnapshot(t *testing.T) {
	deleting := false
	var attrs []rbac.Attributes
	h, st := zoneCommandHarness(t, func(a []rbac.Attributes) error {
		attrs = a
		if deleting && (len(a) != 1 || a[0].Resource != "records" || a[0].Verb != "delete") {
			return errForbidden(Missing{Resource: "records", Verb: "write"}, "")
		}
		return nil
	})
	old := recordSetObj("default", "api-example", "example", ep("sibling.current.test", "A", "192.0.2.5"))
	old.Labels = map[string]string{"keep": "this"}
	zoneSeed(t, st, store.ManagerAPI, zoneObj("example", "current.test."), old)
	h.seed(store.ManagerAPI, zoneObj("example", "wrong.test."))
	before, _ := st.ReadGeneration(t.Context())
	h.do(http.MethodPost, "/api/v1/zones/example/records", "operator", ep("www", "A", "192.0.2.8")).want(http.StatusCreated)
	after, _ := st.ReadGeneration(t.Context())
	if after.Revision != before.Revision+1 {
		t.Fatal("endpoint mutation was not one command")
	}
	written := zoneRecord(t, st, "default", "api-example")
	if len(written.Spec.Endpoints) != 2 || written.Spec.Endpoints[1].DNSName != "www.current.test" || written.Labels["keep"] != "this" {
		t.Fatal("merge used live Store or dropped a sibling", written)
	}
	h.do(http.MethodPost, "/api/v1/zones/example/records", "operator", ep("www", "A", "192.0.2.8")).want(http.StatusOK)
	unchanged, _ := st.ReadGeneration(t.Context())
	if !after.Generation().Same(unchanged.Generation()) {
		t.Fatal("identical endpoint changed generation")
	}
	deleting = true
	h.do(http.MethodDelete, "/api/v1/zones/example/records/www/A", "operator", nil).want(http.StatusNoContent)
	written = zoneRecord(t, st, "default", "api-example")
	if len(written.Spec.Endpoints) != 1 || !reflect.DeepEqual(written.Spec.Endpoints[0], old.Spec.Endpoints[0]) {
		t.Fatal("partial deletion changed a surviving endpoint")
	}
	if attrs[0].Scope != (rbac.Scope{Kind: rbac.ScopeZone, Name: "example"}) {
		t.Fatal("wrong delete scope", attrs)
	}
}

func TestZoneCommandCascadeChecksEveryOwnerAndDryRun(t *testing.T) {
	var attrs []rbac.Attributes
	h, st := zoneCommandHarness(t, func(a []rbac.Attributes) error { attrs = a; return nil })
	zoneSeed(t, st, store.ManagerAPI, zoneObj("example", "example.test."), recordSetObj("default", "first", "example", ep("a", "A", "192.0.2.1")))
	blocked := recordSetObj("other", "last", "example", ep("b", "A", "192.0.2.2"))
	zoneSeed(t, st, store.ManagerFile("zones.yaml"), blocked)
	before, _ := st.ReadGeneration(t.Context())
	h.do(http.MethodDelete, "/api/v1/zones/example", "admin", nil).want(http.StatusConflict)
	after, _ := st.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("late managed owner left partial deletion")
	}
	zoneSeed(t, st, store.ManagerAPI, blocked)
	before, _ = st.ReadGeneration(t.Context())
	var result ZoneDeleteResult
	h.do(http.MethodDelete, "/api/v1/zones/example?dryRun=true", "admin", nil).want(http.StatusOK).json(&result)
	after, _ = st.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) || !result.DryRun || result.RecordSetCount != 2 || len(attrs) != 2 {
		t.Fatal("dry run skipped current references/permissions or changed data", result, attrs)
	}
	h.do(http.MethodDelete, "/api/v1/zones/example", "admin", nil, "If-Match", "wrong").want(http.StatusPreconditionFailed)
	h.do(http.MethodDelete, "/api/v1/zones/example", "admin", nil).want(http.StatusOK)
	for _, key := range []store.Key{{Kind: "DNSZone", Name: "example"}, {Kind: "DNSRecordSet", Namespace: "default", Name: "first"}, {Kind: "DNSRecordSet", Namespace: "other", Name: "last"}} {
		var obj store.Object = &public.DNSRecordSet{}
		if key.Kind == "DNSZone" {
			obj = &public.DNSZone{}
		}
		if !errors.Is(st.Get(t.Context(), key, obj), store.ErrNotFound) {
			t.Fatal("cascade omitted current reference", key)
		}
	}
	zoneSeed(t, st, store.ManagerBootstrap, zoneObj("root", "."))
	if got := h.do(http.MethodDelete, "/api/v1/zones/root?dryRun=true", "admin", nil).want(http.StatusConflict).errKey(); got != KeyDefaultZone {
		t.Fatal(got)
	}
}

func TestZoneCommandEndpointDeleteValidatesAllOwnersBeforeApply(t *testing.T) {
	h, st := zoneCommandHarness(t, nil)
	z := zoneObj("example", "example.test.")
	z.Spec.Type = public.ZoneTypeForward // Removal retains legacy non-primary behavior.
	z.Spec.Nameservers = nil
	first := recordSetObj("default", "first", "example", ep("a", "A", "192.0.2.1"), ep("sibling", "A", "192.0.2.2"))
	last := recordSetObj("other", "last", "example", ep("a", "A", "192.0.2.3"))
	zoneSeed(t, st, store.ManagerAPI, z, first)
	zoneSeed(t, st, store.ManagerFile("zones.yaml"), last)
	before, _ := st.ReadGeneration(t.Context())
	h.do(http.MethodDelete, "/api/v1/zones/example/records/a/A", "operator", nil).want(http.StatusConflict)
	after, _ := st.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) || len(zoneRecord(t, st, "default", "first").Spec.Endpoints) != 2 {
		t.Fatal("late owner refusal committed an earlier endpoint removal")
	}
	zoneSeed(t, st, store.ManagerAPI, last)
	before, _ = st.ReadGeneration(t.Context())
	h.do(http.MethodDelete, "/api/v1/zones/example/records/a/A", "operator", nil).want(http.StatusNoContent)
	after, _ = st.ReadGeneration(t.Context())
	if after.Revision != before.Revision+1 || len(zoneRecord(t, st, "default", "first").Spec.Endpoints) != 1 {
		t.Fatal("removal across record sets did not commit atomically")
	}
	var removed public.DNSRecordSet
	if err := st.Get(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "other", Name: "last"}, &removed); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("empty current record set survived removal", err)
	}
}

func TestZoneCommandCascadeRequiresConditionalDeleteOnDryRun(t *testing.T) {
	calls := 0
	h, st := zoneCommandHarness(t, func(attrs []rbac.Attributes) error {
		calls++
		for _, attr := range attrs {
			if attr.Resource == "records" && attr.Verb == "delete" {
				return errForbidden(Missing{Resource: "records", Verb: "delete"}, "")
			}
		}
		return nil
	})
	zoneSeed(t, st, store.ManagerAPI, zoneObj("example", "example.test."), recordSetObj("other", "ref", "example", ep("a", "A", "192.0.2.1")))
	before, _ := st.ReadGeneration(t.Context())
	h.do(http.MethodDelete, "/api/v1/zones/example?dryRun=true", "admin", nil).want(http.StatusForbidden)
	h.do(http.MethodDelete, "/api/v1/zones/example", "admin", nil).want(http.StatusForbidden)
	after, _ := st.ReadGeneration(t.Context())
	if calls != 2 || !before.Generation().Same(after.Generation()) {
		t.Fatal("cascade dryRun bypassed conditional current permission")
	}
}

func zoneImportFile(count int) string {
	var b strings.Builder
	b.WriteString("$ORIGIN example.test.\n$TTL 60\n@ IN SOA ns hm 17 7200 3600 1209600 300\n@ IN NS ns\n")
	for i := range count {
		fmt.Fprintf(&b, "h%d IN A 192.0.2.%d\n", i, i%250+1)
	}
	return b.String()
}

func TestZoneCommandImportAtomicConditionalCleanup(t *testing.T) {
	denyDelete := false
	var attrs []rbac.Attributes
	h, st := zoneCommandHarness(t, func(a []rbac.Attributes) error {
		attrs = a
		for _, attr := range a {
			if denyDelete && attr.Resource == "records" && attr.Verb == "delete" {
				return errForbidden(Missing{Resource: "records", Verb: "delete"}, "")
			}
		}
		return nil
	})
	stale := recordSetObj("default", "import-example-3", "example", ep("old", "A", "192.0.2.6"))
	stale.Labels = map[string]string{labelImport: "example"}
	foreign := recordSetObj("default", "foreign", "other", ep("safe", "A", "192.0.2.7"))
	foreign.Labels = map[string]string{labelImport: "example"}
	zoneSeed(t, st, store.ManagerAPI, zoneObj("example", "example.test."), stale, foreign)
	blocked := recordSetObj("default", "import-example-2", "example", ep("blocked", "A", "192.0.2.9"))
	zoneSeed(t, st, store.ManagerFile("zones.yaml"), blocked)
	before, _ := st.ReadGeneration(t.Context())
	h.do(http.MethodPost, "/api/v1/zones/example/import", "admin", zoneImportFile(1500), "Content-Type", "text/dns").want(http.StatusConflict)
	after, _ := st.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("late blocked chunk committed earlier chunks")
	}
	if err := st.Delete(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: blocked.Name}, store.DeleteOptions{Manager: store.ManagerAPI, Force: true}); err != nil {
		t.Fatal(err)
	}
	denyDelete = true
	h.do(http.MethodPost, "/api/v1/zones/example/import", "admin", zoneImportFile(1500), "Content-Type", "text/dns").want(http.StatusForbidden)
	denyDelete = false
	before, _ = st.ReadGeneration(t.Context())
	var result ImportResult
	h.do(http.MethodPost, "/api/v1/zones/example/import", "admin", zoneImportFile(1500), "Content-Type", "text/dns").want(http.StatusOK).json(&result)
	after, _ = st.ReadGeneration(t.Context())
	if after.Revision != before.Revision+1 || len(result.RecordSets) != 2 || result.Endpoints != 1500 || len(attrs) != 2 {
		t.Fatal("import was not one complete command", result, attrs)
	}
	if len(zoneRecord(t, st, "default", "import-example-1").Spec.Endpoints) != 1000 || len(zoneRecord(t, st, "default", "import-example-2").Spec.Endpoints) != 500 {
		t.Fatal("incomplete chunk image")
	}
	var gone public.DNSRecordSet
	if !errors.Is(st.Get(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: stale.Name}, &gone), store.ErrNotFound) {
		t.Fatal("stale import chunk survived")
	}
	if !reflect.DeepEqual(zoneRecord(t, st, "default", foreign.Name).Spec, foreign.Spec) {
		t.Fatal("forged foreign label granted cleanup authority")
	}
	h.do(http.MethodPost, "/api/v1/zones/example/import", "admin", zoneImportFile(0), "Content-Type", "text/dns").want(http.StatusOK)
	before, _ = st.ReadGeneration(t.Context())
	h.do(http.MethodPost, "/api/v1/zones/example/import", "admin", zoneImportFile(0), "Content-Type", "text/dns").want(http.StatusOK)
	after, _ = st.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) || len(attrs) != 1 {
		t.Fatal("empty unchanged import skipped decision or unnecessarily required delete", attrs)
	}
}

func TestZoneCommandImportCreateAndCrossZoneCollision(t *testing.T) {
	denyCreate := true
	var calls int
	h, st := zoneCommandHarness(t, func(a []rbac.Attributes) error {
		calls++
		for _, attr := range a {
			if denyCreate && attr.Resource == "zones" && attr.Scope.Kind == rbac.ScopeCluster {
				return errForbidden(Missing{Resource: "zones", Verb: "write", Scope: "cluster"}, "")
			}
		}
		return nil
	})
	h.do(http.MethodPost, "/api/v1/zones/example/import", "admin", zoneImportFile(1), "Content-Type", "text/dns").want(http.StatusNotFound)
	if calls != 1 {
		t.Fatal("not-found import omitted current authentication")
	}
	h.do(http.MethodPost, "/api/v1/zones/example/import?origin=example.test.", "admin", zoneImportFile(1), "Content-Type", "text/dns").want(http.StatusForbidden)
	denyCreate = false
	foreign := recordSetObj("default", "import-example-1", "other", ep("safe", "A", "192.0.2.7"))
	zoneSeed(t, st, store.ManagerAPI, foreign)
	before, _ := st.ReadGeneration(t.Context())
	h.do(http.MethodPost, "/api/v1/zones/example/import?origin=example.test.", "admin", zoneImportFile(1), "Content-Type", "text/dns").want(http.StatusUnprocessableEntity)
	after, _ := st.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("cross-zone collision committed a new zone or rewrote foreign data")
	}
	if err := st.Delete(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: foreign.Name}, store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	before, _ = st.ReadGeneration(t.Context())
	h.do(http.MethodPost, "/api/v1/zones/example/import?origin=example.test.", "admin", zoneImportFile(1), "Content-Type", "text/dns").want(http.StatusOK)
	after, _ = st.ReadGeneration(t.Context())
	if after.Revision != before.Revision+1 {
		t.Fatal("zone and records did not commit together")
	}
}

func TestZoneCommandPlanLifetimeBoundsAndRemovalOnly(t *testing.T) {
	h, st := zoneCommandHarness(t, nil)
	err := st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		target := ZoneCommandTarget{Zone: "example", Action: ZoneCommandDeleteRecord}
		attrs := []rbac.Attributes{zoneRequirement(target, "records", "delete")}
		plan := newZoneCommandPlan(tx, target, attrs, func(context.Context, *sqlite.CommandTx) error { return nil })
		attrs[0].Verb = "write"
		copyAttrs := plan.Requirements()
		copyAttrs[0].Verb = "write"
		if plan.Requirements()[0].Verb != "delete" || !plan.PreparedFor(tx, target) || plan.PreparedFor(new(sqlite.CommandTx), target) {
			t.Fatal("plan requirements/identity mutable")
		}
		plan.Release()
		if plan.PreparedFor(tx, target) || plan.Apply(ctx, tx) == nil {
			t.Fatal("released no-op plan retained authority")
		}
		plan = newZoneCommandPlan(tx, target, []rbac.Attributes{zoneRequirement(target, "records", "delete")}, func(context.Context, *sqlite.CommandTx) error { return nil })
		if err := plan.Apply(ctx, tx); err != nil {
			return err
		}
		if plan.Apply(ctx, tx) == nil {
			t.Fatal("plan executed twice")
		}
		old := recordSetObj("default", "r", "example", ep("a", "A", "192.0.2.1"), ep("b", "A", "192.0.2.2"))
		desired := old.DeepCopy()
		desired.Spec.Endpoints = []public.Endpoint{ep("changed", "A", "192.0.2.99")}
		m := zoneCommandMutation{key: store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "r"}, old: old, desired: desired}
		if checkZoneMutation(target, m) == nil {
			t.Fatal("delete-only plan changed surviving content")
		}
		if _, err := h.srv.makeZonePlan(tx, target, nil, make([]zoneCommandMutation, maxZoneCommandObjects+1), nil, nil); err == nil {
			t.Fatal("unbounded plan accepted")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestZoneCommandCanonicalMissingCapabilityAndFinalRollback(t *testing.T) {
	h, st := zoneCommandHarness(t, nil)
	zoneSeed(t, st, store.ManagerAPI, zoneObj("example", "example.test."))
	command := h.srv.opts.ZoneCommand
	h.srv.opts.ZoneCommand = nil
	for _, req := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/v1/zones/example/records", ep("a", "A", "192.0.2.1")},
		{http.MethodDelete, "/api/v1/zones/example/records/a/A", nil},
		{http.MethodDelete, "/api/v1/zones/example?dryRun=true", nil},
		{http.MethodPost, "/api/v1/zones/example/import", zoneImportFile(0)},
	} {
		if _, raw := req.body.(string); raw {
			h.do(req.method, req.path, "admin", req.body, "Content-Type", "text/dns").want(http.StatusServiceUnavailable)
		} else {
			h.do(req.method, req.path, "admin", req.body).want(http.StatusServiceUnavailable)
		}
	}
	h.srv.opts.ZoneCommand = func(ctx context.Context, target ZoneCommandTarget, prepare ZoneCommandPrepare) error {
		return command(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx) (*ZoneCommandPlan, error) {
			if err := tx.RequireCommitCheck(func(context.Context) error { return errUnavailable("original fence expired") }); err != nil {
				return nil, err
			}
			return prepare(ctx, tx)
		})
	}
	before, _ := st.ReadGeneration(t.Context())
	h.do(http.MethodPost, "/api/v1/zones/example/records", "admin", ep("a", "A", "192.0.2.1")).want(http.StatusServiceUnavailable)
	h.do(http.MethodDelete, "/api/v1/zones/example?dryRun=true", "admin", nil).want(http.StatusServiceUnavailable)
	h.do(http.MethodDelete, "/api/v1/zones/example/records/absent/A", "admin", nil).want(http.StatusServiceUnavailable)
	h.do(http.MethodPost, "/api/v1/zones/example/import", "admin", zoneImportFile(0), "Content-Type", "text/dns").want(http.StatusServiceUnavailable)
	after, _ := st.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("final failure committed or returned unchecked success")
	}
}
