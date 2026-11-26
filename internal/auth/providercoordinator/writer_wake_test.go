package providercoordinator

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// writerRunner runs RunWriter alone on a second coordinator over the
// fixture's Store, publisher and resources, so that host counts only its own
// admissions (the fixture's coordinator keeps acquiring with the real host).
// now must advance: RunWriter spaces admissions by RetryInterval on it.
func writerRunner(t *testing.T, f *fixture, host HostCaptureFunc, retry time.Duration, now func() time.Time) *Coordinator {
	t.Helper()
	c, err := New(Options{Store: f.store, Resources: f.resources, Schema: f.schema, Publisher: f.publisher,
		Host: host, HostChanges: f.host.subscribe, PinHost: f.host.pin, Now: now,
		RetryInterval: retry, OperationTimeout: time.Second,
		Admit: func(context.Context, *sqlite.CommandTx, *Admission) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, target := writer(t, f.store)
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- c.RunWriter(ctx, target) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error("writer did not join", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("writer did not join")
		}
	})
	return c
}

// quietAdmissions requires an interval of many retry ticks in which calls
// does not move: the writer is waiting for a notification, not polling.
func quietAdmissions(t *testing.T, calls *atomic.Int64) {
	t.Helper()
	until := time.NewTimer(2 * time.Second)
	defer until.Stop()
	quiet := time.NewTicker(200 * time.Millisecond)
	defer quiet.Stop()
	previous := calls.Load()
	for {
		select {
		case <-until.C:
			t.Fatal("successful unchanged admission kept running every retry interval")
		case <-quiet.C:
			current := calls.Load()
			if current == previous {
				return
			}
			previous = current
		}
	}
}

func awaitAdmission(t *testing.T, calls *atomic.Int64, after int64, what string) {
	t.Helper()
	until := time.Now().Add(time.Second)
	for calls.Load() <= after {
		if time.Now().After(until) {
			t.Fatal(what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A successful admission of an unchanged state is not repeated on every
// retry tick. Each repetition holds the publication writer gate, the resource
// guard and the SQLite writer across a complete security-section decode, so
// with a full section it starved complete preparation past its bound.
func TestRunWriterSuccessfulIdleDoesNotReadmit(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int64
	writerRunner(t, f, func(ctx context.Context, r *providerregistry.Registry) (HostCapture, error) {
		calls.Add(1)
		return f.host.capture(ctx, r)
	}, 10*time.Millisecond, time.Now)
	awaitAdmission(t, &calls, 0, "initial admission missing")
	quietAdmissions(t, &calls)
}

// The writer admits again on what the retry tick used to pick up: a host
// notification (license or keys) and a notification that arrived while an
// admission ran, which its subscription taken before admission retains.
func TestRunWriterAdmitsAgainOnHostAndRetainedChanges(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int64
	var arm atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	c := writerRunner(t, f, func(ctx context.Context, r *providerregistry.Registry) (HostCapture, error) {
		calls.Add(1)
		if arm.CompareAndSwap(true, false) {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return f.host.capture(ctx, r)
	}, 10*time.Millisecond, time.Now)
	awaitAdmission(t, &calls, 0, "initial admission missing")
	quietAdmissions(t, &calls)

	before := calls.Load()
	f.host.install(f.host.state.Load()) // a notification; the capture stays current
	awaitAdmission(t, &calls, before, "host notification did not admit again")
	quietAdmissions(t, &calls)

	arm.Store(true)
	c.signal() // starts one admission, held in its host capture
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("notification did not start an admission")
	}
	before = calls.Load()
	c.signal() // arrives while that admission runs
	close(release)
	awaitAdmission(t, &calls, before, "notification during admission was lost")
}

// The original host deadline still bounds a successful admission: the writer
// admits again when it passes, with no notification.
func TestRunWriterRetainsHostDeadline(t *testing.T) {
	f := newFixture(t)
	deadline := time.Now().Add(2 * time.Second)
	var calls atomic.Int64
	writerRunner(t, f, func(ctx context.Context, r *providerregistry.Registry) (HostCapture, error) {
		calls.Add(1)
		h, err := f.host.capture(ctx, r)
		h.ValidUntil = deadline
		return h, err
	}, 10*time.Millisecond, time.Now)
	awaitAdmission(t, &calls, 0, "initial admission missing")
	before := calls.Load()
	select {
	case <-time.After(time.Until(deadline) + 200*time.Millisecond):
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	if calls.Load() <= before {
		t.Fatal("host deadline was not retained")
	}
}
