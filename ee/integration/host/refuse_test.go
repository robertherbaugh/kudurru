package host

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
)

// A root refusal (Options.Refuse, D384) stops the elected job before any
// provider is constructed, reports the reason and publishes it once through
// the fenced Status callback; an instance root accepts still runs.
func TestRefusedInstanceReportsWithoutProvider(t *testing.T) {
	st := memory.New()
	in := captureInput(t, webhook("sink"))
	r, err := ledger.New(ledger.Options{Reader: st, Write: func(ctx context.Context, _ ledger.Guard, o store.Object, a store.ApplyOptions) (store.Object, error) {
		return st.Apply(ctx, o, a)
	}})
	if err != nil {
		t.Fatal(err)
	}
	var factories, fences atomic.Int32
	var mu sync.Mutex
	var statuses []InstanceStatus
	h, err := New(Options{Capture: func(context.Context) (Input, error) { return in, nil }, Factory: func(string) (integration.Integration, error) {
		factories.Add(1)
		return &fakePublisher{start: func(ctx context.Context) error { <-ctx.Done(); return nil }}, nil
	}, Elector: st.Elector, Fence: func(context.Context, ExecutionGuard) error { fences.Add(1); return nil }, Ledger: r, Reader: st, Allowed: func() bool { return true },
		Refuse: func(d *public.Integration) string {
			if d.Spec.Type == "webhook" {
				return "UnsupportedOnKubernetes"
			}
			return ""
		},
		Status: func(_ context.Context, _ ExecutionGuard, s InstanceStatus) error {
			mu.Lock()
			statuses = append(statuses, s)
			mu.Unlock()
			return nil
		}, PollInterval: time.Millisecond, OperationTimeout: time.Second, StopTimeout: time.Second, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(statuses) == 1 })
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	got := statuses
	mu.Unlock()
	if len(got) != 1 || got[0].Phase != "Degraded" || got[0].Reason != "UnsupportedOnKubernetes" || !got[0].Leader {
		t.Fatalf("refusal status %+v", got)
	}
	if factories.Load() != 0 {
		t.Fatal("a refused instance constructed a provider")
	}
	if fences.Load() == 0 {
		t.Fatal("the refusal status was published without the fence")
	}
	local := h.Statuses()
	if len(local) != 1 || local[0].Reason != "UnsupportedOnKubernetes" {
		t.Fatal("local status lost the refusal", local)
	}
}

// A refusal whose one status write fails is not left silently inert: the
// next capture replaces the job and a new term publishes the reason again,
// still without constructing a provider.
func TestRefusedInstanceRepublishesAfterFailedStatus(t *testing.T) {
	st := memory.New()
	in := captureInput(t, webhook("sink"))
	r, err := ledger.New(ledger.Options{Reader: st, Write: func(ctx context.Context, _ ledger.Guard, o store.Object, a store.ApplyOptions) (store.Object, error) {
		return st.Apply(ctx, o, a)
	}})
	if err != nil {
		t.Fatal(err)
	}
	var factories, attempts, published atomic.Int32
	h, err := New(Options{Capture: func(context.Context) (Input, error) { return in, nil }, Factory: func(string) (integration.Integration, error) {
		factories.Add(1)
		return &fakePublisher{start: func(ctx context.Context) error { <-ctx.Done(); return nil }}, nil
	}, Elector: st.Elector, Fence: func(context.Context, ExecutionGuard) error { return nil }, Ledger: r, Reader: st, Allowed: func() bool { return true },
		Refuse: func(*public.Integration) string { return "UnsupportedOnKubernetes" },
		Status: func(_ context.Context, _ ExecutionGuard, s InstanceStatus) error {
			if s.Reason != "UnsupportedOnKubernetes" {
				t.Error("unexpected status", s.Reason)
			}
			if attempts.Add(1) == 1 {
				return errors.New("conflict")
			}
			published.Add(1)
			return nil
		}, PollInterval: 5 * time.Millisecond, OperationTimeout: time.Second, StopTimeout: time.Second, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	waitFor(t, func() bool { return published.Load() == 1 })
	// Once published, the refusal is not written again.
	time.Sleep(100 * time.Millisecond)
	if attempts.Load() != 2 || published.Load() != 1 {
		t.Fatal("refusal status attempts", attempts.Load(), "published", published.Load())
	}
	if factories.Load() != 0 {
		t.Fatal("a refused instance constructed a provider")
	}
}
