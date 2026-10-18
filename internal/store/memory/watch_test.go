package memory

import (
	"context"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func applyRR(t *testing.T, s *Store, ns, name string) {
	t.Helper()
	rs := &v1alpha1.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: "home"}}}
	if _, err := s.Apply(context.Background(), rs, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
}

// next reads one event or fails after a timeout; ok is false on close.
func next(t *testing.T, w store.Watcher) (store.Event, bool) {
	t.Helper()
	select {
	case ev, ok := <-w.ResultChan():
		return ev, ok
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5s")
		return store.Event{}, false
	}
}

// TestWatchOverflowCloses: a watcher that lets more than WatchBuffer live
// events queue is closed (never silently dropped), writers never block,
// and other watchers are unaffected.
func TestWatchOverflowCloses(t *testing.T) {
	s := New()
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	slow, err := s.Watch(ctx, "DNSRecordSet", store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Stop()
	fast, err := s.Watch(ctx, "DNSRecordSet", store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Stop()
	if ev, _ := next(t, fast); ev.Type != store.Bookmark {
		t.Fatalf("want Bookmark first on an empty kind, got %s", ev.Type)
	}
	got := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range fast.ResultChan() {
			if ev.Type == store.Added {
				got++
				if got == WatchBuffer+10 {
					return
				}
			}
		}
	}()
	for i := range WatchBuffer + 10 {
		applyRR(t, s, "apps", fmt.Sprintf("r%d", i))
	}
	<-done
	if got != WatchBuffer+10 {
		t.Fatalf("reading watcher got %d events, want %d", got, WatchBuffer+10)
	}
	// The slow watcher closes: at most its initial Bookmark and buffered
	// events, then the end of the channel.
	n := 0
	for {
		_, ok := next(t, slow)
		if !ok {
			break
		}
		n++
	}
	if n > WatchBuffer+1 {
		t.Fatalf("slow watcher delivered %d events before closing", n)
	}
	s.mu.RLock()
	left := len(s.watchers["DNSRecordSet"])
	s.mu.RUnlock()
	if left != 1 {
		t.Fatalf("%d watchers registered after the overflow, want 1", left)
	}
}

func TestWatchNamespaceFilterAndStop(t *testing.T) {
	s := New()
	defer func() { _ = s.Close() }()
	applyRR(t, s, "apps", "a")
	applyRR(t, s, "dns", "b")
	ctx, cancel := context.WithCancel(context.Background())
	w, err := s.Watch(ctx, "DNSRecordSet", store.WatchOptions{Namespace: "apps"})
	if err != nil {
		t.Fatal(err)
	}
	if ev, _ := next(t, w); ev.Type != store.Added || ev.Object.GetName() != "a" {
		t.Fatalf("initial: %s %v", ev.Type, ev.Object)
	}
	if ev, _ := next(t, w); ev.Type != store.Bookmark {
		t.Fatalf("want Bookmark, got %s", ev.Type)
	}
	applyRR(t, s, "dns", "c")
	applyRR(t, s, "apps", "d")
	if ev, _ := next(t, w); ev.Object.GetName() != "d" {
		t.Fatalf("live event from another namespace leaked: %s", ev.Object.GetName())
	}
	cancel() // the context ends the watch
	if _, ok := next(t, w); ok {
		t.Fatal("channel must close when ctx ends")
	}
	w.Stop() // idempotent after close
	w.Stop()
}

func TestCloseEndsWatches(t *testing.T) {
	s := New()
	w, err := s.Watch(context.Background(), "DNSZone", store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for {
		if _, ok := next(t, w); !ok {
			break
		}
	}
	w.Stop()
	if _, err := s.Watch(context.Background(), "DNSZone", store.WatchOptions{}); err == nil {
		t.Fatal("Watch after Close must fail")
	}
}
