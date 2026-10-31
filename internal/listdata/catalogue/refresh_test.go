package catalogue

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// docServer serves a catalogue document with an ETag.
type docServer struct {
	mu       sync.Mutex
	body     string
	status   int
	requests int
	notMod   int
}

func (d *docServer) set(status int, body string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status, d.body = status, body
}

func (d *docServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests++
	etag := `"` + digestOf([]byte(d.body)) + `"`
	if d.status == http.StatusOK && r.Header.Get("If-None-Match") == etag {
		d.notMod++
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	w.WriteHeader(d.status)
	_, _ = io.WriteString(w, d.body)
}

func doc(t *testing.T, version string, services ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"schemaVersion":1,"version":"` + version + `","license":"Apache-2.0","services":[`)
	for i, s := range services {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"id":"` + s + `","name":"` + s + `","category":"video","rules":["||` + s + `.example^"],"source":"test"}`)
	}
	b.WriteString(`],"safeSearch":[]}`)
	return b.String()
}

func TestRefresherLeader(t *testing.T) {
	srv := &docServer{}
	srv.set(http.StatusOK, doc(t, "2099.01.01", "alpha"))
	ts := httptest.NewServer(srv)
	defer ts.Close()
	dir := t.TempDir()
	h := NewHolder(nil)
	r, err := NewRefresher(RefresherOptions{URL: ts.URL + "/catalogue.json", Holder: h, Dir: dir, Logger: quiet(), RefreshInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if wait := r.Refresh(ctx); wait != time.Hour || !h.HasService("alpha") || h.Current().Origin() != ts.URL+"/catalogue.json" {
		t.Fatalf("first refresh: wait %v, services %v", wait, h.Current().Services)
	}
	if st := r.Status(); st.LastError != "" || st.LastSuccess.IsZero() {
		t.Errorf("status = %+v", st)
	}
	// Unchanged: a conditional GET answered 304.
	r.Refresh(ctx)
	if srv.notMod != 1 {
		t.Errorf("304s = %d", srv.notMod)
	}
	// A newer schema version is refused; the catalogue in service stays.
	srv.set(http.StatusOK, strings.Replace(doc(t, "2099.02.01", "beta"), `"schemaVersion":1`, `"schemaVersion":3`, 1))
	if wait := r.Refresh(ctx); wait != retryAfter || !h.HasService("alpha") {
		t.Errorf("schema 3: wait %v, alpha kept %v", wait, h.HasService("alpha"))
	}
	if st := r.Status(); !strings.Contains(st.LastError, "unsupported schemaVersion 3") {
		t.Errorf("status = %+v", st)
	}
	// Invalid documents and HTTP errors keep it too.
	srv.set(http.StatusOK, `{"schemaVersion":1,"version":"x","license":"Apache-2.0","services":[{"id":"Bad"}]}`)
	r.Refresh(ctx)
	srv.set(http.StatusInternalServerError, "oops")
	r.Refresh(ctx)
	if !h.HasService("alpha") {
		t.Error("a failed refresh replaced the catalogue")
	}
	// A restart serves the kept download before the first fetch.
	h2 := NewHolder(nil)
	r2, _ := NewRefresher(RefresherOptions{URL: ts.URL + "/catalogue.json", Holder: h2, Dir: dir, Logger: quiet()})
	r2.loadPersisted()
	if !h2.HasService("alpha") {
		t.Error("the kept download was not loaded")
	}
	// ... but not when the URL changed.
	h3 := NewHolder(nil)
	r3, _ := NewRefresher(RefresherOptions{URL: ts.URL + "/other.json", Holder: h3, Dir: dir, Logger: quiet()})
	r3.loadPersisted()
	if h3.Current() != Embedded() {
		t.Error("a download of another URL was loaded")
	}
}

func TestRefresherReplica(t *testing.T) {
	srv := &docServer{}
	srv.set(http.StatusOK, doc(t, "2099.01.01", "self"))
	ts := httptest.NewServer(srv)
	defer ts.Close()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	leaderDoc := doc(t, "2099.01.02", "fromleader")
	var pullErr error
	pull := func(_ context.Context, addr string) ([]byte, string, error) {
		if addr != "10.0.0.1:8082" {
			t.Errorf("pulled from %s", addr)
		}
		return []byte(leaderDoc), ts.URL, pullErr
	}
	h := NewHolder(nil)
	r, err := NewRefresher(RefresherOptions{
		URL: ts.URL, Holder: h, Logger: quiet(), Pull: pull, Now: func() time.Time { return now },
		Membership: &cluster.Static{SelfName: "b", LeaderName: "a", Members: []cluster.Peer{{Name: "a", Address: "10.0.0.1:8082"}, {Name: "b"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if wait := r.Refresh(ctx); wait != replicaPoll || !h.HasService("fromleader") || srv.requests != 0 {
		t.Fatalf("replica: wait %v, services %v, downloads %d", wait, h.Current().Services, srv.requests)
	}
	// The leader is unreachable: keep what we have, then fall back to a
	// direct download after FallbackAfter.
	pullErr = errors.New("connection refused")
	r.Refresh(ctx)
	if srv.requests != 0 || !h.HasService("fromleader") {
		t.Fatal("downloaded before FallbackAfter")
	}
	now = now.Add(DefaultFallbackAfter)
	r.Refresh(ctx)
	if srv.requests != 1 || !h.HasService("self") {
		t.Errorf("fallback: downloads %d, services %v", srv.requests, h.Current().Services)
	}
}

func TestNewRefresherRejects(t *testing.T) {
	for _, u := range []string{"", "ftp://x/c.json", "file:///etc/passwd"} {
		if _, err := NewRefresher(RefresherOptions{URL: u, Holder: NewHolder(nil)}); err == nil {
			t.Errorf("URL %q accepted", u)
		}
	}
}
