package providercoordinator

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Reuse the real initialized Store and host after joining the fixture's
// workers. This runner exercises acquisition only, with no publication claim.
func acquisitionRunner(t *testing.T, f *fixture, host HostCaptureFunc, retry time.Duration, now func() time.Time) *Coordinator {
	t.Helper()
	f.stop()
	if err := f.coordinator.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.publisher.Close()
	resources, err := providerresources.New(providerresources.Options{Now: now, RefreshInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{Store: f.store, Resources: resources, Schema: f.schema, Publisher: f.publisher,
		Host: host, HostChanges: f.host.subscribe, PinHost: f.host.pin, Now: now,
		RetryInterval: retry, OperationTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		joined, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := c.Close(joined); err != nil {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error("acquisition worker did not join", err)
			}
		case <-joined.Done():
			t.Error("acquisition worker did not join")
		}
	})
	return c
}

func TestReconcileSuccessfulIdleDoesNotPollCompleteImage(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int64
	acquisitionRunner(t, f, func(ctx context.Context, r *providerregistry.Registry) (HostCapture, error) {
		calls.Add(1)
		return f.host.capture(ctx, r)
	}, 10*time.Millisecond, f.host.now)
	wait(t, func() bool { return calls.Load() > 0 })
	// SetDesired can produce one resource notification of its own. Allow that
	// actual work to settle, then require a quiet interval across retry ticks.
	until := time.NewTimer(time.Second)
	defer until.Stop()
	quiet := time.NewTicker(100 * time.Millisecond)
	defer quiet.Stop()
	previous := calls.Load()
	for {
		select {
		case <-until.C:
			t.Fatal("successful unchanged source kept polling")
		case <-quiet.C:
			current := calls.Load()
			if current == previous {
				return
			}
			previous = current
		}
	}
}

func TestReconcileRetainsSourceChangeDuringSuccessfulRelease(t *testing.T) {
	f := newFixture(t)
	var arm atomic.Bool
	entered, release, observed := make(chan struct{}), make(chan struct{}), make(chan struct{}, 8)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	acquisitionRunner(t, f, func(ctx context.Context, r *providerregistry.Registry) (HostCapture, error) {
		h, err := f.host.capture(ctx, r)
		if err != nil {
			return h, err
		}
		select {
		case observed <- struct{}{}:
		default:
		}
		if arm.CompareAndSwap(true, false) {
			original := h.Release
			h.Release = func() {
				original()
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
		}
		return h, nil
	}, time.Minute, f.host.now)
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("initial acquisition missing")
	}
	// Drain startup resource notifications before the controlled interposition.
	settled := time.NewTimer(100 * time.Millisecond)
	defer settled.Stop()
	waiting := time.NewTimer(time.Second)
	defer waiting.Stop()
settle:
	for {
		select {
		case <-observed:
			settled.Reset(100 * time.Millisecond)
		case <-settled.C:
			break settle
		case <-waiting.C:
			t.Fatal("initial resource acquisition did not settle")
		}
	}
	arm.Store(true)
	f.host.install(f.host.state.Load()) // notification without replacement work
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("successful acquisition did not enter release")
	}
	for len(observed) > 0 {
		<-observed
	}
	command(t, f.store, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var settings public.Kudurru
		if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
			return err
		}
		settings.Status.Blocking.Disabled = true
		return tx.UpdateStatus(ctx, &settings)
	})
	close(release)
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("source changed during release but its notification was lost")
	}
}

func TestReconcileRetainsOriginalHostDeadline(t *testing.T) {
	f := newFixture(t)
	deadline := time.Now().Add(time.Second)
	var calls atomic.Int64
	acquisitionRunner(t, f, func(ctx context.Context, r *providerregistry.Registry) (HostCapture, error) {
		calls.Add(1)
		h, err := f.host.capture(ctx, r)
		h.ValidUntil = deadline
		return h, err
	}, time.Minute, time.Now)
	wait(t, func() bool { return calls.Load() > 0 })
	before := calls.Load()
	select {
	case <-time.After(time.Until(deadline) + 100*time.Millisecond):
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	if calls.Load() <= before {
		t.Fatal("host deadline was replaced by the retry interval")
	}
}
