package conformance

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// SlowWatcherWrites is how many objects the slow-watcher case writes while
// its watcher is not read. It exceeds every driver's in-process buffering
// (memory: 256 live events; sqlite: 1024 queued plus 256 in the result
// channel) so the overflow path runs.
var SlowWatcherWrites = 1500

// testSlowWatcher is the slow-watcher contract: a consumer that stops
// reading while writes continue must never lose an event silently. The
// driver either keeps the stream complete (sqlite catches up from its
// change feed, kube informers buffer without bound) or closes the result
// channel, after which the consumer watches again and the new watch lists
// every object before its Bookmark (memory).
func testSlowWatcher(t *testing.T, s store.Store) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := s.Apply(ctx, zone("seed", "seed."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	w, err := s.Watch(ctx, "DNSZone", store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	want := map[string]bool{"seed": true}
	for i := range SlowWatcherWrites {
		want[fmt.Sprintf("z%04d", i)] = true
	}
	// Write without reading, from a few workers (the kube driver's
	// apiserver round trips dominate otherwise).
	var next atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			for {
				i := int(next.Add(1) - 1)
				if i >= SlowWatcherWrites {
					return
				}
				name := fmt.Sprintf("z%04d", i)
				if _, err := s.Apply(ctx, zone(name, name+"."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	closed := false
	for !closed && len(seen) < len(want) {
		select {
		case ev, ok := <-w.ResultChan():
			if !ok {
				closed = true
				break
			}
			if ev.Type == store.Added || ev.Type == store.Modified {
				seen[ev.Object.GetName()] = true
			}
		case <-ctx.Done():
			t.Fatalf("slow watcher neither caught up nor was closed: %d of %d objects seen", len(seen), len(want))
		}
	}
	if !closed {
		t.Logf("slow watcher caught up: %d objects", len(seen))
		return
	}

	// Closed: the consumer re-lists through a new watch.
	t.Logf("slow watcher closed after %d of %d objects; re-listing", len(seen), len(want))
	w2, err := s.Watch(ctx, "DNSZone", store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Stop()
	listed := map[string]bool{}
	for {
		select {
		case ev, ok := <-w2.ResultChan():
			if !ok {
				t.Fatalf("re-list watch closed after %d objects", len(listed))
			}
			if ev.Type == store.Bookmark {
				for name := range want {
					if !listed[name] {
						t.Fatalf("re-list is missing %s (%d of %d listed)", name, len(listed), len(want))
					}
				}
				return
			}
			listed[ev.Object.GetName()] = true
		case <-ctx.Done():
			t.Fatalf("re-list did not finish: %d of %d objects", len(listed), len(want))
		}
	}
}
