package listfetcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func sha(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// clock is a settable fake clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// listServer is an httptest list host with switchable behaviour.
type listServer struct {
	*httptest.Server
	mu     sync.Mutex
	body   []byte
	etag   string
	lastMo string
	mode   string // "", "500", "html", "404"
	hits   atomic.Int64
	conds  atomic.Int64 // requests carrying If-None-Match or If-Modified-Since
}

func newListServer(t *testing.T, body string) *listServer {
	s := &listServer{body: []byte(body), etag: `"v1"`, lastMo: "Thu, 01 Oct 2026 10:00:00 GMT"}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *listServer) set(mode, body, etag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = mode
	if body != "" {
		s.body, s.etag = []byte(body), etag
	}
}

func (s *listServer) serve(w http.ResponseWriter, r *http.Request) {
	s.hits.Add(1)
	s.mu.Lock()
	mode, body, etag, lastMo := s.mode, s.body, s.etag, s.lastMo
	s.mu.Unlock()
	if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
		s.conds.Add(1)
	}
	switch mode {
	case "500":
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	case "404":
		http.NotFound(w, r)
		return
	case "html":
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<!DOCTYPE html><html><body>captive portal</body></html>")
		return
	}
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Last-Modified", lastMo)
	_, _ = w.Write(body)
}

// countingStore counts UpdateStatus calls and can inject conflicts.
type countingStore struct {
	store.Store
	updates   atomic.Int64
	conflicts atomic.Int64 // UpdateStatus calls to fail with ErrConflict
}

func (c *countingStore) UpdateStatus(ctx context.Context, obj store.Object) error {
	if c.conflicts.Load() > 0 {
		c.conflicts.Add(-1)
		return store.ErrConflict
	}
	c.updates.Add(1)
	return c.Store.UpdateStatus(ctx, obj)
}

// storeObjects reads BlockLists straight from the store (no zonecache).
type storeObjects struct {
	st   store.Store
	init chan struct{}
}

func newStoreObjects(st store.Store) *storeObjects {
	ch := make(chan struct{})
	close(ch)
	return &storeObjects{st: st, init: ch}
}

func (o *storeObjects) BlockLists() []*v1alpha1.BlockList {
	l, err := o.st.List(context.Background(), "BlockList", store.ListOptions{})
	if err != nil {
		panic(err)
	}
	out := make([]*v1alpha1.BlockList, 0, len(l.Items))
	for _, it := range l.Items {
		out = append(out, it.(*v1alpha1.BlockList))
	}
	return out
}
func (o *storeObjects) Subscribe() <-chan struct{}   { return make(chan struct{}) }
func (o *storeObjects) Initialized() <-chan struct{} { return o.init }
func (o *storeObjects) Live() bool                   { return true }

func applyList(t testing.TB, st store.Store, name string, sources ...v1alpha1.ListSource) *v1alpha1.BlockList {
	t.Helper()
	bl := &v1alpha1.BlockList{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1alpha1.BlockListSpec{Mode: "block", Sources: sources, RefreshInterval: metav1.Duration{Duration: time.Hour}},
	}
	out, err := st.Apply(context.Background(), bl, store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	return out.(*v1alpha1.BlockList)
}

func getList(t testing.TB, st store.Store, name string) *v1alpha1.BlockList {
	t.Helper()
	bl := &v1alpha1.BlockList{}
	if err := st.Get(context.Background(), store.Key{Kind: "BlockList", Namespace: "default", Name: name}, bl); err != nil {
		t.Fatal(err)
	}
	return bl
}

// sink records snapshots handed to SetLists.
type sink struct {
	mu    sync.Mutex
	snaps []*blocking.Snapshot
}

func (s *sink) SetLists(b *blocking.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snaps = append(s.snaps, b)
}

func (s *sink) last() *blocking.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.snaps) == 0 {
		return nil
	}
	return s.snaps[len(s.snaps)-1]
}

func (s *sink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.snaps)
}

func storeKey(name string) store.Key {
	return store.Key{Kind: "BlockList", Namespace: "default", Name: name}
}

func deleteOpts() store.DeleteOptions { return store.DeleteOptions{Manager: store.ManagerAPI} }

// fixture is one leader: store, cache, downloader and fetcher.
type fixture struct {
	clk   *clock
	st    *countingStore
	cache *DiskCache
	dl    *Downloader
	f     *Fetcher
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clk := newClock()
	st := &countingStore{Store: memory.New()}
	cache, err := OpenDiskCache(t.TempDir(), quiet())
	if err != nil {
		t.Fatal(err)
	}
	dl := NewDownloader(cache, DownloadOptions{Now: clk.Now, MaxHTTPSize: 1 << 20, AllowLocalAddresses: true})
	dl.sleep = func(context.Context, time.Duration) error { return nil }
	f, err := New(Options{Store: st, Objects: newStoreObjects(st), Cache: cache, Downloader: dl, Logger: quiet(), Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{clk: clk, st: st, cache: cache, dl: dl, f: f}
}
