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

// provisionRetryHost runs a host over one webhook instance whose providers
// come from factory and whose elector comes from elector (the memory store's
// single term when nil), with a 20 ms poll interval. It returns the host and
// the instance's status reader.
func provisionRetryHost(t *testing.T, factory func(string) (integration.Integration, error), elector func(string) store.Elector) (*Host, func() (string, string)) {
	t.Helper()
	st := memory.New()
	in := captureInput(t, webhook("sink"))
	r, err := ledger.New(ledger.Options{Reader: st, Write: func(ctx context.Context, _ ledger.Guard, o store.Object, a store.ApplyOptions) (store.Object, error) {
		return st.Apply(ctx, o, a)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if elector == nil {
		elector = st.Elector
	}
	h, err := New(Options{Capture: func(context.Context) (Input, error) { return in, nil }, Factory: factory, Elector: elector, Fence: func(context.Context, ExecutionGuard) error { return nil }, Ledger: r, Reader: st, Allowed: func() bool { return true },
		PollInterval: 20 * time.Millisecond, OperationTimeout: time.Second, StopTimeout: time.Second, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	key := instanceKey(InstanceID{Namespace: "dns", Name: "sink"})
	return h, func() (string, string) {
		for _, s := range h.Statuses() {
			if instanceKey(s.Owner) == key {
				return s.Phase, s.Reason
			}
		}
		return "", ""
	}
}

// A schedule refused as unsupported (a negotiated provider whose peer
// declared no role this instance can take, like a webhook instance without
// domains against a records-only peer) follows from the definition and the
// peer, so the instance stays ProvisionFailed with one setup instead of
// negotiating with its peer again every backoff step.
func TestDefiniteScheduleRefusalIsNotRetried(t *testing.T) {
	var setups atomic.Int32
	h, status := provisionRetryHost(t, func(string) (integration.Integration, error) {
		return &negotiatedProvider{fakePublisher: &fakePublisher{provision: func(context.Context) error { setups.Add(1); return nil }}, rolesErr: ErrUnsupported}, nil
	}, nil)
	waitFor(t, func() bool { _, reason := status(); return reason == "ProvisionFailed" })
	time.Sleep(time.Second)
	key := instanceKey(InstanceID{Namespace: "dns", Name: "sink"})
	h.mu.RLock()
	j, counted := h.jobs[key], h.provisionFailures[key]
	retry := j != nil && j.provisionRetry
	h.mu.RUnlock()
	if got := setups.Load(); got != 1 || retry || counted != 0 {
		t.Fatalf("a definite refusal was set up %d times in a second (retry %t, failures %d)", got, retry, counted)
	}
	if _, reason := status(); reason != "ProvisionFailed" {
		t.Fatal("the refused instance left ProvisionFailed", reason)
	}
}

// reacquiringElector gives one job two terms in a row and then waits without
// a term until its job ends, like a Lease that is lost after a renewal. It
// counts the jobs that asked for it.
type reacquiringElector struct {
	jobs  *atomic.Int32
	terms chan context.CancelFunc
}

func (e reacquiringElector) Run(ctx context.Context, onStarted func(context.Context), onStopped func()) error {
	if e.jobs.Add(1) == 1 {
		for range 2 {
			term, cancel := context.WithCancel(ctx)
			e.terms <- cancel
			onStarted(term)
			cancel()
		}
	}
	<-ctx.Done()
	if onStopped != nil {
		onStopped()
	}
	return nil
}
func (reacquiringElector) IsLeader() bool { return false }
func (reacquiringElector) Leader() string { return "" }

// A job whose setup failed in one term and succeeded in the next is a
// running instance like any other: when that term is lost, the job waits to
// be elected again instead of being replaced at the next capture as if its
// last setup had failed.
func TestSuccessfulSetupClearsTheRetryOfItsJob(t *testing.T) {
	var setups, jobs atomic.Int32
	terms := make(chan context.CancelFunc, 2)
	_, status := provisionRetryHost(t, func(string) (integration.Integration, error) {
		return &fakePublisher{check: func(context.Context) (integration.Status, error) {
			return integration.Status{Reachable: setups.Add(1) > 1, Authenticated: true}, nil
		}, start: func(ctx context.Context) error { <-ctx.Done(); return nil }}, nil
	}, func(string) store.Elector { return reacquiringElector{jobs: &jobs, terms: terms} })
	<-terms
	second := <-terms
	waitFor(t, func() bool { phase, _ := status(); return phase == "Running" || phase == "Ready" })
	second()
	time.Sleep(500 * time.Millisecond)
	if got := jobs.Load(); got != 1 || setups.Load() != 2 {
		t.Fatalf("the job that set up in its second term was replaced: %d jobs, %d setups", got, setups.Load())
	}
}
