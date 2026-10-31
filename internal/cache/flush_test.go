package cache

import (
	"net/netip"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func TestFlushNameAllResolutionKeysAndExactBoundary(t *testing.T) {
	clock := newClock()
	metrics := NewMetrics(nil)
	c := New(Options{MaxEntries: 4096, StaleTTL: time.Hour, Now: clock.Now, Metrics: metrics})
	var removed []Key
	for _, scope := range []pipeline.CacheScope{{}, {ID: [32]byte{1}, Epoch: 1}, {ID: [32]byte{2}, Epoch: 2}} {
		for _, ecs := range []netip.Prefix{{}, netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("2001:db8::/48")} {
			for _, qtype := range []uint16{1, 65400} {
				for _, class := range []uint16{1, 3} {
					for _, do := range []bool{false, true} {
						for _, cd := range []bool{false, true} {
							key := MakeScopedKey(scope, "Target.EXAMPLE.", qtype, class, do, cd, ecs)
							if scope == (pipeline.CacheScope{}) {
								key = MakeKey("target.example.", qtype, class, do, cd, ecs)
							}
							kind := Positive
							if cd {
								kind = Negative
							}
							c.Set(key, []byte("wire"), kind, time.Minute)
							removed = append(removed, key)
						}
					}
				}
			}
		}
	}
	for _, name := range []string{".", "child.target.example.", "nottarget.example.", "example."} {
		c.Set(key(name), []byte("other"), Positive, time.Minute)
	}
	c.Set(Key("bad"), []byte("historical"), Negative, time.Minute)
	clock.Advance(2 * time.Minute)
	if count := c.FlushName("TARGET.Example"); count != len(removed) || c.Len() != 5 {
		t.Fatal("exact-name removal missed scoped/stale entries or crossed boundary", count, c.Len())
	}
	for _, key := range removed {
		if _, present := c.Peek(key); present {
			t.Fatal("matching key survived exact-name flush")
		}
	}
	if count := testutil.ToFloat64(metrics.Evictions.WithLabelValues(ReasonFlush)); count != float64(len(removed)) {
		t.Fatal("flush removal metrics differ", count)
	}
	if count := c.FlushName("."); count != 1 || c.Len() != 4 {
		t.Fatal("explicit root acted as a full flush", count, c.Len())
	}
	if count := c.FlushCount(); count != 4 || c.Len() != 0 {
		t.Fatal("full flush missed malformed/historical key", count)
	}
	if testutil.ToFloat64(metrics.Entries.WithLabelValues("positive"))+testutil.ToFloat64(metrics.Entries.WithLabelValues("negative")) != 0 {
		t.Fatal("entry metrics retained removed entries")
	}
}

func TestFlushCountAndNameFenceDetachedFillsEvenWhenEmpty(t *testing.T) {
	for _, mode := range []string{"full", "name", "empty-full", "absent-name"} {
		t.Run(mode, func(t *testing.T) {
			c := New(Options{MaxEntries: 100})
			if mode == "full" || mode == "name" {
				c.Set(key("target.example."), []byte("old"), Positive, time.Minute)
			}
			c.Set(key("unrelated.example."), []byte("keep"), Positive, time.Minute)
			if mode == "empty-full" {
				c.Flush()
			}
			generation := c.Generation()
			resume, done := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(done)
				<-resume
				c.SetIfGeneration(key("target.example."), []byte("late"), Positive, time.Minute, Metadata{}, generation)
			}()
			if mode == "full" || mode == "empty-full" {
				c.FlushCount()
			} else {
				c.FlushName("target.example.")
			}
			close(resume)
			<-done
			if _, present := c.Peek(key("target.example.")); present || c.Generation() != generation+1 {
				t.Fatal("pre-invalidation fill repopulated cache")
			}
			if mode == "name" || mode == "absent-name" {
				if _, present := c.Peek(key("unrelated.example.")); !present {
					t.Fatal("exact-name flush removed unrelated entry")
				}
			}
			c.Set(key("target.example."), []byte("fresh"), Positive, time.Minute)
			if _, present := c.Peek(key("target.example.")); !present {
				t.Fatal("new fills remained fenced after invalidation")
			}
		})
	}
}

func TestFlushNameFoldsOnlyASCIILabelBytes(t *testing.T) {
	c := New(Options{MaxEntries: 10})
	for _, name := range []string{"É.example.", "é.example.", "E.example."} {
		c.Set(key(name), []byte("wire"), Positive, time.Minute)
	}
	if removed := c.FlushName("É.EXAMPLE"); removed != 1 || c.Len() != 2 {
		t.Fatal("non-ASCII DNS label bytes were case folded", removed, c.Len())
	}
	if _, present := c.Peek(key("É.example.")); present {
		t.Fatal("exact non-ASCII label survived flush")
	}
}
