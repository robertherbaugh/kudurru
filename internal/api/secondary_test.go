package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSecondaryZoneAPIHonorsLocalState(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.NodeName = "dns-one" })
	z := zoneObj("example", "example.com.")
	z.Spec.Type = v1alpha1.ZoneTypeSecondary
	z.Spec.Secondary = &v1alpha1.SecondarySpec{Primaries: []string{"ns.example.net:5300"}}
	z.Status = v1alpha1.DNSZoneStatus{Serial: 12345, RecordCount: 999, Conditions: []metav1.Condition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue}}}
	h.seed("api", z)
	var detail ZoneDetail
	h.do(http.MethodGet, "/api/v1/zones/example", "zoneadmin", nil).want(http.StatusOK).json(&detail)
	local := detail.SecondaryRuntime
	if local == nil || local.Node != "dns-one" || local.State != "pending" || local.Serial != nil || detail.Ready || detail.Serial != 0 || detail.RecordCount != 0 {
		t.Fatalf("shared status misrepresented as served data: %+v %+v", detail, local)
	}
	var list struct {
		Items []ZoneSummary `json:"items"`
	}
	h.do(http.MethodGet, "/api/v1/zones", "zoneadmin", nil).want(http.StatusOK).json(&list)
	if len(list.Items) != 1 || list.Items[0].SecondaryRuntime == nil || list.Items[0].SecondaryRuntime.Node != "dns-one" {
		t.Fatalf("list state %+v", list)
	}
	h.do(http.MethodPost, "/api/v1/zones/example/records", "zoneadmin", ep("www", "A", "192.0.2.1")).want(http.StatusConflict)
	h.do(http.MethodPost, "/api/v1/zones/example/import", "zoneadmin", exampleZoneFile, "Content-Type", "text/dns").want(http.StatusConflict)
	h.do(http.MethodPost, "/api/v1/zones/example/notify", "zoneadmin", nil).want(http.StatusConflict)
	h.do(http.MethodGet, "/api/v1/zones/example/export", "zoneadmin", nil).want(http.StatusConflict)
}

func TestSecondaryRuntimeStates(t *testing.T) {
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, state string
		st          secondary.Status
		serial      bool
	}{
		{name: "pending", state: "pending"},
		{name: "initial failure", state: "unavailable", st: secondary.Status{Error: "key unavailable"}},
		{name: "valid serial zero", state: "serving", serial: true, st: secondary.Status{Serial: 0, Available: true, RecordCount: 4, ConfirmedAt: now, ExpiresAt: now.Add(time.Hour)}},
		{name: "last good", state: "serving", serial: true, st: secondary.Status{Serial: 7, Available: true, ConfirmedAt: now, ExpiresAt: now.Add(time.Hour), Error: "primary offline"}},
		{name: "expired", state: "expired", serial: true, st: secondary.Status{Serial: 8, ConfirmedAt: now, ExpiresAt: now.Add(time.Hour)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := secondaryRuntime("dns-one", tc.st, true)
			if got.State != tc.state || (got.Serial != nil) != tc.serial || got.Node != "dns-one" {
				t.Fatalf("state %+v", got)
			}
			if tc.serial && (*got.Serial != tc.st.Serial || !got.LastConfirmedAt.Equal(tc.st.ConfirmedAt) || !got.ExpiresAt.Equal(tc.st.ExpiresAt)) {
				t.Fatalf("lost persisted state %+v", got)
			}
			if tc.st.Error != "" && got.Message != tc.st.Error {
				t.Fatal("lost refresh failure")
			}
		})
	}
}
