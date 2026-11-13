package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// D402: an external zone is filled only from Kubernetes sources. API writes
// of record sets that name it (whole-object PUT, POST /apply, the zone
// record endpoints and imports) are refused instead of stored and silently
// left out of the zone; record sets of other zones are unaffected.
func TestExternalZoneRefusesAPIRecordWrites(t *testing.T) {
	h := newHarness(t)
	ext := zoneObj("ext", "ext.example.")
	ext.Spec.Type = v1alpha1.ZoneTypeExternal
	ext.Spec.External = &v1alpha1.ExternalSpec{Ingress: true}
	h.seed("api", ext, zoneObj("example", "example.com."))

	doc := func(zone string) map[string]any {
		return map[string]any{
			"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSRecordSet",
			"metadata": map[string]any{"name": "web", "namespace": "dns"},
			"spec":     map[string]any{"zoneRef": map[string]any{"name": zone}, "endpoints": []any{map[string]any{"dnsName": "web." + map[string]string{"ext": "ext.example", "example": "example.com"}[zone], "recordType": "A", "targets": []any{"192.0.2.1"}}}},
		}
	}
	r := h.do(http.MethodPut, "/api/v1/objects/dnsrecordsets/dns/web", "admin", doc("ext")).want(http.StatusConflict)
	if r.errKey() != KeyConflict {
		t.Fatalf("refusal key %q", r.errKey())
	}
	h.do(http.MethodPost, "/api/v1/apply", "admin", []any{doc("ext")}).want(http.StatusOK)
	var rs v1alpha1.DNSRecordSet
	if err := h.st.Get(context.Background(), store.Key{Kind: "DNSRecordSet", Namespace: "dns", Name: "web"}, &rs); err == nil {
		t.Fatal("a record set for the external zone was stored")
	}
	h.do(http.MethodPost, "/api/v1/zones/ext/records", "admin", ep("www", "A", "192.0.2.1")).want(http.StatusConflict)
	// Other zones keep accepting record sets.
	h.do(http.MethodPut, "/api/v1/objects/dnsrecordsets/dns/web", "admin", doc("example")).want(http.StatusCreated)
}
