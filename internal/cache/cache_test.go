package cache

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
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

func key(name string) Key { return MakeKey(name, 1, 1, false, false, netip.Prefix{}) }

func TestMakeKey(t *testing.T) {
	ecs4 := netip.MustParsePrefix("198.51.100.0/24")
	ecs6 := netip.MustParsePrefix("2001:db8:1234:5600::/56")
	base := MakeKey("Example.COM.", 1, 1, false, false, netip.Prefix{})
	if base != MakeKey("example.com.", 1, 1, false, false, netip.Prefix{}) {
		t.Fatal("keys must fold case")
	}
	distinct := []Key{
		base,
		MakeKey("example.com.", 28, 1, false, false, netip.Prefix{}),
		MakeKey("example.com.", 1, 3, false, false, netip.Prefix{}),
		MakeKey("example.com.", 1, 1, true, false, netip.Prefix{}),
		MakeKey("example.com.", 1, 1, false, true, netip.Prefix{}),
		MakeKey("example.com.", 1, 1, false, false, ecs4),
		MakeKey("example.com.", 1, 1, false, false, netip.MustParsePrefix("198.51.101.0/24")),
		MakeKey("example.com.", 1, 1, false, false, ecs6),
		MakeKey("example.org.", 1, 1, false, false, netip.Prefix{}),
	}
	seen := map[Key]int{}
	for i, k := range distinct {
		if j, dup := seen[k]; dup {
			t.Fatalf("keys %d and %d collide", i, j)
		}
		seen[k] = i
	}
	if MakeKey("a.", 1, 1, false, false, netip.MustParsePrefix("198.51.100.7/24")) != MakeKey("a.", 1, 1, false, false, ecs4) {
		t.Fatal("ECS prefixes must be masked")
	}
	for _, tc := range []KeyParts{
		{Name: "example.com.", Type: 1, Class: 1},
		{Name: "x.", Type: 65, Class: 1, DO: true, CD: true, ECS: ecs4},
		{Name: "x.", Type: 28, Class: 1, DO: true, ECS: ecs6},
		{Name: "x.", Type: 1, Class: 1, ECS: netip.MustParsePrefix("0.0.0.0/0")},
	} {
		k := MakeKey(tc.Name, tc.Type, tc.Class, tc.DO, tc.CD, tc.ECS)
		got, ok := k.Parts()
		if !ok || got != tc {
			t.Errorf("Parts(%q) = %+v, %v; want %+v", k, got, ok, tc)
		}
	}
	if _, ok := Key("abc").Parts(); ok {
		t.Fatal("short key must not decode")
	}
}

func TestGetLifecycle(t *testing.T) {
	clk := newClock()
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	c := New(Options{MaxEntries: 100, StaleTTL: time.Hour, Now: clk.Now, Metrics: m})
	k := key("a.example.")

	if _, ok := c.Get(k); ok {
		t.Fatal("empty cache hit")
	}
	c.Set(k, []byte("wire"), Positive, 60*time.Second)
	clk.Advance(10 * time.Second)
	l, ok := c.Get(k)
	if !ok || l.State != Fresh || string(l.Wire) != "wire" || l.Age != 10*time.Second || l.TTL != time.Minute || l.Kind != Positive {
		t.Fatalf("fresh lookup: %+v", l)
	}
	clk.Advance(60 * time.Second)
	l, ok = c.Get(k)
	if !ok || l.State != Stale || l.Hold {
		t.Fatalf("stale lookup: %+v", l)
	}
	c.CountStaleHit(l.Kind)
	if p, ok := c.Peek(k); !ok || p.State != Stale || string(p.Wire) != "wire" {
		t.Fatalf("peek: %+v", p)
	}
	c.MarkRefreshFailed(k, 30*time.Second)
	if l, _ = c.Get(k); !l.Hold {
		t.Fatal("hold not reported after a failed refresh")
	}
	clk.Advance(31 * time.Second)
	if l, _ = c.Get(k); l.Hold || l.State != Stale {
		t.Fatalf("hold should expire: %+v", l)
	}
	clk.Advance(time.Hour)
	if _, ok := c.Get(k); ok {
		t.Fatal("entry past the stale window must be gone")
	}
	if c.Len() != 0 {
		t.Fatal("expired entry not removed")
	}

	if got := testutil.ToFloat64(m.Hits.WithLabelValues("positive", "false")); got != 1 {
		t.Errorf("fresh hits = %v", got)
	}
	if got := testutil.ToFloat64(m.Hits.WithLabelValues("positive", "true")); got != 1 {
		t.Errorf("stale hits = %v", got)
	}
	if got := testutil.ToFloat64(m.Requests); got != 6 { // Peek is not a request
		t.Errorf("requests = %v", got)
	}
	if _, ok := c.Peek(k); ok {
		t.Error("peek past the stale window")
	}
	if got := testutil.ToFloat64(m.Evictions.WithLabelValues(ReasonExpired)); got != 1 {
		t.Errorf("expired evictions = %v", got)
	}
	if got := testutil.ToFloat64(m.Entries.WithLabelValues("positive")); got != 0 {
		t.Errorf("entries gauge = %v", got)
	}
}

func TestNoStaleWindow(t *testing.T) {
	clk := newClock()
	c := New(Options{MaxEntries: 10, Now: clk.Now})
	c.Set(key("a."), []byte("x"), Negative, 5*time.Second)
	clk.Advance(5 * time.Second)
	if _, ok := c.Get(key("a.")); ok {
		t.Fatal("without serve-stale an expired entry is a miss")
	}
	c.Set(key("b."), []byte("x"), Positive, 0)
	if c.Len() != 0 {
		t.Fatal("zero TTL must not be stored")
	}
}

func TestLRUEviction(t *testing.T) {
	clk := newClock()
	m := NewMetrics(nil)
	c := New(Options{MaxEntries: 3, Now: clk.Now, Metrics: m})
	if len(c.shards) != 1 {
		t.Fatalf("small caches use one shard, got %d", len(c.shards))
	}
	for _, n := range []string{"a.", "b.", "c."} {
		c.Set(key(n), []byte(n), Positive, time.Minute)
	}
	c.Get(key("a.")) // a is now most recent; b is the LRU victim
	c.Set(key("d."), []byte("d."), Negative, time.Minute)
	if _, ok := c.Get(key("b.")); ok {
		t.Fatal("LRU entry should have been evicted")
	}
	for _, n := range []string{"a.", "c.", "d."} {
		if _, ok := c.Get(key(n)); !ok {
			t.Fatalf("%s evicted", n)
		}
	}
	if got := testutil.ToFloat64(m.Evictions.WithLabelValues(ReasonCapacity)); got != 1 {
		t.Fatalf("capacity evictions = %v", got)
	}
	if p, n := testutil.ToFloat64(m.Entries.WithLabelValues("positive")), testutil.ToFloat64(m.Entries.WithLabelValues("negative")); p != 2 || n != 1 {
		t.Fatalf("entries gauge positive=%v negative=%v", p, n)
	}

	// Replacing keeps the count; Delete, Flush and Sweep keep the gauge true.
	c.Set(key("a."), []byte("a2"), Negative, time.Minute)
	if c.Len() != 3 || testutil.ToFloat64(m.Entries.WithLabelValues("negative")) != 2 {
		t.Fatal("replace must not grow the cache")
	}
	c.Delete(key("a."))
	c.Delete(key("missing."))
	if c.Len() != 2 || testutil.ToFloat64(m.Evictions.WithLabelValues(ReasonFlush)) != 1 {
		t.Fatal("delete")
	}
	c.Flush()
	if c.Len() != 0 || testutil.ToFloat64(m.Entries.WithLabelValues("positive"))+testutil.ToFloat64(m.Entries.WithLabelValues("negative")) != 0 {
		t.Fatal("flush")
	}

	// Large caches shard.
	if big := New(Options{MaxEntries: 200000}); len(big.shards) != 256 {
		t.Fatalf("200k entries: %d shards", len(big.shards))
	}
}

func TestSweep(t *testing.T) {
	clk := newClock()
	c := New(Options{MaxEntries: 1000, StaleTTL: time.Minute, Now: clk.Now})
	for i := range 10 {
		c.Set(key(fmt.Sprintf("n%d.", i)), []byte("x"), Positive, time.Duration(i+1)*time.Minute)
	}
	clk.Advance(4*time.Minute + time.Second) // entries with TTL <= 3m are past the stale window
	if n := c.Sweep(); n != 3 {
		t.Fatalf("swept %d", n)
	}
	if c.Len() != 7 {
		t.Fatalf("left %d", c.Len())
	}
}

func TestFlushZone(t *testing.T) {
	c := New(Options{MaxEntries: 1000})
	ecs := netip.MustParsePrefix("198.51.100.0/24")
	for _, k := range []Key{
		key("example.com."), key("WWW.Example.com."), MakeKey("a.b.example.com.", 28, 1, true, true, ecs),
		key("notexample.com."), key("example.org."), key("com."),
	} {
		c.Set(k, []byte("x"), Positive, time.Minute)
	}
	if n := c.FlushZone("Example.COM"); n != 3 {
		t.Fatalf("flushed %d, want 3", n)
	}
	for _, name := range []string{"notexample.com.", "example.org.", "com."} {
		if _, ok := c.Peek(key(name)); !ok {
			t.Fatalf("%s was flushed", name)
		}
	}
	if n := c.FlushZone("."); n != 3 || c.Len() != 0 {
		t.Fatalf("root flush: %d, left %d", n, c.Len())
	}
}

func TestPrefetchClaim(t *testing.T) {
	clk := newClock()
	c := New(Options{MaxEntries: 10, PrefetchThreshold: 3, PrefetchWindow: time.Hour, Now: clk.Now})
	k := key("hot.example.")
	c.Set(k, []byte("x"), Positive, 100*time.Second)

	for range 5 { // hot, but far from expiry
		if l, _ := c.Get(k); l.Prefetch {
			t.Fatal("prefetch claimed too early")
		}
	}
	clk.Advance(91 * time.Second) // 9 s left, under a tenth of the TTL
	l, _ := c.Get(k)
	if !l.Prefetch {
		t.Fatal("hot entry near expiry must be claimed")
	}
	if l, _ = c.Get(k); l.Prefetch {
		t.Fatal("a claim is exclusive")
	}
	c.ReleasePrefetch(k)
	if l, _ = c.Get(k); !l.Prefetch {
		t.Fatal("a released claim can be taken again")
	}
	// A refresh replaces the entry and keeps its hit history.
	c.Set(k, []byte("y"), Positive, 100*time.Second)
	clk.Advance(95 * time.Second)
	if l, _ = c.Get(k); !l.Prefetch {
		t.Fatal("refreshed hot entry should stay hot")
	}

	// Cold entries are never prefetched; hits outside the window reset.
	cold := key("cold.example.")
	c.Set(cold, []byte("x"), Positive, 100*time.Second)
	c.Get(cold)
	c.Get(cold)
	clk.Advance(2 * time.Hour)
	c.Set(cold, []byte("x"), Positive, 10*time.Second)
	clk.Advance(9 * time.Second)
	if l, _ = c.Get(cold); l.Prefetch {
		t.Fatal("hits outside the window must not count")
	}

	// Short TTLs use the 1 s floor.
	short := key("short.example.")
	c.Set(short, []byte("x"), Positive, 5*time.Second)
	c.Get(short)
	c.Get(short)
	clk.Advance(4500 * time.Millisecond)
	if l, _ = c.Get(short); !l.Prefetch {
		t.Fatal("short TTL: claim within the last second")
	}

	off := New(Options{MaxEntries: 10, Now: clk.Now})
	off.Set(k, []byte("x"), Positive, 10*time.Second)
	clk.Advance(9500 * time.Millisecond)
	for range 10 {
		if l, _ := off.Get(k); l.Prefetch {
			t.Fatal("threshold 0 disables prefetch")
		}
	}
}

func TestDumpLoad(t *testing.T) {
	clk := newClock()
	c := New(Options{MaxEntries: 1000, StaleTTL: time.Hour, Now: clk.Now})
	c.Set(key("short."), []byte("s"), Positive, time.Minute)
	clk.Advance(2 * time.Hour) // short. is now past its stale window
	for i := range 50 {
		c.Set(key(fmt.Sprintf("n%d.", i)), []byte(fmt.Sprintf("wire-%d", i)), Kind(i%2), time.Duration(i+1)*time.Minute)
	}
	c.Set(MakeKey("ecs.", 1, 1, true, true, netip.MustParsePrefix("2001:db8::/56")), []byte("ecs"), Positive, time.Hour)
	var buf bytes.Buffer
	if err := c.Dump(&buf); err != nil {
		t.Fatal(err)
	}

	clk.Advance(30 * time.Minute)
	d := New(Options{MaxEntries: 1000, StaleTTL: time.Hour, Now: clk.Now})
	n, err := d.Load(bytes.NewReader(buf.Bytes()))
	if err != nil || n != 51 {
		t.Fatalf("loaded %d: %v", n, err)
	}
	l, ok := d.Get(key("n49."))
	if !ok || l.State != Fresh || string(l.Wire) != "wire-49" || l.Age != 30*time.Minute || l.Kind != Negative {
		t.Fatalf("restored entry: %+v", l)
	}
	if l, _ = d.Get(key("n0.")); l.State != Stale || l.Kind != Positive {
		t.Fatalf("n0 should be stale after 30 min: %+v", l)
	}
	if _, ok := d.Get(MakeKey("ECS.", 1, 1, true, true, netip.MustParsePrefix("2001:db8::/56"))); !ok {
		t.Fatal("ECS key lost")
	}
	if _, ok := d.Get(key("short.")); ok {
		t.Fatal("dead entry must not be dumped")
	}

	// Loading after the stale window of everything loads nothing.
	clk.Advance(48 * time.Hour)
	if n, err := New(Options{MaxEntries: 10, Now: clk.Now}).Load(bytes.NewReader(buf.Bytes())); err != nil || n != 0 {
		t.Fatalf("expired dump: %d %v", n, err)
	}

	// Capacity applies: a smaller cache keeps the most recently used.
	clk2 := newClock()
	src := New(Options{MaxEntries: 100, Now: clk2.Now})
	for i := range 10 {
		src.Set(key(fmt.Sprintf("r%d.", i)), []byte("x"), Positive, time.Hour)
	}
	src.Get(key("r0."))
	buf.Reset()
	if err := src.Dump(&buf); err != nil {
		t.Fatal(err)
	}
	small := New(Options{MaxEntries: 2, Now: clk2.Now})
	if _, err := small.Load(&buf); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"r0.", "r9."} {
		if _, ok := small.Get(key(n)); !ok {
			t.Fatalf("%s should survive (most recent)", n)
		}
	}
}

func TestLoadRejectsGarbage(t *testing.T) {
	c := New(Options{MaxEntries: 10})
	var good bytes.Buffer
	c.Set(key("a."), []byte("wire"), Positive, time.Hour)
	if err := c.Dump(&good); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string][]byte{
		"empty":     nil,
		"magic":     []byte("NOPE\x01\x00"),
		"version":   []byte("KDRC\x09\x00"),
		"truncated": good.Bytes()[:good.Len()-3],
		"key size":  append([]byte("KDRC\x01"), 0xff, 0xff, 0x03),
	} {
		_, err := New(Options{MaxEntries: 10}).Load(bytes.NewReader(in))
		if !errors.Is(err, ErrBadDump) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := New(Options{MaxEntries: 10}).Load(bytes.NewReader(good.Bytes()[:good.Len()-3])); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncation should wrap io.ErrUnexpectedEOF: %v", err)
	}
}

func TestCoalesce(t *testing.T) {
	c := New(Options{MaxEntries: 10})
	var calls atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan any, 20)
	for range 20 {
		wg.Go(func() {
			r := <-c.Coalesce(key("a."), func() (any, error) {
				calls.Add(1)
				<-release
				return "answer", nil
			})
			results <- r.Val
		})
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	close(results)
	if calls.Load() != 1 {
		t.Fatalf("fn ran %d times", calls.Load())
	}
	for v := range results {
		if v != "answer" {
			t.Fatalf("got %v", v)
		}
	}
}

func TestConcurrentUse(t *testing.T) {
	clk := newClock()
	c := New(Options{MaxEntries: 5000, StaleTTL: time.Minute, PrefetchThreshold: 2, PrefetchWindow: time.Minute, Now: clk.Now, Metrics: NewMetrics(nil)})
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 2000 {
				k := key(fmt.Sprintf("n%d.", (i*7+g)%3000))
				if l, ok := c.Get(k); ok && l.Prefetch {
					c.ReleasePrefetch(k)
				}
				c.Set(k, []byte("x"), Kind(i%2), time.Duration(i%50+1)*time.Second)
				if i%500 == 0 {
					clk.Advance(time.Second)
					c.Sweep()
					_ = c.Dump(io.Discard)
				}
			}
		})
	}
	wg.Wait()
	if c.Len() > 5000 {
		t.Fatalf("over capacity: %d", c.Len())
	}
}
