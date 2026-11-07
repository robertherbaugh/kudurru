package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHumanDetailsRemainDiagnostic(t *testing.T) {
	c := New(map[string]any{"node": "local", "version": "test"})
	read := func(code int, want Details) {
		t.Helper()
		rec := httptest.NewRecorder()
		c.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		var body struct {
			Leader      string `json:"leader"`
			SnapshotAge string `json:"snapshotAge"`
			Node        string `json:"node"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if rec.Code != code || body.Leader != want.Leader || body.SnapshotAge != want.SnapshotAge || body.Node != "local" {
			t.Fatalf("health: status=%d leader=%q age=%q node=%q", rec.Code, body.Leader, body.SnapshotAge, body.Node)
		}
	}
	read(http.StatusOK, Details{})
	details := Details{Leader: "peer", SnapshotAge: "1m0s"}
	calls := 0
	c.SetDetails(func() Details {
		calls++
		// A callback can inspect checker state: it must run outside its lock.
		_ = c.IsLameduck()
		return details
	})
	read(http.StatusOK, details)
	details = Details{SnapshotAge: "48h0m0s"}
	c.Set("store", false, "stale")
	read(http.StatusServiceUnavailable, details)
	for _, path := range []string{"/healthz/ready", "/healthz/live", "/healthz/startup"} {
		c.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	if calls != 2 {
		t.Fatalf("probe invoked diagnostic callback: %d", calls)
	}
	c.SetDetails(nil)
	read(http.StatusServiceUnavailable, Details{})
}
