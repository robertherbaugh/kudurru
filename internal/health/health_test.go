package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadinessSources(t *testing.T) {
	c := New(map[string]any{"version": "test"})
	c.Set("store", false, "syncing")
	ok, bad := c.Ready()
	if ok || len(bad) != 1 || bad[0] != "store" {
		t.Fatalf("expected store unready, got ok=%v bad=%v", ok, bad)
	}
	c.Set("store", true, "")
	ready := false
	c.Register("dns", func() (bool, []string) {
		if ready {
			return true, nil
		}
		return false, []string{"doq:127.0.0.1:853"}
	})
	if ok, bad := c.Ready(); ok || bad[0] != "dns(doq:127.0.0.1:853)" {
		t.Fatalf("dynamic source should block readiness: ok=%v bad=%v", ok, bad)
	}
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/healthz should be 503 while unready, got %d", rec.Code)
	}
	var body struct {
		Status     string               `json:"status"`
		Components map[string]Component `json:"components"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "not-ready" || body.Components["dns"].Ready || body.Components["dns"].Message != "doq:127.0.0.1:853" || !body.Components["store"].Ready {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
	ready = true
	rec = httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz/ready", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz/ready should be 200 once every source is ready, got %d: %s", rec.Code, rec.Body.String())
	}
	c.Lameduck()
	rec = httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatal("lameduck must fail readiness")
	}
}
