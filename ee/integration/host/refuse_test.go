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

// A refused instance whose policy is Delete but which carries no cleanup
// finalizer still reports its refusal, instead of pausing as
// CleanupFinalizerMissing with nothing on its object: root never runs it, so
// it owns nothing to clean up (D384). An accepted instance in the same state
// stays paused.
func TestRefusedUnarmedDeleteInstanceReportsRefusal(t *testing.T) {
	st := memory.New()
	refused := webhook("sink")
	refused.Spec.DeletionPolicy = public.IntegrationDeletionDelete
	accepted := webhook("other")
	accepted.Spec.DeletionPolicy = public.IntegrationDeletionDelete
	in := captureInput(t, refused, accepted)
	r, err := ledger.New(ledger.Options{Reader: st, Write: func(ctx context.Context, _ ledger.Guard, o store.Object, a store.ApplyOptions) (store.Object, error) {
		return st.Apply(ctx, o, a)
	}})
	if err != nil {
		t.Fatal(err)
	}
	var factories atomic.Int32
	h, err := New(Options{Capture: func(context.Context) (Input, error) { return in, nil }, Factory: func(string) (integration.Integration, error) {
		factories.Add(1)
		return &fakePublisher{start: func(ctx context.Context) error { <-ctx.Done(); return nil }}, nil
	}, Elector: st.Elector, Fence: func(context.Context, ExecutionGuard) error { return nil }, Ledger: r, Reader: st, Allowed: func() bool { return true },
		Refuse: func(d *public.Integration) string {
			if d.Name == "sink" {
				return "UnsupportedOnKubernetes"
			}
			return ""
		},
		Purge:        func(context.Context, ExecutionGuard) (PurgeResult, error) { return PurgeResult{}, nil },
		Finalize:     func(context.Context, ExecutionGuard, CleanupSummary) error { return nil },
		Status:       func(context.Context, ExecutionGuard, InstanceStatus) error { return nil },
		PollInterval: time.Millisecond, OperationTimeout: time.Second, StopTimeout: time.Second, MaxConcurrent: 2})
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
	reasons := func() map[string]string {
		out := map[string]string{}
		for _, s := range h.Statuses() {
			out[s.Owner.Name] = s.Reason
		}
		return out
	}
	waitFor(t, func() bool {
		got := reasons()
		return got["sink"] == "UnsupportedOnKubernetes" && got["other"] == ReasonFinalizerMissing
	})
	if factories.Load() != 0 {
		t.Fatal("a refused or unarmed instance constructed a provider")
	}
}

// A refusal whose status write keeps failing (RBAC, an admission webhook,
// validation) is retried with backoff from one poll interval, never in a hot
// loop: every retry is a Lease term, a capture and a status write.
func TestRefusedInstanceStatusFailureBacksOff(t *testing.T) {
	st := memory.New()
	in := captureInput(t, webhook("sink"))
	r, err := ledger.New(ledger.Options{Reader: st, Write: func(ctx context.Context, _ ledger.Guard, o store.Object, a store.ApplyOptions) (store.Object, error) {
		return st.Apply(ctx, o, a)
	}})
	if err != nil {
		t.Fatal(err)
	}
	const poll = 20 * time.Millisecond
	var factories, attempts, captures atomic.Int32
	h, err := New(Options{Capture: func(context.Context) (Input, error) { captures.Add(1); return in, nil }, Factory: func(string) (integration.Integration, error) {
		factories.Add(1)
		return &fakePublisher{start: func(ctx context.Context) error { <-ctx.Done(); return nil }}, nil
	}, Elector: st.Elector, Fence: func(context.Context, ExecutionGuard) error { return nil }, Ledger: r, Reader: st, Allowed: func() bool { return true },
		Refuse: func(*public.Integration) string { return "UnsupportedOnKubernetes" },
		Status: func(context.Context, ExecutionGuard, InstanceStatus) error {
			attempts.Add(1)
			return errors.New("admission webhook denied the request")
		}, PollInterval: poll, OperationTimeout: time.Second, StopTimeout: time.Second, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	const window = 1500 * time.Millisecond
	time.Sleep(window)
	// Read the backoff state while the host runs: a refresh with a canceled
	// capture forgets the instance and its count.
	h.mu.RLock()
	failures := h.refusalFailures[instanceKey(InstanceID{Namespace: "dns", Name: "sink"})]
	h.mu.RUnlock()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Backoff 20, 40, 80, 160, 320, 640 ms: at most seven attempts fit in
	// the window (the first is immediate); without backoff there are
	// thousands. A ticker tick may arrive just before a retry is due, so
	// fewer is fine, but the refusal must be retried at all.
	got, captured := attempts.Load(), captures.Load()
	t.Logf("status attempts %d, captures %d in %v", got, captured, window)
	if got < 3 || got > 7 {
		t.Fatalf("refusal status attempts %d in %v, want 3 to 7 (backoff from one poll interval)", got, window)
	}
	// One capture per tick plus the replacement's wake per attempt.
	if limit := int32(window/poll) + 2*got + 2; captured > limit {
		t.Fatalf("captures %d in %v, want at most %d", captured, window, limit)
	}
	if factories.Load() != 0 {
		t.Fatal("a refused instance constructed a provider")
	}
	if failures < 3 || int64(failures) > int64(got) {
		t.Fatalf("consecutive failures %d, attempts %d", failures, got)
	}
}

func TestRefusalBackoffBounds(t *testing.T) {
	for _, tc := range []struct {
		poll     time.Duration
		failures uint32
		want     time.Duration
	}{
		{2 * time.Second, 1, 2 * time.Second},
		{2 * time.Second, 2, 4 * time.Second},
		{2 * time.Second, 8, 256 * time.Second},
		{2 * time.Second, 9, 5 * time.Minute},
		{2 * time.Second, 32, 5 * time.Minute},
		{10 * time.Minute, 3, 10 * time.Minute},
	} {
		if got := refusalBackoff(tc.poll, tc.failures); got != tc.want {
			t.Errorf("refusalBackoff(%v, %d) = %v, want %v", tc.poll, tc.failures, got, tc.want)
		}
	}
}

// A refusal that depends on the capture (D384: a reported source conflict)
// is asked after every capture: a running job whose instance becomes refused
// is replaced, its provider stopped, and the refusal published; once the
// refusal clears, a new job constructs a provider again.
func TestCaptureDependentRefusalReplacesJob(t *testing.T) {
	st := memory.New()
	in := captureInput(t, webhook("sink"))
	r, err := ledger.New(ledger.Options{Reader: st, Write: func(ctx context.Context, _ ledger.Guard, o store.Object, a store.ApplyOptions) (store.Object, error) {
		return st.Apply(ctx, o, a)
	}})
	if err != nil {
		t.Fatal(err)
	}
	var refused atomic.Bool
	var factories, running, published atomic.Int32
	h, err := New(Options{Capture: func(context.Context) (Input, error) { return in, nil }, Factory: func(string) (integration.Integration, error) {
		factories.Add(1)
		return &fakePublisher{start: func(ctx context.Context) error { running.Add(1); <-ctx.Done(); running.Add(-1); return nil }}, nil
	}, Elector: st.Elector, Fence: func(context.Context, ExecutionGuard) error { return nil }, Ledger: r, Reader: st, Allowed: func() bool { return true },
		Refuse: func(*public.Integration) string {
			if refused.Load() {
				return "SourceConflicted"
			}
			return ""
		},
		Status: func(_ context.Context, _ ExecutionGuard, s InstanceStatus) error {
			if s.Reason == "SourceConflicted" {
				published.Add(1)
			}
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
	waitFor(t, func() bool { return running.Load() == 1 })
	refused.Store(true)
	waitFor(t, func() bool { return running.Load() == 0 && published.Load() == 1 })
	if n := factories.Load(); n != 1 {
		t.Fatal("a refused job constructed a provider", n)
	}
	refused.Store(false)
	waitFor(t, func() bool { return running.Load() == 1 && factories.Load() == 2 })
	if published.Load() != 1 {
		t.Fatal("the refusal was published again", published.Load())
	}
}
