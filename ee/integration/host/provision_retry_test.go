package host

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
)

// A provider whose setup fails (here its Check, as when it is briefly
// unreachable) is set up again by a new term once a backoff has passed,
// with no change of its object; once setup succeeds it runs and the failure
// count is forgotten (D384). Before, the instance stayed ProvisionFailed
// until its object, credential or license changed.
func TestFailedProvisionIsRetriedWithBackoff(t *testing.T) {
	st := memory.New()
	in := captureInput(t, webhook("sink"))
	r, err := ledger.New(ledger.Options{Reader: st, Write: func(ctx context.Context, _ ledger.Guard, o store.Object, a store.ApplyOptions) (store.Object, error) {
		return st.Apply(ctx, o, a)
	}})
	if err != nil {
		t.Fatal(err)
	}
	const poll = 20 * time.Millisecond
	var checks, healthyAfter atomic.Int32
	healthyAfter.Store(1 << 30)
	h, err := New(Options{Capture: func(context.Context) (Input, error) { return in, nil }, Factory: func(string) (integration.Integration, error) {
		return &fakePublisher{check: func(context.Context) (integration.Status, error) {
			n := checks.Add(1)
			return integration.Status{Reachable: n >= healthyAfter.Load(), Authenticated: true}, nil
		}, start: func(ctx context.Context) error { <-ctx.Done(); return nil }}, nil
	}, Elector: st.Elector, Fence: func(context.Context, ExecutionGuard) error { return nil }, Ledger: r, Reader: st, Allowed: func() bool { return true },
		PollInterval: poll, OperationTimeout: time.Second, StopTimeout: time.Second, MaxConcurrent: 1})
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
	key := instanceKey(InstanceID{Namespace: "dns", Name: "sink"})
	status := func() (string, string) {
		for _, s := range h.Statuses() {
			if instanceKey(s.Owner) == key {
				return s.Phase, s.Reason
			}
		}
		return "", ""
	}
	const window = 1500 * time.Millisecond
	time.Sleep(window)
	// Backoff 20, 40, 80, 160, 320, 640 ms: three to seven setups fit in
	// the window; without the retry there is one.
	got := checks.Load()
	h.mu.RLock()
	failures := h.provisionFailures[key]
	h.mu.RUnlock()
	t.Logf("setups %d, consecutive failures %d in %v", got, failures, window)
	if got < 3 || got > 7 || failures < 3 || int64(failures) > int64(got) {
		t.Fatalf("setups %d with %d consecutive failures in %v, want 3 to 7 (backoff from one poll interval)", got, failures, window)
	}
	// The provider becomes reachable: the next setup runs it.
	healthyAfter.Store(got + 1)
	waitFor(t, func() bool { phase, _ := status(); return phase == "Running" || phase == "Ready" })
	h.mu.RLock()
	_, counted := h.provisionFailures[key]
	h.mu.RUnlock()
	if counted {
		t.Fatal("a successful setup kept its failure count")
	}
	running := checks.Load()
	time.Sleep(10 * poll)
	if checks.Load() != running {
		t.Fatal("a running instance was set up again", checks.Load()-running)
	}
}
