package zonestate

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/zone"
)

func TestMaterializerWaiterAdmissionAndCancellation(t *testing.T) {
	m, err := NewMaterializer(MaterializerOptions{MaxWaiters: 1, Build: func(context.Context, *View) (*zone.Zone, error) { return nil, ErrInvalid }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.WaitDesired(ctx, DesiredTarget{}); done <- err }()
	deadline := time.Now().Add(time.Second)
	for {
		m.mu.RLock()
		admitted := m.waiters == 1
		m.mu.RUnlock()
		if admitted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiter not admitted")
		}
		runtime.Gosched()
	}
	if _, err := m.WaitDesired(context.Background(), DesiredTarget{}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("capacity %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter cancellation leaked")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.waiters != 0 {
		t.Fatal("admission was not released")
	}
}
