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

func TestClusterJoinTokenNeverRedirectsOrReplays(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var requests, destinations atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/destination" {
					destinations.Add(1)
					return
				}
				requests.Add(1)
				if status == 503 {
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":{"key":"completion_unknown","message":"inspect enrollment"}}`)
					return
				}
				http.Redirect(w, r, "/destination", status)
			}))
			defer srv.Close()
			c, err := New(srv.URL, Options{HTTPClient: srv.Client()})
			if err != nil {
				t.Fatal(err)
			}
			err = c.IssueClusterJoinToken(t.Context(), map[string]string{}, nil)
			if !IsStatus(err, status) || requests.Load() != 1 || destinations.Load() != 0 {
				t.Fatal("invitation request replayed", err)
			}
			if status == 503 && !IsKey(err, "completion_unknown") {
				t.Fatal("unknown outcome replaced", err)
			}
		})
	}
}

func TestClusterJoinTokenHasNoReplayBody(t *testing.T) {
	transport := totpTransport(func(r *http.Request) (*http.Response, error) {
		if r.GetBody != nil || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-Idempotency-Key") != "" {
			t.Error("invitation request retained replay authority")
		}
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"key":"completion_unknown","message":"inspect enrollment"}}`)), Request: r}, nil
	})
	c, err := New("https://kudurru.test", Options{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.IssueClusterJoinToken(t.Context(), map[string]string{}, nil); !IsKey(err, "completion_unknown") {
		t.Fatal("unknown result replaced", err)
	}
}
