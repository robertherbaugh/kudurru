package kube

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

type failingRT struct{}

func (failingRT) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused")
}

func TestContactTracker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = io.WriteString(w, "hello")
		case "/missing":
			http.NotFound(w, r)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	clk := &stepClock{t: time.Unix(1_800_000_000, 0)}
	c := &contact{now: clk.now}
	if !c.Last().IsZero() {
		t.Fatal("LastContact before any request must be zero")
	}
	hc := &http.Client{Transport: c.wrap(http.DefaultTransport)}
	get := func(path string) {
		t.Helper()
		resp, err := hc.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	get("/unavailable")
	if !c.Last().IsZero() {
		t.Fatal("a 503 is not contact with the apiserver")
	}
	get("/missing")
	afterMissing := c.Last()
	if afterMissing.IsZero() {
		t.Fatal("a 404 is a healthy apiserver answer")
	}
	get("/ok") // round trip plus body reads
	afterOK := c.Last()
	if !afterOK.After(afterMissing) {
		t.Fatalf("LastContact did not advance: %s then %s", afterMissing, afterOK)
	}

	// Body reads (watch streams) mark contact too.
	body := &contactBody{ReadCloser: io.NopCloser(strings.NewReader("event")), c: c}
	if _, err := io.ReadAll(body); err != nil {
		t.Fatal(err)
	}
	if !c.Last().After(afterOK) {
		t.Fatal("reading a response body must mark contact")
	}

	// Transport errors never mark.
	before := c.Last()
	dead := &http.Client{Transport: c.wrap(failingRT{})}
	if _, err := dead.Get(srv.URL + "/ok"); err == nil {
		t.Fatal("expected a transport error")
	}
	if !c.Last().Equal(before) {
		t.Fatal("a failed round trip must not mark contact")
	}
}
