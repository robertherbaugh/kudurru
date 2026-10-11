package cache

import (
	"fmt"
	"testing"
	"time"
)

func TestReconfigureResizesAndKeepsEntries(t *testing.T) {
	clk := newClock()
	c := New(Options{MaxEntries: 1024, StaleTTL: time.Hour, Now: clk.Now})
	for i := range 1000 {
		c.Set(key(fmt.Sprintf("n%d.example.", i)), []byte{1}, Positive, time.Minute)
	}
	if n := c.Len(); n < 900 {
		t.Fatalf("filled %d", n)
	}
	// Growing keeps everything.
	c.Reconfigure(Options{MaxEntries: 4096, StaleTTL: time.Hour})
	before := c.Len()
	for i := 1000; i < 3000; i++ {
		c.Set(key(fmt.Sprintf("n%d.example.", i)), []byte{1}, Positive, time.Minute)
	}
	if c.Len() <= before || c.Len() > 4096 {
		t.Fatalf("after growing: %d entries (was %d)", c.Len(), before)
	}
	// Shrinking evicts at once and the bound holds afterwards.
	c.Reconfigure(Options{MaxEntries: 100, StaleTTL: time.Hour})
	if n := c.Len(); n > 100+len(c.shards) {
		t.Fatalf("after shrinking: %d entries", n)
	}
	for i := 3000; i < 3500; i++ {
		c.Set(key(fmt.Sprintf("n%d.example.", i)), []byte{1}, Positive, time.Minute)
	}
	if n := c.Len(); n > 100+len(c.shards) {
		t.Fatalf("bound after shrinking: %d entries", n)
	}
	// MaxEntries 0 keeps the size; the stale window applies to new entries.
	c.Reconfigure(Options{MaxEntries: 0, StaleTTL: 0})
	c.Set(key("fresh.example."), []byte{1}, Positive, time.Minute)
	clk.Advance(2 * time.Minute)
	if _, ok := c.Get(key("fresh.example.")); ok {
		t.Fatal("an entry stored after StaleTTL 0 must expire without a stale window")
	}
}

func TestReconfigurePrefetch(t *testing.T) {
	clk := newClock()
	c := New(Options{MaxEntries: 100, Now: clk.Now})
	c.Set(key("hot.example."), []byte{1}, Positive, 10*time.Second)
	clk.Advance(9500 * time.Millisecond)
	if l, _ := c.Get(key("hot.example.")); l.Prefetch {
		t.Fatal("prefetch is off")
	}
	c.Reconfigure(Options{PrefetchThreshold: 1, PrefetchWindow: time.Hour})
	if l, _ := c.Get(key("hot.example.")); !l.Prefetch {
		t.Fatal("prefetch must follow the reconfigured threshold")
	}
}
