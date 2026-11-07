package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
)

func TestAnchorUpdaterBootstrapRereadsAcceptedSchedule(t *testing.T) {
	endpoint, seeds, reads := anchorOperationServer(t, 2, nil)
	s, ctx, target, host := anchorOperationFixture(t, endpoint, seeds)
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	u := &anchorUpdater{runtime: s, target: target, host: host, clock: clock}
	first, err := u.step(ctx)
	if err != nil || !first.attempted || first.completion.Update.Outcome() != sqlite.AnchorCommitted || reads.Load() != 1 {
		t.Fatal("scheduler did not perform one actual original bootstrap", err, reads.Load())
	}
	owner := anchorOperationOwner(ctx, t, s)
	state, err := owner.State()
	if err != nil {
		t.Fatal(err)
	}
	second, err := u.step(ctx)
	if err != nil || second.attempted || !second.next.Equal(state.Inspect().Schedule.NextRefresh) || reads.Load() != 1 || !u.retryAt.IsZero() {
		t.Fatal("scheduler did not wait for exact accepted refresh", err, second, reads.Load())
	}
	// A failed acquisition's schedule cannot become another owner's policy.
	// No future Sample or accepted observation is fabricated for this check.
	original, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	op, err := s.captureAnchorOperation(original, target, host, clock, 0)
	if err != nil {
		t.Fatal(err)
	}
	due, err := state.RetryDue(clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	u.retryFor, u.retryAt = anchorRetryIdentity(op), due.Add(time.Hour)
	held, err := u.step(ctx)
	if err != nil || held.attempted || !held.next.Equal(u.retryAt) || reads.Load() != 1 {
		t.Fatal("exact-source retry floor was discarded", err)
	}
	u.retryFor.policyID = "foreign"
	fresh, err := u.step(ctx)
	if err != nil || fresh.attempted || !fresh.next.Equal(state.Inspect().Schedule.NextRefresh) || reads.Load() != 1 {
		t.Fatal("foreign retry schedule suppressed actual committed owner", err)
	}
}

func TestAnchorUpdaterIncompleteWireCooldownAndCancellation(t *testing.T) {
	seed, _ := anchorWireSignedReply(t)
	endpoint, reads := anchorWireServer(t, "tcp", func([]byte) []byte { return nil })
	s, ctx, target, host := anchorOperationFixture(t, endpoint.String(), seed.DS)
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	u := &anchorUpdater{runtime: s, target: target, host: host, clock: clock}
	before := anchorOperationOwner(ctx, t, s).Version()
	started := time.Now()
	first, err := u.step(ctx)
	if err != nil || !first.attempted || first.acquisitionError == nil || first.next.Before(started.Add(trustanchors.MinRefresh)) || reads.Load() != 1 {
		t.Fatal("incomplete wire did not schedule a bounded new attempt", err, first, reads.Load())
	}
	second, err := u.step(ctx)
	if err != nil || second.attempted || !second.next.Equal(first.next) || reads.Load() != 1 || anchorOperationOwner(ctx, t, s).Version() != before {
		t.Fatal("wire failure retried immediately or changed durable owner", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := u.step(canceled); !errors.Is(err, context.Canceled) || reads.Load() != 1 {
		t.Fatal("canceled original scheduler reacquired", err)
	}
}

func TestAnchorUpdaterCompleteReplyAndHostRefusalAreTerminal(t *testing.T) {
	for _, kind := range []string{"correlation", "proof", "host"} {
		t.Run(kind, func(t *testing.T) {
			seed, signed := anchorWireSignedReply(t)
			endpoint, reads := anchorWireServer(t, "tcp", func(raw []byte) []byte {
				response := signed(raw)
				switch kind {
				case "correlation":
					response[0] ^= 1
				case "proof":
					response[len(response)-1] ^= 1
				}
				return response
			})
			s, ctx, target, host := anchorOperationFixture(t, endpoint.String(), seed.DS)
			clock, err := trustanchors.NewSessionClock()
			if err != nil {
				t.Fatal(err)
			}
			var current atomic.Bool
			current.Store(kind != "host")
			originalCurrent := host.Current
			host.Current = func() bool { return current.Load() && originalCurrent() }
			u := &anchorUpdater{runtime: s, target: target, host: host, clock: clock}
			before := anchorOperationOwner(ctx, t, s).Version()
			step, err := u.step(ctx)
			wantReads := int32(1)
			if kind == "host" {
				wantReads = 0
			}
			var acquisition *anchorAcquisitionFailure
			if err == nil || errors.As(err, &acquisition) || step.acquisitionError != nil || !u.retryAt.IsZero() || reads.Load() != wantReads {
				t.Fatal("terminal reply/admission became a transport retry", err, reads.Load())
			}
			current.Store(true)
			if _, nextErr := u.step(ctx); nextErr == nil || reads.Load() != wantReads || anchorOperationOwner(ctx, t, s).Version() != before {
				t.Fatal("stopped scheduler adopted a new proof or host", nextErr)
			}
		})
	}
}

func TestAnchorUpdaterRunJoinsOriginalWait(t *testing.T) {
	seed, _ := anchorWireSignedReply(t)
	endpoint, reads := anchorWireServer(t, "tcp", func([]byte) []byte { return nil })
	s, original, target, host := anchorOperationFixture(t, endpoint.String(), seed.DS)
	ctx, cancel := context.WithCancel(original)
	done := make(chan error, 1)
	go func() { done <- s.runAnchorUpdater(ctx, target, host) }()
	joined := false
	t.Cleanup(func() {
		cancel()
		if !joined {
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					t.Error("updater cleanup", err)
				}
			case <-time.After(3 * time.Second):
				t.Error("updater cleanup failed to join")
			}
		}
	})
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for reads.Load() == 0 {
		select {
		case err := <-done:
			joined = true
			t.Fatal("updater stopped before wire acquisition", err)
		case <-timer.C:
			t.Fatal("updater did not acquire original wire")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		joined = true
		if !errors.Is(err, context.Canceled) || reads.Load() != 1 {
			t.Fatal("updater did not join original lifetime", err, reads.Load())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled updater failed to join")
	}
}
