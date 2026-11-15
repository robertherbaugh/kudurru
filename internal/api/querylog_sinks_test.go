package api

import (
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/querylog"
)

// TestQuerylogSinksReportsTheNodesSinkHost: GET /querylog/sinks returns the
// sink host's state of the node that answers, with querylog.read, and no
// field beyond the documented counters.
func TestQuerylogSinksReportsTheNodesSinkHost(t *testing.T) {
	last := time.Date(2026, 11, 11, 9, 30, 0, 0, time.FixedZone("x", 3600))
	h := newHarness(t, func(o *Options) {
		o.QuerylogSinks = func() []querylog.SinkStatus {
			return []querylog.SinkStatus{
				{Name: "collector", Type: "dnstap", Raw: true, Queued: 3, QueueSize: 100, Sent: 40, Dropped: 2, Errors: 1, LastSuccess: last},
				{Name: "warehouse", Type: "clickhouse", QueueSize: 10000},
			}
		}
	})
	var body QuerylogSinksBody
	h.do("GET", "/api/v1/querylog/sinks", "operator", nil).want(200).json(&body)
	if body.Node != "node-a" || len(body.Sinks) != 2 {
		t.Fatalf("body %+v", body)
	}
	got := body.Sinks[0]
	if got.Name != "collector" || got.Type != "dnstap" || !got.Raw || got.Queued != 3 || got.QueueSize != 100 || got.Sent != 40 || got.Dropped != 2 || got.Errors != 1 || got.LastSuccess == nil || !got.LastSuccess.Equal(last) {
		t.Errorf("collector %+v", got)
	}
	if got := body.Sinks[1]; got.Name != "warehouse" || got.Raw || got.LastSuccess != nil {
		t.Errorf("warehouse %+v", got)
	}
	raw := h.do("GET", "/api/v1/querylog/sinks", "auditor", nil).want(200).Body.String()
	if strings.Contains(raw, `"last_success":"0001`) || strings.Count(raw, `"last_success"`) != 1 {
		t.Errorf("a sink that never wrote reports a last success: %s", raw)
	}
	h.do("GET", "/api/v1/querylog/sinks", "viewer", nil).want(403)
	h.do("GET", "/api/v1/querylog/sinks", "", nil).want(401)

	// A server without a sink host reports none, never null.
	none := newHarness(t)
	raw = none.do("GET", "/api/v1/querylog/sinks", "admin", nil).want(200).Body.String()
	if !strings.Contains(raw, `"sinks":[]`) {
		t.Errorf("no sink host: %s", raw)
	}
}
