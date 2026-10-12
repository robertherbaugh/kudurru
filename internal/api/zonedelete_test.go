package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// errBody decodes the error envelope with error.objects.
func errBody(r response) ErrorBody {
	r.t.Helper()
	var e struct {
		Error ErrorBody `json:"error"`
	}
	r.json(&e)
	return e.Error
}

func (h *harness) exists(key store.Key) bool {
	h.t.Helper()
	var into store.Object = &v1alpha1.DNSRecordSet{}
	if key.Kind == "DNSZone" {
		into = &v1alpha1.DNSZone{}
	}
	err := h.srv.opts.Store.Get(context.Background(), key, into)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		h.t.Fatal(err)
	}
	return err == nil
}

func zoneKey(name string) store.Key { return store.Key{Kind: "DNSZone", Name: name} }

func rsKey(ns, name string) store.Key {
	return store.Key{Kind: "DNSRecordSet", Namespace: ns, Name: name}
}

func forwardZoneObj(name, apex string) *v1alpha1.DNSZone {
	z := zoneObj(name, apex)
	z.Spec.Type, z.Spec.Nameservers = v1alpha1.ZoneTypeForward, nil
	z.Spec.Forward = &v1alpha1.ForwardSpec{UpstreamGroup: "default"}
	return z
}

func TestDeleteZoneCascade(t *testing.T) {
	rc := cache.New(cache.Options{MaxEntries: 1000})
	h := newHarness(t, func(o *Options) { o.ResponseCache = rc })
	h.seed("api", zoneObj("example", "example.com."), zoneObj("other", "other.org."))
	h.seed("api",
		recordSetObj("default", "api-example", "example", ep("www.example.com", "A", "192.0.2.1"), ep("mail.example.com", "A", "192.0.2.2")),
		recordSetObj("apps", "web", "example", ep("app.example.com", "CNAME", "www.example.com")),
		recordSetObj("default", "keep", "other", ep("www.other.org", "A", "192.0.2.3")))
	h.seed(store.ManagerBootstrap, recordSetObj("apps", "seeded", "example", ep("seed.example.com", "A", "192.0.2.4")))
	for _, n := range []string{"www.example.com.", "x.y.example.com.", "example.com.", "www.other.org."} {
		rc.Set(cache.MakeKey(n, 1, 1, false, false, netip.Prefix{}), []byte("x"), cache.Positive, 60e9)
	}

	// The dry run reports and deletes nothing.
	var dry ZoneDeleteResult
	h.do(http.MethodDelete, "/api/v1/zones/example?dryRun=true", "admin", nil).want(http.StatusOK).json(&dry)
	wantSets := []string{"apps/seeded", "apps/web", "default/api-example"}
	if !dry.DryRun || dry.Zone != "example" || dry.ZoneName != "example.com." || dry.Type != "primary" ||
		dry.RecordSetCount != 3 || dry.EndpointCount != 4 || !slices.Equal(dry.RecordSets, wantSets) || dry.Flushed != 0 {
		t.Fatalf("dry run %+v", dry)
	}
	if !h.exists(zoneKey("example")) || !h.exists(rsKey("apps", "web")) || rc.Len() != 4 {
		t.Fatal("the dry run deleted something")
	}
	if n := len(h.audit.Events()); n != 0 {
		t.Fatalf("the dry run was audited: %+v", h.audit.Events())
	}

	var res ZoneDeleteResult
	h.do(http.MethodDelete, "/api/v1/zones/example", "admin", nil).want(http.StatusOK).json(&res)
	if res.DryRun || res.RecordSetCount != 3 || res.EndpointCount != 4 || !slices.Equal(res.RecordSets, wantSets) || res.Flushed != 3 {
		t.Fatalf("delete %+v", res)
	}
	for _, k := range []store.Key{zoneKey("example"), rsKey("default", "api-example"), rsKey("apps", "web"), rsKey("apps", "seeded")} {
		if h.exists(k) {
			t.Fatalf("%s survived the zone delete", k)
		}
	}
	if !h.exists(zoneKey("other")) || !h.exists(rsKey("default", "keep")) || rc.Len() != 1 {
		t.Fatal("the delete went beyond the zone")
	}
	evs := h.audit.Events()
	if len(evs) != 1 || evs[0].Action != "zones.delete" || evs[0].Result != audit.ResultSuccess || evs[0].Resource.Name != "example" ||
		!strings.Contains(string(evs[0].Before), `"recordSetCount":3`) || !strings.Contains(string(evs[0].Before), "apps/web") {
		t.Fatalf("audit %+v", evs)
	}
	h.do(http.MethodDelete, "/api/v1/zones/example", "admin", nil).want(http.StatusNotFound)
}

func TestDeleteZoneForwardAndPermissions(t *testing.T) {
	p, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	p.Roles["zone-deleter"] = []string{"zones.read", "zones.delete"}
	authz, err := rbac.NewBindingAuthorizer(p, append(apitest.StandardBindings(), apitest.Grant("zone-deleter", "deleter")))
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(o *Options) { o.Authorizer = authz })
	h.seed("api", zoneObj("example", "example.com."), forwardZoneObj("corp", "corp.example."), forwardZoneObj("lab", "lab.example."))
	h.seed("api", recordSetObj("default", "api-example", "example", ep("www.example.com", "A", "192.0.2.1")))

	for _, u := range []string{"viewer", "auditor", "operator", "nobody"} {
		if r := h.do(http.MethodDelete, "/api/v1/zones/example?dryRun=true", u, nil); r.Code != http.StatusForbidden {
			t.Fatalf("%s: %d %s", u, r.Code, r.Body.String())
		}
	}
	// zone-admin of "example" only.
	h.do(http.MethodDelete, "/api/v1/zones/corp", "zoneadmin", nil).want(http.StatusForbidden)
	// zones.delete without records.delete: forward zones (no record sets)
	// only.
	r := h.do(http.MethodDelete, "/api/v1/zones/example?dryRun=true", "deleter", nil).want(http.StatusForbidden)
	if m := errBody(r).Missing; m == nil || m.Resource != "records" || m.Verb != "delete" || m.Scope != "zone:example" {
		t.Fatalf("missing %+v", m)
	}
	var res ZoneDeleteResult
	h.do(http.MethodDelete, "/api/v1/zones/corp", "deleter", nil).want(http.StatusOK).json(&res)
	if res.Type != "forward" || res.RecordSetCount != 0 || res.RecordSets == nil || res.EndpointCount != 0 {
		t.Fatalf("forward zone delete %+v", res)
	}
	if h.exists(zoneKey("corp")) {
		t.Fatal("forward zone survived")
	}
	// If-Match on the zone's resourceVersion.
	h.do(http.MethodDelete, "/api/v1/zones/lab", "admin", nil, "If-Match", "1").want(http.StatusPreconditionFailed)
	h.do(http.MethodDelete, "/api/v1/zones/example", "zoneadmin", nil).want(http.StatusOK)
	if h.exists(rsKey("default", "api-example")) {
		t.Fatal("record set survived")
	}
}

func TestDeleteZoneRefusals(t *testing.T) {
	h := newHarness(t)
	root := forwardZoneObj("root", ".")
	h.seed(store.ManagerBootstrap, root)
	h.seed("api", zoneObj("example", "example.com."), zoneObj("free", "free.example."))
	h.seed(store.ManagerFile("objects.d/zones.yaml"), zoneObj("filed", "filed.example."))
	h.seed("api", recordSetObj("default", "api-example", "example", ep("www.example.com", "A", "192.0.2.1")))
	h.seed(store.ManagerIntegration("externaldns"), recordSetObj("apps", "ext", "example", ep("svc.example.com", "A", "192.0.2.2")))

	// The list says why before anyone tries.
	var list struct {
		Items []ZoneSummary `json:"items"`
	}
	h.do(http.MethodGet, "/api/v1/zones", "viewer", nil).want(http.StatusOK).json(&list)
	refusal := map[string]string{}
	for _, z := range list.Items {
		refusal[z.Name] = z.DeleteRefusal
	}
	if refusal["root"] != KeyDefaultZone || refusal["example"] != KeyManagedBy || refusal["filed"] != KeyManagedBy || refusal["free"] != "" {
		t.Fatalf("deleteRefusal %v", refusal)
	}

	for _, dry := range []string{"?dryRun=true", ""} {
		r := h.do(http.MethodDelete, "/api/v1/zones/root"+dry, "admin", nil).want(http.StatusConflict)
		if r.errKey() != KeyDefaultZone {
			t.Fatalf("root: %s", r.Body.String())
		}
		r = h.do(http.MethodDelete, "/api/v1/zones/example"+dry, "admin", nil).want(http.StatusConflict)
		e := errBody(r)
		if e.Key != KeyManagedBy || e.Hint != "integration:externaldns" || len(e.Objects) != 1 ||
			e.Objects[0] != (ObjectOwner{Kind: "DNSRecordSet", Namespace: "apps", Name: "ext", ManagedBy: "integration:externaldns"}) ||
			!strings.Contains(e.Message, "DNSRecordSet/apps/ext (integration:externaldns)") {
			t.Fatalf("managed record set: %s", r.Body.String())
		}
		e = errBody(h.do(http.MethodDelete, "/api/v1/zones/filed"+dry, "admin", nil).want(http.StatusConflict))
		if e.Key != KeyManagedBy || len(e.Objects) != 1 || e.Objects[0].Kind != "DNSZone" || e.Objects[0].ManagedBy != "file:objects.d/zones.yaml" {
			t.Fatalf("managed zone: %+v", e)
		}
	}
	for _, k := range []store.Key{zoneKey("root"), zoneKey("example"), zoneKey("filed"), rsKey("default", "api-example"), rsKey("apps", "ext")} {
		if !h.exists(k) {
			t.Fatalf("%s was deleted by a refused request", k)
		}
	}
	// Refusals change nothing, so they are not audited as deletes.
	for _, ev := range h.audit.Events() {
		if ev.Action == "zones.delete" {
			t.Fatalf("refusal audited: %+v", ev)
		}
	}
	// ha-static: read-only.
	hs := newHarness(t, func(o *Options) { o.Objects = staticLoader(t, o.Store) })
	hs.seed("api", zoneObj("example", "example.com."))
	if r := hs.do(http.MethodDelete, "/api/v1/zones/example?dryRun=true", "admin", nil); r.Code != http.StatusServiceUnavailable || r.errKey() != KeyReadOnly {
		t.Fatalf("ha-static: %d %s", r.Code, r.Body.String())
	}
}

// TestDeleteZoneObjectRoutes checks D148 on the generic routes: a zone
// that record sets reference is never deleted alone, and the root zone not
// at all, so the two delete paths cannot disagree.
func TestDeleteZoneObjectRoutes(t *testing.T) {
	h := newHarness(t)
	h.seed(store.ManagerBootstrap, forwardZoneObj("root", "."))
	h.seed("api", zoneObj("example", "example.com."), zoneObj("empty", "empty.example."))
	h.seed("api", recordSetObj("default", "api-example", "example", ep("www.example.com", "A", "192.0.2.1")),
		recordSetObj("apps", "web", "example", ep("app.example.com", "A", "192.0.2.2")))

	r := h.do(http.MethodDelete, "/api/v1/objects/dnszones/example", "admin", nil).want(http.StatusConflict)
	e := errBody(r)
	if e.Key != KeyZoneNotEmpty || len(e.Objects) != 2 || e.Objects[0].Name != "web" || !strings.Contains(e.Message, "2 record sets") ||
		!strings.Contains(e.Hint.(string), "/zones/example") {
		t.Fatalf("zone_not_empty: %s", r.Body.String())
	}
	if r := h.do(http.MethodDelete, "/api/v1/objects/dnszones/root", "admin", nil); r.Code != http.StatusConflict || r.errKey() != KeyDefaultZone {
		t.Fatalf("root: %d %s", r.Code, r.Body.String())
	}
	var out Processed
	h.do(http.MethodPost, "/api/v1/dnszones:batchDelete", "admin", map[string]any{"names": []string{"example", "root", "empty"}}).want(http.StatusOK).json(&out)
	if len(out.Processed.Success) != 1 || out.Processed.Success[0].Item != "empty" || len(out.Processed.Errors) != 2 ||
		!strings.HasPrefix(out.Processed.Errors[0].Error, KeyZoneNotEmpty) || !strings.HasPrefix(out.Processed.Errors[1].Error, KeyDefaultZone) {
		t.Fatalf("batchDelete %+v", out)
	}
	if !h.exists(zoneKey("example")) || !h.exists(zoneKey("root")) || h.exists(zoneKey("empty")) {
		t.Fatal("batchDelete outcome")
	}
	// Once the record sets are gone the zone may go alone.
	h.do(http.MethodDelete, "/api/v1/objects/dnsrecordsets/default/api-example", "admin", nil).want(http.StatusNoContent)
	h.do(http.MethodDelete, "/api/v1/objects/dnsrecordsets/apps/web", "admin", nil).want(http.StatusNoContent)
	h.do(http.MethodDelete, "/api/v1/objects/dnszones/example", "admin", nil).want(http.StatusNoContent)
}

// failingStore hides the memory store's DeleteMany (the kube driver's
// shape: one delete after the other) and fails the delete of one key.
type failingStore struct {
	store.Store
	fail store.Key
}

func (f failingStore) Delete(ctx context.Context, key store.Key, opts store.DeleteOptions) error {
	if key == f.fail {
		return store.ErrClosed
	}
	return f.Store.Delete(ctx, key, opts)
}

func TestDeleteZonePartialFailure(t *testing.T) {
	h := newHarness(t)
	h.seed("api", zoneObj("example", "example.com."))
	h.seed("api", recordSetObj("a", "one", "example", ep("a.example.com", "A", "192.0.2.1")),
		recordSetObj("b", "two", "example", ep("b.example.com", "A", "192.0.2.2")))
	h.srv.opts.Store = failingStore{Store: h.st, fail: rsKey("b", "two")}

	r := h.do(http.MethodDelete, "/api/v1/zones/example", "admin", nil).want(http.StatusServiceUnavailable)
	if !strings.Contains(errBody(r).Message, "1 of 2 record sets were deleted") {
		t.Fatalf("partial failure: %s", r.Body.String())
	}
	if h.exists(rsKey("a", "one")) || !h.exists(rsKey("b", "two")) || !h.exists(zoneKey("example")) {
		t.Fatal("the zone must stay until every record set is gone")
	}
	evs := h.audit.Events()
	if len(evs) != 1 || evs[0].Result != audit.ResultError || !strings.Contains(string(evs[0].Before), `"deleted":1`) {
		t.Fatalf("audit %+v", evs)
	}
	// The retry finishes the job.
	h.srv.opts.Store = h.st
	var res ZoneDeleteResult
	h.do(http.MethodDelete, "/api/v1/zones/example", "admin", nil).want(http.StatusOK).json(&res)
	if res.RecordSetCount != 1 || h.exists(zoneKey("example")) {
		t.Fatalf("retry %+v", res)
	}
}

// TestDeleteZoneSQLite runs the cascade on the sqlite driver: one
// transaction, one hash-chained audit row per deleted object plus the
// API's zones.delete event.
func TestDeleteZoneSQLite(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{Validator: testValidator(t), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rec := audit.Multi{audit.SQLite{Store: st}}
	h := newHarness(t, func(o *Options) { o.Store = st; o.Audit = rec })
	h.do(http.MethodPut, "/api/v1/objects/dnszones/example", "admin", zoneDoc("example", "example.com.")).want(http.StatusCreated)
	h.do(http.MethodPost, "/api/v1/zones/example/records", "admin", ep("www", "A", "192.0.2.1")).want(http.StatusCreated)
	h.do(http.MethodPost, "/api/v1/zones/example/records?recordSet=apps/web", "admin", ep("app", "A", "192.0.2.2")).want(http.StatusCreated)

	var res ZoneDeleteResult
	h.do(http.MethodDelete, "/api/v1/zones/example", "admin", nil).want(http.StatusOK).json(&res)
	if res.RecordSetCount != 2 || res.EndpointCount != 2 {
		t.Fatalf("delete %+v", res)
	}
	for _, k := range []store.Key{zoneKey("example"), rsKey("default", "api-example"), rsKey("apps", "web")} {
		if h.exists(k) {
			t.Fatalf("%s survived", k)
		}
	}
	var page AuditPage
	h.do(http.MethodGet, "/api/v1/audit?order=desc&limit=4", "auditor", nil).want(http.StatusOK).json(&page)
	var actions []string
	for _, ev := range page.Items {
		if ev.Actor.Name != "admin" || ev.Result != audit.ResultSuccess {
			t.Fatalf("audit row %+v", ev)
		}
		actions = append(actions, ev.Action)
	}
	want := []string{"zones.delete", "dnszone.delete", "dnsrecordset.delete", "dnsrecordset.delete"}
	if !slices.Equal(actions, want) {
		t.Fatalf("audit actions %v, want %v", actions, want)
	}
}
