package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/central"
)

type centralStub struct{ st central.State }

func (c centralStub) State() central.State { return c.st }

func TestCentralDiagnosis(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC) // the harness clock
	for _, tc := range []struct {
		name   string
		st     central.State
		reason string
	}{
		{"ready", central.State{Ready: true, Since: now.Add(-time.Hour)}, ""},
		{"briefly away", central.State{LastError: "connection refused", Since: now.Add(-10 * time.Second)}, ""},
		{"away", central.State{LastError: "connection refused", Since: now.Add(-2 * time.Minute)}, "CentralUnreachable"},
		{"incompatible", central.State{Incompatible: true, SchemaVersion: 7, CompatVersion: 5, Since: now}, "CentralIncompatible"},
	} {
		h := newHarness(t, func(o *Options) {
			o.Central = centralStub{tc.st}
			o.Now = func() time.Time { return now }
		})
		var list struct {
			Items []DiagnosisMessage `json:"items"`
		}
		h.do(http.MethodGet, "/api/v1/info/messages", "viewer", nil).want(http.StatusOK).json(&list)
		got := ""
		for _, m := range list.Items {
			if m.Source == "central" {
				got = m.Reason
			}
		}
		if got != tc.reason {
			t.Errorf("%s: reason %q, want %q (%+v)", tc.name, got, tc.reason, list.Items)
		}
	}
}
