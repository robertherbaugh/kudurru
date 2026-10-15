package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// Explicit API capability fixture; actual original-lease/current-source
// acceptance lives with the concrete servingbuild adapter.
type zoneReadFixture struct {
	objects zonecache.ObjectSet
	model   *zone.Zone
	err     error
	reads   int
}

func (f *zoneReadFixture) ZoneObjects(context.Context) (zonecache.ObjectSet, error) {
	return f.objects, nil
}
func (f *zoneReadFixture) ReadZone(_ context.Context, name string) (*zone.Zone, error) {
	f.reads++
	if _, found := f.objects.Zone(name); !found {
		return nil, store.ErrNotFound
	}
	return f.model, f.err
}
func (f *zoneReadFixture) PrimaryStatus(_ context.Context, name string) (primaryruntime.PrimaryStatusObservation, bool, error) {
	if _, found := f.objects.Zone(name); !found {
		return primaryruntime.PrimaryStatusObservation{}, false, store.ErrNotFound
	}
	return primaryruntime.PrimaryStatusObservation{Serial: 42, ContentHash: "actual-head", RecordCount: 3}, true, nil
}

func newZoneReadFixture(t *testing.T) (*harness, *zoneReadFixture) {
	t.Helper()
	f := &zoneReadFixture{}
	h := newHarness(t, func(o *Options) { o.ZoneReads = f })
	z := zoneObj("example", "example.com.")
	z.Status.Serial, z.Status.RecordCount = 9, 99 // Stale desired status is not served status.
	rs := recordSetObj("default", "pending", "example", ep("pending.example.com", "A", "192.0.2.99"))
	store.SetManagedBy(rs, store.ManagerFile("objects.d/source.yaml"))
	snapshot, err := zonecache.Prepare(zonecache.PrepareOptions{}, []store.Object{z, rs}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.objects = snapshot.Objects
	actual := z.DeepCopy()
	actual.Status.Serial = 42
	f.model, _ = zone.Build(actual, []*public.DNSRecordSet{recordSetObj("default", "actual", "example", ep("dynamic.example.com", "A", "192.0.2.1"))}, zone.BuildOptions{})
	if f.model == nil {
		t.Fatal("fixture model did not build")
	}
	// A competing live source must not leak into any captured read route.
	h.seed("api", zoneObj("live-only", "live.invalid."))
	return h, f
}

func TestCapturedZoneReadsSummaryRecordsExportAndOwnership(t *testing.T) {
	h, f := newZoneReadFixture(t)
	var detail ZoneDetail
	h.do(http.MethodGet, "/api/v1/zones/example", "viewer", nil).want(http.StatusOK).json(&detail)
	if detail.Serial != 42 || !detail.Ready || detail.RecordCount != f.model.RecordCount() || detail.RecordSets != 1 || detail.DeleteRefusal != KeyManagedBy || detail.Object["metadata"] == nil {
		t.Fatal("summary mixed desired status or lost desired management", detail)
	}
	var list List[ZoneSummary]
	h.do(http.MethodGet, "/api/v1/zones", "viewer", nil).want(http.StatusOK).json(&list)
	if len(list.Items) != 1 || list.Items[0].Name != "example" || list.Items[0].Serial != 42 {
		t.Fatal("list read the live store", list)
	}
	var records List[ZoneRecord]
	h.do(http.MethodGet, "/api/v1/zones/example/records", "viewer", nil).want(http.StatusOK).json(&records)
	var dynamic, soa bool
	for _, row := range records.Items {
		if row.RecordSet != "" || row.ManagedBy != "" || row.Kudurru != nil || row.Labels != nil {
			t.Fatal("served RR fabricated a mutable desired reference", row)
		}
		if row.DNSName == "pending.example.com" {
			t.Fatal("pending desired row appeared served", row)
		}
		dynamic = dynamic || (row.DNSName == "dynamic.example.com" && strings.Join(row.Targets, ",") == "192.0.2.1")
		soa = soa || row.RecordType == "SOA"
	}
	if !dynamic || !soa {
		t.Fatal("actual dynamic/system records missing", records.Items)
	}
	export := h.do(http.MethodGet, "/api/v1/zones/example/export", "viewer", nil).want(http.StatusOK)
	if !strings.Contains(export.Body.String(), "dynamic.example.com.") || strings.Contains(export.Body.String(), "pending.example.com") || !strings.Contains(export.Body.String(), "serial 42") {
		t.Fatal("export rebuilt desired contents", export.Body.String())
	}
	// Returned rows are detached, and desired ownership remains unmodified.
	for _, row := range records.Items {
		if len(row.Targets) != 0 {
			row.Targets[0] = "changed"
		}
	}
	again := h.do(http.MethodGet, "/api/v1/zones/example/export", "viewer", nil).want(http.StatusOK)
	if again.Body.String() != export.Body.String() || f.objects.RecordSetsForZone("example")[0].Name != "pending" {
		t.Fatal("read response mutated captured data")
	}
	if f.reads < 4 {
		t.Fatal("captured capability was bypassed", f.reads)
	}
}

func TestCapturedZoneReadsUnavailableMissingAndNoCompilerFallback(t *testing.T) {
	h, f := newZoneReadFixture(t)
	f.model, f.err = nil, serving.ErrPending
	for _, path := range []string{"/records", "/export"} {
		h.do(http.MethodGet, "/api/v1/zones/example"+path, "viewer", nil).want(http.StatusServiceUnavailable)
		h.do(http.MethodGet, "/api/v1/zones/absent"+path, "viewer", nil).want(http.StatusNotFound)
	}
	var detail ZoneDetail
	h.do(http.MethodGet, "/api/v1/zones/example", "viewer", nil).want(http.StatusOK).json(&detail)
	if detail.Ready || detail.Serial != 42 || detail.RecordCount != 3 || detail.Reason != "ServingUnavailable" || detail.DeleteRefusal != KeyManagedBy {
		t.Fatal("unavailable discarded actual head or borrowed desired readiness", detail)
	}
	h.do(http.MethodGet, "/api/v1/zones/live-only", "viewer", nil).want(http.StatusNotFound)
	f.err = errors.New("released original lease")
	h.do(http.MethodGet, "/api/v1/zones/example", "viewer", nil).want(http.StatusServiceUnavailable)
	h.do(http.MethodGet, "/api/v1/zones/example/records", "viewer", nil).want(http.StatusServiceUnavailable)
}

func TestCapturedZoneReadsDoNotChangeDesiredMutationSelection(t *testing.T) {
	h, f := newZoneReadFixture(t)
	f.err = serving.ErrPending
	// The captured example exists, while current desired example is absent.
	// Mutation must retain the existing current desired management behavior.
	h.do(http.MethodPost, "/api/v1/zones/example/records", "operator", ep("new.example.com", "A", "192.0.2.2")).want(http.StatusNotFound)
	if f.reads != 0 {
		t.Fatal("desired mutation used serving read capability")
	}
}

func TestConfiguredCrossOriginProtectionIsShared(t *testing.T) {
	cop := http.NewCrossOriginProtection()
	if err := cop.AddTrustedOrigin("https://trusted.example"); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(o *Options) { o.CrossOrigin = cop })
	h.do(http.MethodPost, "/api/v1/dns/blocking", "admin", map[string]any{"blocking": true}, "Sec-Fetch-Site", "cross-site", "Origin", "https://trusted.example").want(http.StatusNotImplemented)
	h.do(http.MethodPost, "/api/v1/dns/blocking", "admin", map[string]any{"blocking": true}, "Sec-Fetch-Site", "cross-site", "Origin", "https://other.example").want(http.StatusForbidden)
}
