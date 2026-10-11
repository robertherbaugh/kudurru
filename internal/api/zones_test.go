package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestZonesList(t *testing.T) {
	h := newHarness(t)
	h.seed("api", zoneObj("example", "example.com."), zoneObj("other", "other.org."))
	h.seed(store.ManagerFile("objects.d/rs.yaml"), recordSetObj("default", "web", "example", ep("www.example.com", "A", "192.0.2.1")))
	var list struct {
		Items []ZoneSummary `json:"items"`
	}
	h.do(http.MethodGet, "/api/v1/zones", "viewer", nil).want(http.StatusOK).json(&list)
	if len(list.Items) != 2 || list.Items[0].Name != "example" || list.Items[0].RecordSets != 1 || list.Items[0].ManagedBy != "api" || list.Items[0].Type != "primary" {
		t.Fatalf("zones %+v", list.Items)
	}
	// zone-admin sees only its zone.
	h.do(http.MethodGet, "/api/v1/zones", "zoneadmin", nil).want(http.StatusOK).json(&list)
	if len(list.Items) != 1 || list.Items[0].Name != "example" {
		t.Fatalf("zone-admin zones %+v", list.Items)
	}
	var d ZoneDetail
	h.do(http.MethodGet, "/api/v1/zones/example", "zoneadmin", nil).want(http.StatusOK).json(&d)
	if d.ZoneName != "example.com." || d.Object["kind"] != "DNSZone" {
		t.Fatalf("detail %+v", d)
	}
	h.do(http.MethodGet, "/api/v1/zones/other", "zoneadmin", nil).want(http.StatusForbidden)
	h.do(http.MethodGet, "/api/v1/zones/missing", "admin", nil).want(http.StatusNotFound)
}

func TestZoneRecords(t *testing.T) {
	h := newHarness(t)
	h.seed("api", zoneObj("example", "example.com."))
	h.seed(store.ManagerFile("objects.d/rs.yaml"), recordSetObj("default", "filed", "example", ep("mail.example.com", "A", "192.0.2.9")))

	// Create one endpoint in api-example (created on demand).
	r := h.do(http.MethodPost, "/api/v1/zones/example/records", "operator", ep("www", "A", "192.0.2.1")).want(http.StatusCreated)
	var rec ZoneRecord
	r.json(&rec)
	if rec.RecordSet != "default/api-example" || rec.ManagedBy != "api" {
		t.Fatalf("record %+v", rec)
	}
	// Replace it (same name and type).
	h.do(http.MethodPost, "/api/v1/zones/example/records", "operator", ep("www.example.com.", "A", "192.0.2.2")).want(http.StatusOK)
	h.do(http.MethodPost, "/api/v1/zones/example/records", "operator", ep("www", "AAAA", "2001:db8::1")).want(http.StatusCreated)

	var list struct {
		Items []ZoneRecord `json:"items"`
	}
	h.do(http.MethodGet, "/api/v1/zones/example/records", "viewer", nil).want(http.StatusOK).json(&list)
	if len(list.Items) != 3 {
		t.Fatalf("records %+v", list.Items)
	}
	got := map[string]string{}
	for _, it := range list.Items {
		got[it.RecordType+" "+it.DNSName] = strings.Join(it.Targets, ",") + "@" + it.RecordSet
	}
	if got["A www.example.com."] != "192.0.2.2@default/api-example" || got["A mail.example.com"] != "192.0.2.9@default/filed" {
		t.Fatalf("records %v", got)
	}

	// An invalid endpoint is rejected by the validator.
	h.do(http.MethodPost, "/api/v1/zones/example/records", "operator", map[string]any{"dnsName": "x", "recordType": "A", "targets": []string{}, "bogus": 1}).want(http.StatusBadRequest)

	// Deleting a file-managed record is a managed_by conflict.
	r = h.do(http.MethodDelete, "/api/v1/zones/example/records/mail/A", "operator", nil).want(http.StatusConflict)
	if r.errKey() != KeyManagedBy {
		t.Fatalf("key %q", r.errKey())
	}
	h.do(http.MethodDelete, "/api/v1/zones/example/records/www/AAAA", "operator", nil).want(http.StatusNoContent)
	h.do(http.MethodDelete, "/api/v1/zones/example/records/www/AAAA", "operator", nil).want(http.StatusNotFound)
	h.do(http.MethodDelete, "/api/v1/zones/example/records/www.example.com/a", "operator", nil).want(http.StatusNoContent)
	// The emptied record set is gone.
	h.do(http.MethodGet, "/api/v1/objects/dnsrecordsets/default/api-example", "admin", nil).want(http.StatusNotFound)
	// A viewer cannot write records.
	h.do(http.MethodPost, "/api/v1/zones/example/records", "viewer", ep("x", "A", "192.0.2.3")).want(http.StatusForbidden)
}

const exampleZoneFile = `$ORIGIN example.net.
$TTL 3600
@	IN SOA ns1.example.net. hostmaster.example.net. 2024010101 7200 3600 1209600 300
@	IN NS ns1.example.net.
ns1	IN A 192.0.2.53
www	IN A 192.0.2.80
www	IN AAAA 2001:db8::80
mail	IN MX 10 mx.example.net.
`

func TestZoneImportExport(t *testing.T) {
	h := newHarness(t)
	// Without the zone and without origin: 404 with a hint.
	r := h.do(http.MethodPost, "/api/v1/zones/imported/import", "admin", exampleZoneFile, "Content-Type", "text/dns").want(http.StatusNotFound)
	if !strings.Contains(r.Body.String(), "origin") {
		t.Fatalf("no origin hint: %s", r.Body.String())
	}
	var res ImportResult
	h.do(http.MethodPost, "/api/v1/zones/imported/import?origin=example.net.", "admin", exampleZoneFile, "Content-Type", "text/dns").
		want(http.StatusOK).json(&res)
	if !res.Created || res.Serial != 2024010101 || res.Endpoints != 4 || len(res.RecordSets) != 1 {
		t.Fatalf("import %+v", res)
	}
	// Re-import replaces the previous import.
	h.do(http.MethodPost, "/api/v1/zones/imported/import", "admin", exampleZoneFile, "Content-Type", "text/dns").want(http.StatusOK).json(&res)
	if res.Created || res.Endpoints != 4 {
		t.Fatalf("re-import %+v", res)
	}
	h.do(http.MethodPost, "/api/v1/zones/imported/import", "admin", "this is not a zone file", "Content-Type", "text/dns").want(http.StatusBadRequest)

	r = h.do(http.MethodGet, "/api/v1/zones/imported/export", "viewer", nil).want(http.StatusOK)
	if ct := r.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/dns") {
		t.Fatalf("content type %q", ct)
	}
	body := r.Body.String()
	for _, want := range []string{"$ORIGIN example.net.", "www.example.net.", "192.0.2.80", "2001:db8::80", "MX\t10 mx.example.net."} {
		if !strings.Contains(body, want) {
			t.Errorf("export misses %q:\n%s", want, body)
		}
	}
}

func TestZoneNotify(t *testing.T) {
	var notified []string
	h := newHarness(t, func(o *Options) {
		o.Notifier = notifyFunc(func(zone string) ([]string, error) {
			notified = append(notified, zone)
			return []string{"192.0.2.53:53"}, nil
		})
	})
	h.seed("api", zoneObj("example", "example.com."))
	var res NotifyResult
	h.do(http.MethodPost, "/api/v1/zones/example/notify", "zoneadmin", nil).want(http.StatusAccepted).json(&res)
	if len(notified) != 1 || notified[0] != "example.com." || len(res.Targets) != 1 {
		t.Fatalf("notify %v %+v", notified, res)
	}
	// operator lacks zones.transfer.
	h.do(http.MethodPost, "/api/v1/zones/example/notify", "operator", nil).want(http.StatusForbidden)
	evs := h.audit.Events()
	if len(evs) == 0 || evs[len(evs)-2].Action != "zones.transfer" || evs[len(evs)-2].Result != "success" {
		t.Fatalf("audit %+v", evs)
	}
	if last := evs[len(evs)-1]; last.Result != "denied" || last.Actor.Name != "operator" {
		t.Fatalf("denied attempt not audited: %+v", last)
	}
}

type notifyFunc func(zone string) ([]string, error)

func (f notifyFunc) NotifyZone(_ context.Context, zone string) ([]string, error) { return f(zone) }

func TestZoneAdminScopedWrites(t *testing.T) {
	h := newHarness(t)
	h.seed("api", zoneObj("example", "example.com."), zoneObj("other", "other.org."))
	// zone-admin may update its zone but not create zones or touch others.
	doc := zoneDoc("example", "example.com.")
	doc["spec"].(map[string]any)["defaultTTL"] = 120
	h.do(http.MethodPut, "/api/v1/objects/dnszones/example", "zoneadmin", doc).want(http.StatusOK)
	h.do(http.MethodPut, "/api/v1/objects/dnszones/other", "zoneadmin", zoneDoc("other", "other.org.")).want(http.StatusForbidden)
	r := h.do(http.MethodPut, "/api/v1/objects/dnszones/new", "zoneadmin", zoneDoc("new", "new.example.")).want(http.StatusForbidden)
	var e struct {
		Error struct {
			Hint ForbiddenHint `json:"hint"`
		} `json:"error"`
	}
	r.json(&e)
	if e.Error.Hint.Missing != (Missing{Resource: "zones", Verb: "write", Scope: "cluster"}) {
		t.Fatalf("missing %+v", e.Error.Hint.Missing)
	}
	// Record sets follow their zoneRef.
	rs := map[string]any{
		"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSRecordSet",
		"metadata": map[string]any{"name": "web", "namespace": "dns"},
		"spec":     map[string]any{"zoneRef": map[string]any{"name": "example"}, "endpoints": []any{map[string]any{"dnsName": "www.example.com", "recordType": "A", "targets": []any{"192.0.2.1"}}}},
	}
	h.do(http.MethodPut, "/api/v1/objects/dnsrecordsets/dns/web", "zoneadmin", rs).want(http.StatusCreated)
	rs["spec"].(map[string]any)["zoneRef"] = map[string]any{"name": "other"}
	h.do(http.MethodPut, "/api/v1/objects/dnsrecordsets/dns/web", "zoneadmin", rs).want(http.StatusForbidden)
	// Listing record sets filters by zone.
	h.seed("api", recordSetObj("dns", "o", "other", ep("a.other.org", "A", "192.0.2.7")))
	var list ObjectList
	h.do(http.MethodGet, "/api/v1/objects/dnsrecordsets", "zoneadmin", nil).want(http.StatusOK).json(&list)
	if len(list.Items) != 1 {
		t.Fatalf("zone-admin sees %d record sets", len(list.Items))
	}
	// zone-admin has no blocklists.read: listing is refused, not empty.
	h.do(http.MethodGet, "/api/v1/objects/blocklists", "zoneadmin", nil).want(http.StatusForbidden)
}

func TestImportChunks(t *testing.T) {
	var b strings.Builder
	b.WriteString("$ORIGIN big.example.\n$TTL 60\n@ SOA ns hm 1 2 3 4 5\n@ NS ns\n")
	for i := range 1500 {
		fmt.Fprintf(&b, "h%d A 192.0.2.%d\n", i, i%250+1)
	}
	h := newHarness(t)
	var res ImportResult
	h.do(http.MethodPost, "/api/v1/zones/big/import?origin=big.example.", "admin", b.String(), "Content-Type", "text/dns").want(http.StatusOK).json(&res)
	if len(res.RecordSets) != 2 || res.Endpoints != 1500 {
		t.Fatalf("import %+v", res)
	}
	var rs v1alpha1.DNSRecordSet
	if err := h.st.Get(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "import-big-1"}, &rs); err != nil || len(rs.Spec.Endpoints) != 1000 {
		t.Fatalf("chunk 1: %v %d", err, len(rs.Spec.Endpoints))
	}
}
