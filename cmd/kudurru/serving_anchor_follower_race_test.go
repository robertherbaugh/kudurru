package main

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
)

// Found by the separate-process managed acceptance (F28-HA-3): a follower's
// deny-only check stopped for its whole process with "authority changed"
// when an ordinary install landed between its capture's read and final
// check. Nothing had been acquired. The step now defers and the next capture
// adopts the newer incarnation (D369).
func TestAnchorFollowerPollerCaptureRaceBeforeAcquisitionContinues(t *testing.T) {
	endpoint, seeds, reads := anchorSupervisorServer(t, func(int32, *dns.Msg) {})
	primary, ordinary := anchorBootstrappedPrimary(t, endpoint, seeds)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	f := newAnchorFollowerFixture(ctx, t, primary)
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	u := &anchorFollowerPoller{runtime: f.s, clock: clock}
	var raced atomic.Bool
	u.capture = func(ctx context.Context, clock *trustanchors.SessionClock, offset uint64) (*anchorFollowerDenial, error) {
		if raced.CompareAndSwap(false, true) {
			op, err := f.s.captureAnchorFollowerDenial(ctx, clock, offset)
			if err != nil {
				return nil, err
			}
			// The install lands after this capture's read; its final check
			// in a real interleaving refuses it. Re-run the check now.
			anchorOrdinaryInstall(ctx, t, f, primary, ordinary, "capture-race")
			if err := op.checkOriginal(); err == nil {
				t.Fatal("fixture install did not change the captured cut")
			} else {
				return nil, err
			}
		}
		return f.s.captureAnchorFollowerDenial(ctx, clock, offset)
	}
	authority := f.store.Authority()
	step, err := u.step(ctx)
	if err != nil || !step.captureRaced || step.attempted || reads.Load() != 1 || u.stopped != nil {
		t.Fatalf("an install-raced capture stopped the poller: %v %+v reads %d", err, step, reads.Load())
	}
	if reflect.DeepEqual(authority, f.store.Authority()) {
		t.Fatal("fixture did not install")
	}
	step, err = u.step(ctx)
	if err != nil || !step.attempted || step.result.branch != sqlite.AnchorPollNormal || reads.Load() != 2 {
		t.Fatalf("the next capture did not acquire once: %v %+v reads %d", err, step, reads.Load())
	}
}

// The supervisor's read-only readiness classification races installs the
// same way; a refusal that only says the cut changed retries a second later
// instead of ending supervision for the member, term and role.
func TestAnchorFollowerSupervisorReadinessRaceRetries(t *testing.T) {
	endpoint, seeds, reads := anchorSupervisorServer(t, func(int32, *dns.Msg) {})
	primary, _ := anchorBootstrappedPrimary(t, endpoint, seeds)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	f := newAnchorFollowerFixture(ctx, t, primary)
	var calls atomic.Int32
	f.runtime.AnchorReadyHook = func(context.Context) error {
		if calls.Add(1) == 1 {
			return replication.ErrCAS
		}
		return nil
	}
	stop := startAnchorFollowerFixture(ctx, t, f)
	defer stop()
	awaitServingRuntime(t, f.s, func() bool { return reads.Load() == 2 })
	if calls.Load() < 2 {
		t.Fatal("readiness was not retried", calls.Load())
	}
	status, err := f.s.rootAnchorStatus(ctx)
	if err != nil || status.Supervisor.State != anchorWorkerRunning {
		t.Fatalf("supervisor after a raced readiness: %+v %v", status.Supervisor, err)
	}
}
