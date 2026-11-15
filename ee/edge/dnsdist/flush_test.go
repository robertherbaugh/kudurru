package dnsdist

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type flushRecorder struct {
	mu   sync.Mutex
	reqs []string
}

func (r *flushRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

func newFlushServer(t *testing.T, key string) (*httptest.Server, *flushRecorder) {
	t.Helper()
	rec := &flushRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/cache" {
			http.NotFound(w, r)
			return
		}
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, r.URL.RawQuery)
		rec.mu.Unlock()
		_, _ = io.WriteString(w, `{"count":"3","status":"purged"}`)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestCacheFlusherFlushResolver(t *testing.T) {
	srv, rec := newFlushServer(t, "k1")
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "dnsdist-api.key")
	f := NewLocalCacheFlusher(srv.URL, keyFile, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := f.FlushResolver(context.Background()); !errors.Is(err, ErrNoLocalEdge) {
		t.Fatalf("without a key file: %v, want ErrNoLocalEdge", err)
	}
	if err := os.WriteFile(keyFile, []byte("k1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.FlushResolver(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec.reqs[0] != "name=.&pool=resolver&suffix=true" {
		t.Errorf("flush query %q", rec.reqs[0])
	}
	if err := os.WriteFile(keyFile, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.FlushResolver(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("stale key: %v", err)
	}
	if d := NewLocalCacheFlusher("", "", nil); d.API.BaseURL != DefaultAPIURL || d.API.KeyFile != DefaultAPIKeyFile {
		t.Errorf("defaults %+v", d.API)
	}
}

func TestCacheFlusherBlockingChanged(t *testing.T) {
	srv, rec := newFlushServer(t, "k")
	f := &CacheFlusher{API: &APIClient{BaseURL: srv.URL, Key: "k"}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	wait := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for rec.count() < n {
			if time.Now().After(deadline) {
				t.Fatalf("%d flushes, want %d", rec.count(), n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	// Enable: one flush.
	f.BlockingChanged(time.Time{})
	wait(1)
	// Temporary disable: a flush now and one when blocking resumes.
	f.BlockingChanged(time.Now().Add(150 * time.Millisecond))
	wait(2)
	wait(3)
	// Disable without a timer: no delayed flush; a later enable replaces
	// a pending one.
	f.BlockingChanged(time.Now().Add(1000 * 24 * time.Hour))
	wait(4)
	f.BlockingChanged(time.Now().Add(time.Hour))
	wait(5)
	f.Stop()
	f.BlockingChanged(time.Time{}) // after Stop: ignored
	time.Sleep(50 * time.Millisecond)
	if n := rec.count(); n != 5 {
		t.Errorf("%d flushes after Stop, want 5", n)
	}
}

func TestCacheFlusherCoalescesHeldPublicationChanges(t *testing.T) {
	var calls, active, peak atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for p := peak.Load(); n > p; p = peak.Load() {
			if peak.CompareAndSwap(p, n) {
				break
			}
		}
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		_, _ = io.WriteString(w, `{"count":"1","status":"purged"}`)
	}))
	t.Cleanup(srv.Close)
	f := &CacheFlusher{API: &APIClient{BaseURL: srv.URL, Key: "fixture"}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	t.Cleanup(func() { unblock(); f.Stop() })
	f.BlockResponsesChanged()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("flush did not reach hold")
	}
	for range 1000 {
		f.BlockResponsesChanged()
	}
	if calls.Load() != 1 || peak.Load() != 1 {
		t.Fatal("publication callbacks spawned concurrent flushes")
	}
	unblock()
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("coalesced pending publication was lost")
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.Stop()
	if calls.Load() != 2 || peak.Load() != 1 || active.Load() != 0 {
		t.Fatal("flush coalescing or shutdown escaped bounds")
	}
}
