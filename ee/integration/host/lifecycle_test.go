package host

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
)

type fakeElector struct {
	run func(context.Context, func(context.Context), func()) error
}

func (e fakeElector) Run(ctx context.Context, start func(context.Context), stop func()) error {
	return e.run(ctx, start, stop)
}
func (fakeElector) IsLeader() bool { return true }
func (fakeElector) Leader() string { return "node" }
func waitFor(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(time.Millisecond)
	}
}
func lifecycleFixture(t *testing.T, factory func(string) (integration.Integration, error), elector func(string) store.Elector) (*Host, func(Input), context.CancelFunc, <-chan error) {
	t.Helper()
	st := memory.New()
	in := captureInput(t, webhook("sink"))
	var mu sync.RWMutex
	r, err := ledger.New(ledger.Options{Reader: st, Write: func(ctx context.Context, _ ledger.Guard, o store.Object, a store.ApplyOptions) (store.Object, error) {
		return st.Apply(ctx, o, a)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if elector == nil {
		elector = st.Elector
	}
	h, err := New(Options{Capture: func(context.Context) (Input, error) { mu.RLock(); defer mu.RUnlock(); return in, nil }, Factory: factory, Elector: elector, Fence: func(context.Context, ExecutionGuard) error { return nil }, Ledger: r, Reader: st, Allowed: func() bool { return true }, PollInterval: time.Millisecond, OperationTimeout: time.Second, StopTimeout: time.Second, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("host did not quiesce")
		}
	})
	replace := func(next Input) { mu.Lock(); in = next; mu.Unlock(); h.Wake() }
	return h, replace, cancel, done
}
func TestAsyncElectionCallbackQuiescesBeforeReplacement(t *testing.T) {
	var starts, stops atomic.Int32
	stopEntered := make(chan struct{})
	releaseStop := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseStop:
		default:
			close(releaseStop)
		}
	})
	factory := func(string) (integration.Integration, error) {
		index := starts.Add(1)
		return &fakePublisher{start: func(ctx context.Context) error { <-ctx.Done(); return nil }, stop: func(context.Context) error {
			stops.Add(1)
			if index == 1 {
				close(stopEntered)
				<-releaseStop
			}
			return nil
		}}, nil
	}
	elect := func(string) store.Elector {
		return fakeElector{run: func(ctx context.Context, start func(context.Context), _ func()) error {
			go start(ctx)
			<-ctx.Done()
			return nil
		}}
	}
	h, replace, _, _ := lifecycleFixture(t, factory, elect)
	waitFor(t, func() bool { return starts.Load() == 1 })
	next := captureInput(t, webhook("sink"))
	next.BundleID = "new"
	instances, _ := CaptureInstances(context.Background(), next)
	// Change UID, then A-B-A while old Stop blocks. The name must stay quarantined.
	changed := webhook("sink")
	changed.UID = "uid-recreated"
	next = captureInput(t, changed)
	replace(next)
	select {
	case <-stopEntered:
	case <-time.After(time.Second):
		t.Fatal("stop never entered")
	}
	replace(captureInput(t, instances[0].Definition()))
	time.Sleep(20 * time.Millisecond)
	if starts.Load() != 1 {
		t.Fatal("replacement started before old Stop", starts.Load())
	}
	close(releaseStop)
	waitFor(t, func() bool { return starts.Load() == 2 })
	if stops.Load() < 1 {
		t.Fatal("old worker not stopped")
	}
	statuses := h.Statuses()
	if len(statuses) != 1 || statuses[0].Owner.UID != "uid-sink" {
		t.Fatal(statuses)
	}
}
func TestLateCallbackAndSequentialTerms(t *testing.T) {
	t.Run("late callback", func(t *testing.T) {
		h, j, _, _, _ := engineFixture(t)
		j.active = false
		var callback func(context.Context)
		var factories atomic.Int32
		h.opts.Factory = func(string) (integration.Integration, error) { factories.Add(1); return &fakePublisher{}, nil }
		h.opts.Elector = func(string) store.Elector {
			return fakeElector{run: func(_ context.Context, start func(context.Context), _ func()) error { callback = start; return nil }}
		}
		h.elect(j)
		callback(context.Background())
		if factories.Load() != 0 {
			t.Fatal("late callback provisioned removed job")
		}
	})
	t.Run("sequential terms", func(t *testing.T) {
		var factories atomic.Int32
		factory := func(string) (integration.Integration, error) { factories.Add(1); return &fakePublisher{}, nil }
		elect := func(string) store.Elector {
			return fakeElector{run: func(ctx context.Context, start func(context.Context), _ func()) error {
				for range 2 {
					term, cancel := context.WithCancel(ctx)
					timer := time.AfterFunc(20*time.Millisecond, cancel)
					start(term)
					timer.Stop()
					cancel()
				}
				<-ctx.Done()
				return nil
			}}
		}
		_, _, _, _ = lifecycleFixture(t, factory, elect)
		waitFor(t, func() bool { return factories.Load() == 2 })
	})
}
func TestCanceledCommandsAndCompleteCaptureLoss(t *testing.T) {
	var checks atomic.Int32
	p := &fakePublisher{check: func(ctx context.Context) (integration.Status, error) {
		if checks.Add(1) > 1 {
			<-ctx.Done()
			return integration.Status{}, ctx.Err()
		}
		return integration.Status{Authenticated: true, Reachable: true}, nil
	}}
	h, replace, _, _ := lifecycleFixture(t, func(string) (integration.Integration, error) { return p, nil }, nil)
	waitFor(t, func() bool { return len(h.Statuses()) == 1 && h.Statuses()[0].Phase != "Waiting" })
	owner := h.Statuses()[0].Owner
	ctx, cancel := context.WithCancel(context.Background())
	reply := make(chan error, 1)
	go func() { _, err := h.Check(ctx, owner); reply <- err }()
	waitFor(t, func() bool { return checks.Load() > 1 })
	cancel()
	select {
	case err := <-reply:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled command blocked")
	}
	in := captureInput(t, webhook("sink"))
	in.Complete = false
	replace(in)
	waitFor(t, func() bool { return len(h.Statuses()) == 0 })
	if _, err := h.Check(context.Background(), owner); !errors.Is(err, ErrUnavailable) {
		t.Fatal("incomplete capture left old instance active", err)
	}
}
func TestIncarnationChangesAcrossRestart(t *testing.T) {
	h, _, _, _, _ := engineFixture(t)
	other, _, _, _, _ := engineFixture(t)
	if h.incarnation == other.incarnation {
		t.Fatal("restart reused incarnation")
	}
}

type countingPublisher struct {
	fakePublisher
	plans atomic.Int32
}

func (p *countingPublisher) Destinations(context.Context) ([]Publication, error) {
	p.plans.Add(1)
	return nil, nil
}
func TestBundleOnlyRefreshDoesNotSelfTriggerSync(t *testing.T) {
	p := &countingPublisher{}
	h, replace, _, _ := lifecycleFixture(t, func(string) (integration.Integration, error) { return p, nil }, nil)
	waitFor(t, func() bool { return p.plans.Load() == 1 })
	h.mu.RLock()
	in := h.jobs["dns/sink"].frame.input
	h.mu.RUnlock()
	in.BundleID = "runtime-receipt-status-update"
	replace(in)
	time.Sleep(20 * time.Millisecond)
	if p.plans.Load() != 1 {
		t.Fatal("runtime bundle churn triggered self-sync", p.plans.Load())
	}
	in.BundleID = "source-update"
	in.Sources = sources()
	replace(in)
	waitFor(t, func() bool { return p.plans.Load() == 2 })
}
