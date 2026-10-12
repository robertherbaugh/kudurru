package health

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLameduckRoute(t *testing.T) {
	c := New(nil)
	c.SetStarted()
	h := c.Handler()
	do := func(method, path, remote string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do(http.MethodGet, "/healthz/ready", "127.0.0.1:1"); w.Code != http.StatusOK {
		t.Fatalf("ready before lameduck: %d", w.Code)
	}
	if w := do(http.MethodPost, PathLameduck, "10.0.0.9:1234"); w.Code != http.StatusForbidden || c.IsLameduck() {
		t.Fatalf("remote POST: %d lameduck=%v", w.Code, c.IsLameduck())
	}
	if w := do(http.MethodPost, PathLameduck, "127.0.0.1:1234"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"lameduck":true`) {
		t.Fatalf("loopback POST: %d %s", w.Code, w.Body.String())
	}
	if !c.IsLameduck() {
		t.Fatal("not lameduck after POST")
	}
	if w := do(http.MethodGet, "/healthz/ready", "127.0.0.1:1"); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "lameduck") {
		t.Fatalf("ready during lameduck: %d %s", w.Code, w.Body.String())
	}
	if w := do(http.MethodGet, PathLameduck, "10.0.0.9:1"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "true") {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	if w := do(http.MethodDelete, PathLameduck, "[::1]:1234"); w.Code != http.StatusOK || c.IsLameduck() {
		t.Fatalf("DELETE: %d lameduck=%v", w.Code, c.IsLameduck())
	}
}
