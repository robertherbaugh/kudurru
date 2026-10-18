package persist

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Unix(1_800_000_000, 0)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func openDB(t testing.TB, path string) *localdb.SnapshotStore {
	t.Helper()
	db, err := localdb.OpenSnapshotStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func key(name string) cache.Key {
	return cache.MakeKey(name, dns.TypeA, dns.ClassINET, false, false, netip.Prefix{})
}

// answer packs a realistic A response for name.
func answer(tb testing.TB, name string, i int) []byte {
	tb.Helper()
	m := new(dns.Msg)
	m.SetQuestion(name, dns.TypeA)
	m.Id = 0 // deterministic bytes; the cache stage rewrites IDs anyway
	m.Response, m.RecursionAvailable = true, true
	m.Answer = []dns.RR{&dns.A{
		Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300},
		A:   net.IP(binary.BigEndian.AppendUint32(nil, 10<<24|uint32(i)&0xffffff)), // #nosec G115 -- test index
	}}
	b, err := m.Pack()
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

func fill(tb testing.TB, c *cache.Cache, n int, ttl time.Duration) {
	tb.Helper()
	for i := range n {
		name := fmt.Sprintf("host-%d.example-%d.com.", i, i%997)
		c.Set(key(name), answer(tb, name, i), cache.Positive, ttl)
	}
}

func TestSaveRestoreAcrossReopen(t *testing.T) {
	for _, noCompress := range []bool{false, true} {
		t.Run(fmt.Sprintf("noCompress=%v", noCompress), func(t *testing.T) {
			ctx := context.Background()
			clk := newClock()
			path := filepath.Join(t.TempDir(), localdb.FileName)
			db := openDB(t, path)
			p, err := New(db, Options{NoCompress: noCompress, Now: clk.Now})
			if err != nil {
				t.Fatal(err)
			}
			src := cache.New(cache.Options{MaxEntries: 10_000, StaleTTL: time.Hour, Now: clk.Now})
			fill(t, src, 1000, 10*time.Minute)
			src.Set(key("short.example."), answer(t, "short.example.", 1), cache.Positive, time.Minute)
			src.Set(key("nx.example."), []byte("negative"), cache.Negative, time.Hour)
			saved, err := p.Save(ctx, src)
			if err != nil {
				t.Fatal(err)
			}
			if saved.Entries != 1002 || saved.Bytes == 0 || !saved.WrittenAt.Equal(clk.Now()) {
				t.Fatalf("save stats %+v", saved)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			// A restart 5 minutes later: short.example. is stale but inside
			// its stale window, everything else fresh.
			clk.Advance(5 * time.Minute)
			db2 := openDB(t, path)
			p2, err := New(db2, Options{Now: clk.Now})
			if err != nil {
				t.Fatal(err)
			}
			dst := cache.New(cache.Options{MaxEntries: 10_000, StaleTTL: time.Hour, Now: clk.Now})
			got, err := p2.Restore(ctx, dst)
			if err != nil {
				t.Fatal(err)
			}
			if got.Entries != 1002 || got.Bytes != saved.Bytes || !got.WrittenAt.Equal(saved.WrittenAt) {
				t.Fatalf("restore stats %+v, saved %+v", got, saved)
			}
			l, ok := dst.Peek(key("host-7.example-7.com."))
			if !ok || l.State != cache.Fresh || l.Age != 5*time.Minute || string(l.Wire) != string(answer(t, "host-7.example-7.com.", 7)) {
				t.Fatalf("restored entry %+v %v", l, ok)
			}
			if l, ok := dst.Peek(key("short.example.")); !ok || l.State != cache.Stale {
				t.Fatalf("short.example. should be restored stale: %+v %v", l, ok)
			}
			if l, ok := dst.Peek(key("nx.example.")); !ok || l.Kind != cache.Negative {
				t.Fatalf("negative entry lost: %+v %v", l, ok)
			}
		})
	}
}

func TestRestoreMissingStaleAndIncompatible(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	db := openDB(t, filepath.Join(t.TempDir(), localdb.FileName))
	p, err := New(db, Options{Now: clk.Now, MaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	empty := func() *cache.Cache {
		return cache.New(cache.Options{MaxEntries: 100, StaleTTL: 72 * time.Hour, Now: clk.Now})
	}

	if st, err := p.Restore(ctx, empty()); err != nil || st != (cache.PersistStats{}) {
		t.Fatalf("no dump: %+v %v", st, err)
	}

	src := empty()
	fill(t, src, 10, 48*time.Hour)
	if _, err := p.Save(ctx, src); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Hour)
	c := empty()
	if _, err := p.Restore(ctx, c); !errors.Is(err, ErrStale) || c.Len() != 0 {
		t.Fatalf("dump older than MaxAge: err %v, %d entries loaded", err, c.Len())
	}

	for name, blob := range map[string][]byte{
		"short":         []byte("KDRP"),
		"bad magic":     []byte("XXXX\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00"),
		"newer version": []byte("KDRP\x02\x00\x00\x00\x00\x00\x00\x00\x00\x00"),
		"unknown flag":  []byte("KDRP\x01\x80\x00\x00\x00\x00\x00\x00\x00\x00"),
	} {
		if err := db.PutBlob(ctx, BlobName, blob); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Restore(ctx, empty()); !errors.Is(err, ErrIncompatible) {
			t.Errorf("%s: want ErrIncompatible, got %v", name, err)
		}
	}

	// A header that is fine around a damaged payload is a cache.ErrBadDump.
	hdr := make([]byte, headerLen)
	copy(hdr, magic)
	hdr[len(magic)] = version
	hdr[len(magic)+1] = flagGzip
	putTime(hdr, clk.Now())
	if err := db.PutBlob(ctx, BlobName, append(hdr, "not gzip"...)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Restore(ctx, empty()); !errors.Is(err, cache.ErrBadDump) {
		t.Errorf("damaged payload: want cache.ErrBadDump, got %v", err)
	}

	if _, err := New(nil, Options{}); err == nil {
		t.Error("New without a BlobStore must fail")
	}
}

func putTime(hdr []byte, t time.Time) {
	binary.BigEndian.PutUint64(hdr[len(magic)+2:], uint64(t.UnixNano())) // #nosec G115 -- test clock is post-1970
}

// TestRunPersisterFinalSave: cache.RunPersister with this Persister writes
// the dump when its context ends, and a new process restores it.
func TestRunPersisterFinalSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), localdb.FileName)
	db := openDB(t, path)
	p, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := cache.New(cache.Options{MaxEntries: 1000})
	fill(t, c, 100, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cache.RunPersister(ctx, c, p, 0, time.Second, nil) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	d := cache.New(cache.Options{MaxEntries: 1000})
	st, err := p.Restore(context.Background(), d)
	if err != nil || st.Entries != 100 || d.Len() != 100 {
		t.Fatalf("restored %+v (%d entries): %v", st, d.Len(), err)
	}
}

// BenchmarkPersist200k measures Save and Restore of a full default-size
// cache (200 k packed A answers) through local.db, reporting the blob size.
func BenchmarkPersist200k(b *testing.B) {
	for _, noCompress := range []bool{false, true} {
		b.Run(fmt.Sprintf("gzip=%v", !noCompress), func(b *testing.B) {
			ctx := context.Background()
			db := openDB(b, filepath.Join(b.TempDir(), localdb.FileName))
			p, err := New(db, Options{NoCompress: noCompress})
			if err != nil {
				b.Fatal(err)
			}
			src := cache.New(cache.Options{MaxEntries: 200_000, StaleTTL: 72 * time.Hour})
			fill(b, src, 200_000, time.Hour)
			b.Run("save", func(b *testing.B) {
				var st cache.PersistStats
				for b.Loop() {
					if st, err = p.Save(ctx, src); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(st.Bytes), "bytes")
				b.ReportMetric(float64(st.Entries), "entries")
			})
			b.Run("restore", func(b *testing.B) {
				for b.Loop() {
					dst := cache.New(cache.Options{MaxEntries: 200_000, StaleTTL: 72 * time.Hour})
					if _, err := p.Restore(ctx, dst); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}
