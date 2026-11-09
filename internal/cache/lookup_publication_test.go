package cache

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Pause the query after it captured addressing but before taking a shard lock.
// Publication must neither return a detached old map nor reuse the old clock after
// waiting for publication. The hook is the existing injectable clock.
func TestLookupAcrossPublicationUsesCurrentEntryAndTime(t *testing.T) {
	for _, resize := range []bool{false, true} {
		for _, expired := range []bool{false, true} {
			t.Run(fmt.Sprintf("resize=%t/expired=%t", resize, expired), func(t *testing.T) {
				clk := newClock()
				entered, release := make(chan struct{}), make(chan struct{})
				unblock := sync.OnceFunc(func() { close(release) })
				defer unblock()
				var armed atomic.Bool
				c := New(Options{MaxEntries: 64, Now: func() time.Time {
					now := clk.Now()
					if armed.CompareAndSwap(true, false) {
						close(entered)
						<-release
					}
					return now
				}})
				k := key("resized.example.")
				c.Set(k, []byte("old"), Positive, time.Hour)
				armed.Store(true)
				type result struct {
					lookup Lookup
					ok     bool
				}
				done := make(chan result, 1)
				go func() { l, ok := c.Get(k); done <- result{l, ok} }()
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("query did not reach captured addressing")
				}
				if resize {
					c.Resize(8192)
				} else {
					c.Flush()
				}
				c.Set(k, []byte("replacement"), Positive, time.Second)
				if expired {
					clk.Advance(2 * time.Second)
				}
				unblock()
				select {
				case got := <-done:
					if expired {
						if got.ok {
							t.Fatal("lookup used the clock from before publication")
						}
					} else if !got.ok || string(got.lookup.Wire) != "replacement" {
						t.Fatal("lookup used retired topology", got)
					}
				case <-time.After(time.Second):
					t.Fatal("lookup did not finish after resize")
				}
			})
		}
	}

}

func TestLookupCannotObservePartialFlush(t *testing.T) {
	c := New(Options{MaxEntries: 128})
	var keys [2]Key
	for i := 0; i < 10000 && (keys[0] == "" || keys[1] == ""); i++ {
		k := key(fmt.Sprintf("flush-%d.example.", i))
		for shard := range keys {
			if c.shard(k) == &c.shards[shard] {
				keys[shard] = k
			}
		}
	}
	if keys[0] == "" || keys[1] == "" {
		t.Fatal("could not populate both shards")
	}
	for _, k := range keys {
		c.Set(k, []byte("resident"), Positive, time.Hour)
	}
	// Hold the second shard so FlushCount must stop after clearing the first.
	c.shards[1].mu.Lock()
	held := true
	defer func() {
		if held {
			c.shards[1].mu.Unlock()
		}
	}()
	flushed := make(chan int, 1)
	go func() { flushed <- c.FlushCount() }()
	deadline := time.Now().Add(time.Second)
	for {
		c.shards[0].mu.Lock()
		empty := len(c.shards[0].m) == 0
		c.shards[0].mu.Unlock()
		if empty {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("flush did not reach held second shard")
		}
		time.Sleep(time.Millisecond)
	}
	done := make(chan bool, 1)
	started := make(chan struct{})
	go func() { close(started); _, ok := c.Get(keys[0]); done <- ok }()
	<-started
	select {
	case <-done:
		t.Fatal("query observed a partially flushed cache")
	case <-time.After(20 * time.Millisecond):
	}
	c.shards[1].mu.Unlock()
	held = false
	select {
	case n := <-flushed:
		if n != 2 {
			t.Fatal("flush count", n)
		}
	case <-time.After(time.Second):
		t.Fatal("flush did not finish")
	}
	select {
	case ok := <-done:
		if ok {
			t.Fatal("query retained flushed entry")
		}
	case <-time.After(time.Second):
		t.Fatal("query did not resume")
	}
}
