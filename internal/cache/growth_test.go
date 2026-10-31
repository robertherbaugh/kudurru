package cache

import (
	"bytes"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func TestGrowthPreservesEntryStateAndInvalidation(t *testing.T) {
	clk := newClock()
	metrics := NewMetrics(nil)
	c := New(Options{MaxEntries: 64, StaleTTL: time.Hour, PrefetchThreshold: 1, PrefetchWindow: time.Minute, Now: clk.Now, Metrics: metrics})
	scope := pipeline.CacheScope{ID: [32]byte{7}, Epoch: 42}
	k := MakeScopedKey(scope, "held.example.", 1, 1, true, false, netip.Prefix{})
	metadata := Metadata{Scope: scope, Upstream: "default/verified", Security: pipeline.Security{State: pipeline.ValidationSecure, ValidatedAt: clk.Now(), ValidUntil: clk.Now().Add(20 * time.Second)}}
	c.SetWithMetadata(k, []byte("wire"), Positive, 10*time.Second, metadata)
	clk.Advance(9500 * time.Millisecond)
	if l, ok := c.Get(k); !ok || !l.Prefetch {
		t.Fatal("prefetch not claimed")
	}
	original := c.shard(k).m[k]
	generation := c.Generation()
	c.Resize(1_000_000)
	if len(c.shards) != 256 || c.Generation() != generation {
		t.Fatal("growth retained small topology or changed epoch")
	}
	if c.shard(k).m[k] != original {
		t.Fatal("growth copied entry/payload")
	}
	if l, ok := c.Get(k); !ok || l.Metadata != metadata || l.Prefetch || l.Age != 9500*time.Millisecond || l.TTL != 10*time.Second {
		t.Fatal("growth changed identity, TTL or prefetch claim", l)
	}
	clk.Advance(time.Second)
	c.MarkRefreshFailed(k, 15*time.Second)
	c.Resize(128)
	if l, ok := c.Get(k); !ok || l.State != Stale || !l.Hold {
		t.Fatal("resize lost stale refresh hold")
	}
	// Resize is capacity-only, including future insertion policy.
	c.Set(key("future.example."), []byte{2}, Negative, time.Second)
	clk.Advance(2 * time.Second)
	if l, ok := c.Get(key("future.example.")); !ok || l.State != Stale {
		t.Fatal("resize changed future stale policy")
	}
	clk.Advance(8 * time.Second)
	if _, ok := c.Get(k); ok {
		t.Fatal("growth renewed security deadline")
	}
	if got := testutil.ToFloat64(metrics.Entries.WithLabelValues("negative")); got != 1 {
		t.Fatal("resize changed entry accounting", got)
	}
	c.Flush()
	c.Resize(200_000)
	c.SetIfGeneration(k, []byte("late"), Positive, time.Hour, metadata, generation)
	if c.Len() != 0 {
		t.Fatal("growth revived a pre-flush fill")
	}
}

func TestGrowthConcurrentOperationsAndExactBounds(t *testing.T) {
	c := New(Options{MaxEntries: 64, StaleTTL: time.Hour})
	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Go(func() {
			for i := range 250 {
				k := key(fmt.Sprintf("w%d-%d.example.", worker, i%100))
				c.Set(k, []byte{1}, Positive, time.Minute)
				c.Get(k)
				c.Peek(k)
				c.ReleasePrefetch(k)
				c.MarkRefreshFailed(k, time.Second)
				if i%50 == 0 {
					var dump bytes.Buffer
					if err := c.Dump(&dump); err != nil {
						t.Error(err)
					}
					if _, err := c.Load(&dump); err != nil {
						t.Error(err)
					}
					c.Sweep()
					c.Len()
					c.RebindRestored(pipeline.CacheScope{Epoch: uint64(worker + 1)}) //nolint:gosec // small nonnegative worker
					c.FlushName("unused.example.")
					c.FlushZone("unused.example.")
					c.Delete(k)
				}
			}
		})
	}
	for i := range 50 {
		c.Resize([]int{1, 127, 128, 1001, 200_003}[i%5])
	}
	wg.Wait()
	for _, bound := range []int{200_003, 1001, 128, 127, 1} {
		c.Resize(bound)
		for i := range 2000 {
			c.Set(key(fmt.Sprintf("fill-%d.example.", i)), []byte{1}, Positive, time.Minute)
		}
		if c.Len() > bound {
			t.Fatalf("capacity %d exceeded: %d", bound, c.Len())
		}
		capacity := 0
		for i := range c.shards {
			capacity += c.shards[i].capacity
		}
		if capacity != bound {
			t.Fatalf("capacity allocation: %d, want %d", capacity, bound)
		}
	}
}

func TestGrowthMovesResidentEntriesWithoutCopyingOrRefreshing(t *testing.T) {
	c := New(Options{MaxEntries: 8192, StaleTTL: time.Hour})
	for i := range 6000 {
		c.Set(key(fmt.Sprintf("resident-%d.example.", i)), []byte{byte(i % 256)}, Positive, time.Minute)
	} //nolint:gosec // i%256 fits a byte
	original := make(map[Key]*entry)
	for i := range c.shards {
		for k, e := range c.shards[i].m {
			original[k] = e
		}
	}
	c.Reconfigure(Options{MaxEntries: 1_000_000, StaleTTL: time.Second})
	if len(c.shards) != 256 || c.Len() != len(original) {
		t.Fatal("large growth lost residents")
	}
	for k, before := range original {
		if after := c.shard(k).m[k]; after != before {
			t.Fatal("growth copied or dropped resident")
		}
	}
	// Tightening a bound with unchanged topology must rebuild oversized maps
	// while keeping accounting exact and linked lists consistent.
	c.Resize(20_000)
	c.Resize(17_000)
	c.Resize(1)
	if c.Len() != 1 {
		t.Fatal("large shrink did not retain exactly one resident")
	}
	var dump bytes.Buffer
	if err := c.Dump(&dump); err != nil {
		t.Fatal(err)
	}
	d := New(Options{MaxEntries: 1})
	if n, err := d.Load(&dump); err != nil || n != 1 || d.Len() != 1 {
		t.Fatal("resharded LRU cannot round trip", n, err)
	}
}
