package cache

import (
	"fmt"
	"net/netip"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// benchWire is the size of a typical cached answer: one A record behind a
// CNAME, compressed, without OPT (~90 bytes on the wire).
const benchWire = 90

func benchKeys(n int) []Key {
	keys := make([]Key, n)
	for i := range keys {
		keys[i] = MakeKey("host-"+strconv.Itoa(i)+".example.com.", 1, 1, false, false, netip.Prefix{})
	}
	return keys
}

func filled(b *testing.B, n int) (*Cache, []Key) {
	b.Helper()
	c := New(Options{MaxEntries: n, StaleTTL: 72 * time.Hour, PrefetchThreshold: 5, PrefetchWindow: 2 * time.Hour, Metrics: NewMetrics(nil)})
	keys := benchKeys(n)
	for _, k := range keys {
		c.Set(k, make([]byte, benchWire), Positive, time.Hour)
	}
	// Shards fill unevenly; keep the keys that stayed resident.
	resident := keys[:0]
	for _, k := range keys {
		if _, ok := c.Peek(k); ok {
			resident = append(resident, k)
		}
	}
	return c, resident
}

// BenchmarkGet measures one lookup in a 200 k-entry cache (the default
// size): hit, miss, and hits from every CPU at once.
func BenchmarkGet(b *testing.B) {
	c, keys := filled(b, 200_000)
	missing := benchKeys(1024)
	for i := range missing {
		missing[i] += "x"
	}
	b.Run("hit", func(b *testing.B) {
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			if _, ok := c.Get(keys[i%len(keys)]); !ok {
				b.Fatal("miss")
			}
			i += 7919
		}
	})
	b.Run("miss", func(b *testing.B) {
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			if _, ok := c.Get(missing[i%len(missing)]); ok {
				b.Fatal("hit")
			}
			i++
		}
	})
	b.Run("hit-parallel", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				c.Get(keys[i%len(keys)])
				i += 7919
			}
		})
	})
}

// BenchmarkMakeKey measures building a key from the question.
func BenchmarkMakeKey(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = MakeKey("www.Example.COM.", 1, 1, true, false, netip.Prefix{})
	}
}

// BenchmarkSet measures inserting into a full cache (every insert evicts).
func BenchmarkSet(b *testing.B) {
	c, _ := filled(b, 200_000)
	keys := benchKeys(1 << 16)
	for i := range keys {
		keys[i] += "n"
	}
	wire := make([]byte, benchWire)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Set(keys[i%len(keys)], wire, Positive, time.Hour)
		i++
	}
}

// BenchmarkMemory reports the heap held per entry for a cache of 1 M
// entries with 90-byte messages and ~30-byte keys: bytes-per-entry is the
// total, overhead-per-entry excludes the key and message bytes.
func BenchmarkMemory(b *testing.B) {
	for _, n := range []int{1_000_000} {
		b.Run(fmt.Sprintf("%dk", n/1000), func(b *testing.B) {
			var perEntry, overhead float64
			for b.Loop() {
				keys := benchKeys(n)
				payload := 0
				for _, k := range keys {
					payload += len(k) + benchWire
				}
				before := heap()
				// Headroom so that uneven shards still hold all n entries.
				c := New(Options{MaxEntries: n + n/8, StaleTTL: 72 * time.Hour})
				for _, k := range keys {
					c.Set(k, make([]byte, benchWire), Positive, time.Hour)
				}
				after := heap()
				if c.Len() != n {
					b.Fatalf("len %d", c.Len())
				}
				// Keys are shared between keys and the cache map; count them
				// as the cache's.
				perEntry = float64(after-before) / float64(n)
				keyBytes := 0
				for _, k := range keys {
					keyBytes += len(k)
				}
				perEntry += float64(keyBytes) / float64(n)
				overhead = perEntry - float64(payload)/float64(n)
				runtime.KeepAlive(c)
				runtime.KeepAlive(keys)
			}
			b.ReportMetric(perEntry, "bytes/entry")
			b.ReportMetric(overhead, "overhead-bytes/entry")
		})
	}
}

func heap() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}
