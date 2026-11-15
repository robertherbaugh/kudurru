package apiclient

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIntegrationCallDoesNotRedirectOrReplay(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test-only" {
					t.Error("original method or credential missing")
				}
				w.Header().Set("Location", "/api/v1/integrations/default/demo/sync")
				w.WriteHeader(status)
			}))
			defer server.Close()
			c, err := New(server.URL, Options{Token: "test-only", Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.IntegrationCall(t.Context(), http.MethodPost, "/integrations/default/demo/sync", nil, map[string]string{"uid": "original", "revision": "1", "mode": "observe"}); err == nil {
				t.Fatal("unacknowledged command succeeded")
			}
			if calls.Load() != 1 {
				t.Fatal("command replayed", calls.Load())
			}
		})
	}
}

func TestIntegrationCallBoundsAcknowledgment(t *testing.T) {
	for _, body := range []string{"", `null`, `[]`, `null trailing`, `{"value":"` + strings.Repeat("x", 1<<20) + `"}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		c, err := New(server.URL, Options{Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.IntegrationCall(t.Context(), http.MethodGet, "/integrations", nil, nil)
		server.Close()
		if err == nil {
			t.Fatal("unbounded or invalid response accepted")
		}
	}
}
