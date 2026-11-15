package apiclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRoleBindingCommandNeverRedirectsOrReplays(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		for _, status := range []int{301, 302, 303, 307, 308} {
			t.Run(method+"/"+strconv.Itoa(status), func(t *testing.T) {
				var attempts, destinations, redirects atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/destination" {
						destinations.Add(1)
						w.WriteHeader(http.StatusNoContent)
						return
					}
					attempts.Add(1)
					http.Redirect(w, r, "/destination", status)
				}))
				defer srv.Close()
				hc := srv.Client()
				hc.CheckRedirect = func(*http.Request, []*http.Request) error { redirects.Add(1); return nil }
				c, err := New(srv.URL, Options{HTTPClient: hc})
				if err != nil {
					t.Fatal("client")
				}
				path := "/iam/rolebindings"
				if method != http.MethodPost {
					path += "/apps/reviewed"
				}
				err = c.RoleBindingCommand(t.Context(), method, path, map[string]string{"uid": "original", "resourceVersion": "original"}, nil)
				if !IsStatus(err, status) || attempts.Load() != 1 || destinations.Load() != 0 || redirects.Load() != 0 {
					t.Fatal("original refusal changed or mutation was replayed")
				}
				// The per-request redirect override must not mutate the shared client.
				if hc.CheckRedirect == nil {
					t.Fatal("shared redirect policy changed")
				}
			})
		}
	}
}

func TestRoleBindingCommandBodyAndResponseBounds(t *testing.T) {
	var calls atomic.Int32
	transport := totpTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.GetBody != nil || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-Idempotency-Key") != "" {
			t.Error("mutation retained replay authority")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"public":"` + strings.Repeat("x", 64<<10) + `"}`)), Request: r}, nil
	})
	c, err := New("https://kudurru.test", Options{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal("client")
	}
	var out map[string]string
	if err := c.RoleBindingCommand(t.Context(), http.MethodPut, "/iam/rolebindings/apps/reviewed", map[string]string{"uid": "original", "resourceVersion": "original"}, &out); err != nil || len(out["public"]) != 64<<10 {
		t.Fatal("supported public response exceeded client bound")
	}
	for _, invalid := range []struct{ method, path string }{{http.MethodGet, "/iam/rolebindings"}, {http.MethodPost, "/objects/rolebindings/apps/one"}, {http.MethodPut, "/iam/rolebindings/apps/one/extra"}} {
		if err := c.RoleBindingCommand(t.Context(), invalid.method, invalid.path, map[string]string{}, nil); err == nil {
			t.Fatal("unknown operation accepted")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("invalid operation sent I/O")
	}
	c.http.Transport = totpTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"public":"` + strings.Repeat("x", 1<<20) + `"}`)), Request: r}, nil
	})
	if err := c.RoleBindingCommand(t.Context(), http.MethodPut, "/iam/rolebindings/apps/reviewed", map[string]string{}, &out); err == nil {
		t.Fatal("unbounded successful body accepted")
	}
	c.http.Transport = totpTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 409, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"key":"conflict","message":"review changed"}}`)), Request: r}, nil
	})
	if err := c.RoleBindingCommand(t.Context(), http.MethodDelete, "/iam/rolebindings/apps/reviewed", map[string]string{}, nil); !IsStatus(err, 409) || !IsKey(err, "conflict") {
		t.Fatal("server CAS refusal was hidden")
	}
}
