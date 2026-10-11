package memory

import (
	"context"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/conformance"
)

func TestConformance(t *testing.T) {
	conformance.Run(t, func(t *testing.T) store.Store {
		s := New()
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

// TestWatchManyInitialObjects: more initial objects than the live-event
// buffer must not block Watch (it queues them before returning).
func TestWatchManyInitialObjects(t *testing.T) {
	s := New()
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	const n = 600
	for i := range n {
		z := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("z%d", i)}, Spec: v1alpha1.DNSZoneSpec{ZoneName: fmt.Sprintf("z%d.example.", i), Type: v1alpha1.ZoneTypePrimary}}
		if _, err := s.Apply(ctx, z, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan store.Watcher, 1)
	go func() {
		w, err := s.Watch(ctx, "DNSZone", store.WatchOptions{})
		if err != nil {
			t.Error(err)
		}
		done <- w
	}()
	select {
	case w := <-done:
		defer w.Stop()
		got := 0
		for ev := range w.ResultChan() {
			if ev.Type == store.Bookmark {
				break
			}
			got++
		}
		if got != n {
			t.Fatalf("initial events %d, want %d", got, n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Watch blocked with more initial objects than its buffer")
	}
}
