package api

import (
	"net/http"
	"net/netip"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/stub"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

func TestStubZoneAPIReportsLocalRoutes(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.NodeName = "dns-one" })
	z := zoneObj("example", "example.com.")
	z.Spec.Type = v1alpha1.ZoneTypeStub
	z.Spec.Stub = &v1alpha1.StubSpec{Nameservers: []string{"ns.example.net:5300"}}
	z.Status = v1alpha1.DNSZoneStatus{Serial: 12345, RecordCount: 999, Conditions: []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue}}}
	h.seed("api", z)
	var detail ZoneDetail
	h.do(http.MethodGet, "/api/v1/zones/example", "zoneadmin", nil).want(http.StatusOK).json(&detail)
	local := detail.StubRuntime
	if local == nil || local.Node != "dns-one" || local.State != "pending" || local.Serial != nil || detail.Ready || detail.Serial != 0 || detail.RecordCount != 0 || local.Authorities == nil {
		t.Fatalf("shared status misrepresented as usable stub route: %+v %+v", detail, local)
	}
	h.do(http.MethodPost, "/api/v1/zones/example/records", "zoneadmin", ep("www", "A", "192.0.2.1")).want(http.StatusConflict)
	h.do(http.MethodPost, "/api/v1/zones/example/import", "zoneadmin", exampleZoneFile, "Content-Type", "text/dns").want(http.StatusConflict)
	h.do(http.MethodPost, "/api/v1/zones/example/notify", "zoneadmin", nil).want(http.StatusConflict)
	h.do(http.MethodGet, "/api/v1/zones/example/export", "zoneadmin", nil).want(http.StatusConflict)
}

func TestStubRuntimeKeepsLastKnownDiscoverySeparateFromAvailability(t *testing.T) {
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	st := stub.Status{Serial: 0, Available: true, ConfirmedAt: now, ExpiresAt: now.Add(time.Hour), Error: "bootstrap authority offline",
		Servers: []zone.StubServer{{Name: "ns.example.", Address: netip.MustParseAddrPort("192.0.2.53:5300"), Protocol: "tcp"}}}
	r := stubRuntime("dns-one", st, true)
	if r.State != "routing" || r.Serial == nil || *r.Serial != 0 || r.Message != st.Error || len(r.Authorities) != 1 || r.Authorities[0].Address != "192.0.2.53:5300" {
		t.Fatalf("unexpired route must remain usable despite a failed discovery: %+v", r)
	}
	st.Available = false
	r = stubRuntime("dns-one", st, true)
	if r.State != "expired" || r.Serial == nil || !r.ExpiresAt.Equal(st.ExpiresAt) {
		t.Fatalf("expired route must retain diagnostics without reporting availability: %+v", r)
	}
	st.ConfigurationError = "apex conflict"
	r = stubRuntime("dns-one", st, true)
	if r.State != "unavailable" || r.Reason != "ConfigurationUnavailable" {
		t.Fatalf("configuration conflict must override route availability: %+v", r)
	}
}
