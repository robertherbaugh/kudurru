package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
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

// A newly elected writer starts a new session clock. If its wall clock trails
// the previous writer's committed checkpoint by a small skew, the first step
// used to return ErrClock and the writer adapter kept that terminal result for
// the whole elected term. Within the tolerance it is only not due yet: no
// acquisition, no state change and no credit. Beyond it the refusal stays.
func TestAnchorUpdaterCheckpointSkewWaitsWithinToleranceAndRefusesBeyond(t *testing.T) {
	endpoint, seeds, reads := anchorOperationServer(t, 2, nil)
	s, ctx, target, host := anchorOperationFixture(t, endpoint, seeds)
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	original := &anchorUpdater{runtime: s, target: target, host: host, clock: clock}
	if first, err := original.step(ctx); err != nil || !first.attempted || first.clockWait != 0 || reads.Load() != 1 {
		t.Fatal("fixture did not commit one original bootstrap checkpoint", err, reads.Load())
	}
	owner := anchorOperationOwner(ctx, t, s)
	state, err := owner.State()
	if err != nil {
		t.Fatal(err)
	}
	view := state.Inspect()
	successor, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	skewed := &anchorUpdater{runtime: s, target: target, host: host, clock: successor, wall: anchorTrailingWall(view.Checkpoint.Wall, 2*time.Second)}
	for range 2 {
		step, err := skewed.step(ctx)
		if err != nil || step.attempted || step.clockWait <= 0 || step.clockWait > 2*time.Second || !step.next.Equal(view.Schedule.NextRefresh) || skewed.stopped != nil || skewed.offset != 0 || !skewed.retryAt.IsZero() {
			t.Fatal("small checkpoint skew stopped or advanced the successor updater", step, err)
		}
	}
	if reads.Load() != 1 || anchorOperationOwner(ctx, t, s).Version() != owner.Version() {
		t.Fatal("waiting updater acquired or changed accepted history", reads.Load())
	}
	time.Sleep(view.Checkpoint.Wall.Sub(skewed.wall()) + 50*time.Millisecond)
	due, err := skewed.step(ctx)
	if err != nil || due.attempted || due.clockWait != 0 || !due.next.Equal(view.Schedule.NextRefresh) || reads.Load() != 1 {
		t.Fatal("updater did not return to the exact accepted refresh after the wait", due, err)
	}
	far := &anchorUpdater{runtime: s, target: target, host: host, clock: successor, wall: anchorTrailingWall(view.Checkpoint.Wall, trustanchors.ClockTolerance+time.Second)}
	refused, err := far.step(ctx)
	if !errors.Is(err, trustanchors.ErrClock) || refused.attempted || far.stopped == nil || reads.Load() != 1 {
		t.Fatal("checkpoint beyond the clock tolerance was not terminal", refused, err)
	}
	far.wall = nil
	if _, next := far.step(ctx); next != err || reads.Load() != 1 || anchorOperationOwner(ctx, t, s).Version() != owner.Version() {
		t.Fatal("terminal clock refusal was recaptured", next)
	}
}

// The elected updater has no installed incarnation to keep: every step captures
// a fresh operation under its original writer term. Ordinary writes that land
// while a newly elected writer waits for its clock therefore neither stop it nor
// make it due, and it returns to the exact accepted refresh afterwards.
func TestAnchorUpdaterCheckpointWaitSurvivesOrdinaryWrites(t *testing.T) {
	endpoint, seeds, reads := anchorOperationServer(t, 2, nil)
	s, ctx, target, host := anchorOperationFixture(t, endpoint, seeds)
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	original := &anchorUpdater{runtime: s, target: target, host: host, clock: clock}
	if first, err := original.step(ctx); err != nil || !first.attempted || reads.Load() != 1 {
		t.Fatal("fixture did not commit one original bootstrap checkpoint", err, reads.Load())
	}
	owner := anchorOperationOwner(ctx, t, s)
	state, err := owner.State()
	if err != nil {
		t.Fatal(err)
	}
	view := state.Inspect()
	successor, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	wall := &anchorHeldWall{at: view.Checkpoint.Wall.Add(-2 * time.Second)}
	skewed := &anchorUpdater{runtime: s, target: target, host: host, clock: successor, wall: wall.now}
	if step, err := skewed.step(ctx); err != nil || step.attempted || step.clockWait != 2*time.Second {
		t.Fatal("fixture updater did not wait for its clock", step, err)
	}
	for _, name := range []string{"updater-skew-first", "updater-skew-second"} {
		before := anchorFollowerImage(ctx, t, s.opts.Store)
		anchorOrdinaryWrite(ctx, t, s.opts.Store, target.Term, name)
		if anchorFollowerImage(ctx, t, s.opts.Store).Generation().Same(before.Generation()) {
			t.Fatal("fixture write did not advance the canonical generation")
		}
		step, err := skewed.step(ctx)
		if err != nil || skewed.stopped != nil || step.attempted || step.clockWait != 2*time.Second || !step.next.Equal(view.Schedule.NextRefresh) || skewed.offset != 0 {
			t.Fatal("an ordinary write during the checkpoint wait stopped or advanced the updater", step, err)
		}
	}
	wall.reached.Store(true)
	due, err := skewed.step(ctx)
	if err != nil || due.attempted || due.clockWait != 0 || !due.next.Equal(view.Schedule.NextRefresh) || reads.Load() != 1 {
		t.Fatal("updater did not return to the exact accepted refresh after the wait", due, err)
	}
	if anchorOperationOwner(ctx, t, s).Version() != owner.Version() {
		t.Fatal("waiting updater changed accepted history")
	}
}

// F28-EF-1. An ordinary commit (here a zone write, in production typically the
// startup zone status write) that lands while the root reply is in flight
// rolls the bootstrap back with the store's authority CAS. That refusal is
// definite and changes nothing, so it must not park the updater for the whole
// writer term: a later step makes one NEW acquisition and commits it. The
// raced reply is never replayed, and retries are paced.
func TestAnchorUpdaterRacedCommitAcquiresAgain(t *testing.T) {
	var write func()
	endpoint, seeds, reads := anchorOperationServer(t, 3, func(n int) {
		if n == 1 {
			write()
		}
	})
	s, ctx, target, host := anchorOperationFixture(t, endpoint, seeds)
	write = func() { anchorOrdinaryWrite(ctx, t, s.opts.Store, target.Term, "raced-bootstrap") }
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	reg := anchorStatusMetrics(t, s)
	u := &anchorUpdater{runtime: s, target: target, host: host, clock: clock}
	before := anchorOperationOwner(ctx, t, s).Version()
	started := time.Now()
	raced, err := u.step(ctx)
	if err != nil || u.stopped != nil || !raced.attempted || !raced.raced || raced.completion.Update.Outcome() != sqlite.AnchorRolledBack || reads.Load() != 1 {
		t.Fatal("a raced bootstrap stopped the updater, was not a definite rollback or was not reported", err, raced.completion.Update.Outcome(), reads.Load())
	}
	if raced.next.Before(started.Add(time.Second)) || raced.next.After(time.Now().Add(time.Second)) || raced.acquisitionError != nil {
		t.Fatal("raced bootstrap was not paced by one second", raced.next.Sub(started))
	}
	if anchorOperationOwner(ctx, t, s).Version() != before {
		t.Fatal("raced bootstrap changed accepted history")
	}
	if got := anchorAcquisitionCounts(t, reg); got[anchorAcquisitionRaced] != 1 || got[anchorAcquisitionApplied]+got[anchorAcquisitionFailed]+got[anchorAcquisitionRefused] != 0 {
		t.Fatal("the raced acquisition is not counted as raced", got)
	}
	held, err := u.step(ctx)
	if err != nil || held.attempted || held.raced || reads.Load() != 1 {
		t.Fatal("raced bootstrap retried before its pace", err, reads.Load())
	}
	time.Sleep(time.Until(raced.next) + 50*time.Millisecond)
	again, err := u.step(ctx)
	if err != nil || !again.attempted || again.raced || again.completion.Update.Outcome() != sqlite.AnchorCommitted || reads.Load() != 2 || u.raced != 0 {
		t.Fatal("updater did not bootstrap with one new acquisition after the race", err, again.completion.Update.Outcome(), reads.Load())
	}
	// Every root query is accounted for: one raced, one applied.
	if got := anchorAcquisitionCounts(t, reg); got[anchorAcquisitionRaced] != 1 || got[anchorAcquisitionApplied] != 1 || got[anchorAcquisitionFailed]+got[anchorAcquisitionRefused] != 0 {
		t.Fatal("the bootstrap acquisitions are not counted by outcome", got)
	}
	owner := anchorOperationOwner(ctx, t, s)
	if owner.Version() == before {
		t.Fatal("new acquisition did not commit managed trust")
	}
	state, err := owner.State()
	if err != nil {
		t.Fatal(err)
	}
	next, err := u.step(ctx)
	if err != nil || next.attempted || !next.next.Equal(state.Inspect().Schedule.NextRefresh) || reads.Load() != 2 || !u.retryAt.IsZero() {
		t.Fatal("updater did not return to the accepted schedule", err, reads.Load())
	}
}

// anchorAheadWall is this node's wall clock moved forward by a settable
// offset, so a managed refresh can be made due without waiting for it.
type anchorAheadWall struct{ ahead atomic.Int64 }

func (w *anchorAheadWall) now() time.Time { return time.Now().Add(time.Duration(w.ahead.Load())) }

// anchorDueRefresh bootstraps u once and moves its wall clock just past the
// committed owner's NextRefresh, so the next step is a managed poll.
func anchorDueRefresh(ctx context.Context, t *testing.T, s *servingRuntime, u *anchorUpdater, wall *anchorAheadWall, reads *atomic.Int32) accepted.Version {
	t.Helper()
	if first, err := u.step(ctx); err != nil || !first.attempted || first.completion.Update.Outcome() != sqlite.AnchorCommitted || reads.Load() != 1 {
		t.Fatal("fixture did not commit one original bootstrap", err, reads.Load())
	}
	owner := anchorOperationOwner(ctx, t, s)
	state, err := owner.State()
	if err != nil {
		t.Fatal(err)
	}
	wall.ahead.Store(int64(time.Until(state.Inspect().Schedule.NextRefresh) + time.Second))
	return owner.Version()
}

// The managed advance poll is the path every refresh after the bootstrap
// takes. An ordinary commit that lands while its reply is in flight rolls the
// advance back with the store's authority CAS: nothing durable changes, the
// reply is discarded, and after a paced hold one NEW acquisition commits.
func TestAnchorUpdaterRacedAdvancePollAcquiresAgain(t *testing.T) {
	var write func()
	endpoint, seeds, reads := anchorOperationServer(t, 4, func(n int) {
		if n == 2 {
			write()
		}
	})
	s, ctx, target, host := anchorOperationFixture(t, endpoint, seeds)
	write = func() { anchorOrdinaryWrite(ctx, t, s.opts.Store, target.Term, "raced-advance") }
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	wall := &anchorAheadWall{}
	u := &anchorUpdater{runtime: s, target: target, host: host, clock: clock, wall: wall.now}
	before := anchorDueRefresh(ctx, t, s, u, wall, reads)
	started := u.now()
	raced, err := u.step(ctx)
	if err != nil || u.stopped != nil || !raced.attempted || !raced.raced || raced.acquisitionError != nil || reads.Load() != 2 {
		t.Fatal("a raced advance poll stopped the updater or was not reported", err, reads.Load())
	}
	if raced.completion.Update.Outcome() != sqlite.AnchorRolledBack || raced.completion.Denial.Outcome() != sqlite.AnchorUnknown {
		t.Fatal("raced advance poll was not a definite rollback without denial", raced.completion.Update.Outcome(), raced.completion.Denial.Outcome())
	}
	if raced.next.Before(started.Add(time.Second)) || raced.next.After(u.now().Add(time.Second)) {
		t.Fatal("raced advance poll was not paced by one second", raced.next.Sub(started))
	}
	if anchorOperationOwner(ctx, t, s).Version() != before {
		t.Fatal("raced advance poll changed accepted history")
	}
	held, err := u.step(ctx)
	if err != nil || held.attempted || held.raced || !held.next.Equal(raced.next) || reads.Load() != 2 {
		t.Fatal("raced advance poll retried before its pace", err, reads.Load())
	}
	time.Sleep(raced.next.Sub(u.now()) + 50*time.Millisecond)
	again, err := u.step(ctx)
	if err != nil || !again.attempted || again.raced || reads.Load() != 3 || u.raced != 0 {
		t.Fatal("updater did not poll with one new acquisition after the race", err, reads.Load())
	}
	switch again.completion.Update.Outcome() {
	case sqlite.AnchorCommitted, sqlite.AnchorUnchanged:
	default:
		t.Fatal("new acquisition after the race did not complete", again.completion.Update.Outcome())
	}
	owner := anchorOperationOwner(ctx, t, s)
	if again.completion.Update.Outcome() == sqlite.AnchorCommitted && owner.Version() == before {
		t.Fatal("committed advance did not move accepted history")
	}
	state, err := owner.State()
	if err != nil {
		t.Fatal(err)
	}
	wall.ahead.Store(0)
	next, err := u.step(ctx)
	if err != nil || next.attempted || !next.next.Equal(state.Inspect().Schedule.NextRefresh) || reads.Load() != 3 || !u.retryAt.IsZero() {
		t.Fatal("updater did not return to the accepted schedule", err, reads.Load())
	}
}

// A revocation reply whose denial an ordinary commit rolls back is NOT
// retried. The failed denial closes the anchor source and leaves its
// withdrawal failed-or-unknown, so the updater fails closed for the writer
// term as before (a recorded follow-up). This pins that behavior so that a
// later change to it is deliberate.
func TestAnchorUpdaterRacedRevokeDenialStaysTerminal(t *testing.T) {
	var write func()
	endpoint, seeds, reads := anchorOperationServer(t, 2, func(n int) {
		if n == 2 {
			write()
		}
	})
	s, ctx, target, host := anchorOperationFixture(t, endpoint, seeds)
	write = func() { anchorOrdinaryWrite(ctx, t, s.opts.Store, target.Term, "raced-revoke") }
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	wall := &anchorAheadWall{}
	u := &anchorUpdater{runtime: s, target: target, host: host, clock: clock, wall: wall.now}
	before := anchorDueRefresh(ctx, t, s, u, wall, reads)
	raced, err := u.step(ctx)
	if err == nil || u.stopped == nil || !raced.attempted || raced.raced || reads.Load() != 2 {
		t.Fatal("a raced revocation denial was retried", err, reads.Load())
	}
	if !errors.Is(err, replication.ErrCAS) || raced.completion.Denial.Outcome() != sqlite.AnchorRolledBack || raced.completion.Update.Outcome() != sqlite.AnchorUnknown {
		t.Fatal("fixture did not race the revocation denial", err, raced.completion.Denial.Outcome(), raced.completion.Update.Outcome())
	}
	if again, next := u.step(ctx); next != err || again.attempted || reads.Load() != 2 {
		t.Fatal("terminal revocation refusal was recaptured", next)
	}
	if anchorOperationOwner(ctx, t, s).Version() != before {
		t.Fatal("rolled-back revocation denial changed accepted history")
	}
}

// A commit that lands between a capture's image read and its currency check
// refuses only that capture (ErrCAS): nothing was acquired, so the updater
// keeps running and captures again one second later, counting consecutive
// raced captures on their own; a successful capture resets the count
// (F28-OPEN6). The pace is not backed off: a capture is a local read, and on
// a fresh node a backed-off bootstrap kept the node unready for many seconds.
func TestAnchorUpdaterCaptureRaceRetriesPacedWithoutStopping(t *testing.T) {
	endpoint, seeds, reads := anchorOperationServer(t, 2, nil)
	s, ctx, target, host := anchorOperationFixture(t, endpoint, seeds)
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	u := &anchorUpdater{runtime: s, target: target, host: host, clock: clock}
	if first, err := u.step(ctx); err != nil || !first.attempted || reads.Load() != 1 {
		t.Fatal("fixture did not commit one original bootstrap", err, reads.Load())
	}
	state, err := anchorOperationOwner(ctx, t, s).State()
	if err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int32
	s.anchorCaptureRead = func() {
		writes.Add(1)
		anchorOrdinaryWrite(ctx, t, s.opts.Store, target.Term, fmt.Sprintf("capture-race-%d", writes.Load()))
	}
	for i := range 3 {
		pace := time.Second
		started := time.Now()
		raced, err := u.step(ctx)
		if err != nil || u.stopped != nil {
			t.Fatal("a raced capture stopped the updater", i, err)
		}
		if raced.attempted || !raced.raced || raced.acquisitionError != nil || reads.Load() != 1 || u.offset != 1 || !u.retryAt.IsZero() {
			t.Fatal("a raced capture acquired, rotated or scheduled a transport retry", i, reads.Load(), u.offset)
		}
		if u.captureRaced != i+1 || u.raced != 0 {
			t.Fatal("raced capture was not counted on its own", i, u.captureRaced, u.raced)
		}
		if raced.next.Before(started.Add(pace)) || raced.next.After(time.Now().Add(pace)) {
			t.Fatal("raced capture was not paced", i, raced.next.Sub(started), pace)
		}
	}
	if int(writes.Load()) != 3 {
		t.Fatal("fixture write did not run inside each capture", writes.Load())
	}
	s.anchorCaptureRead = nil
	again, err := u.step(ctx)
	if err != nil || again.attempted || again.raced || !again.next.Equal(state.Inspect().Schedule.NextRefresh) || reads.Load() != 1 || u.captureRaced != 0 {
		t.Fatal("the next capture did not succeed, reset the pace and wait for the accepted refresh", err, reads.Load(), u.captureRaced)
	}
}

// A capture that keeps racing ordinary commits stays paced at one second
// (TestAnchorUpdaterCaptureRaceRetriesPacedWithoutStopping), so its warning
// is rate limited instead: the first, second, fourth, eighth and so on
// consecutive race logs, an hour of races at most a dozen lines (F28-OPEN6).
func TestAnchorCaptureRaceLogIsBounded(t *testing.T) {
	logged := 0
	for n := 1; n <= 3600; n++ {
		if anchorCaptureRaceLogged(n) {
			logged++
		}
	}
	if logged != 12 || !anchorCaptureRaceLogged(1) || !anchorCaptureRaceLogged(2) || anchorCaptureRaceLogged(3) || !anchorCaptureRaceLogged(4) || anchorCaptureRaceLogged(0) {
		t.Fatal("raced capture warnings are not bounded to powers of two", logged)
	}
}

// An error that ends an acquisition after the writer context ended is
// counted as canceled, not as a refusal that stopped the updater on the
// reply, whatever the error is. With the context live, a transport failure
// stays failed and any other error refused.
func TestAnchorAcquisitionOutcomeSeparatesCancellation(t *testing.T) {
	live := context.Background()
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	transport := &anchorAcquisitionFailure{err: context.Canceled}
	store := errors.New("sql: transaction has already been committed or rolled back")
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want string
	}{
		{"transport", live, transport, anchorAcquisitionFailed},
		{"refusal", live, trustanchors.ErrProof, anchorAcquisitionRefused},
		{"store", live, store, anchorAcquisitionRefused},
		{"canceled transport", ended, transport, anchorAcquisitionCanceled},
		{"canceled store", ended, store, anchorAcquisitionCanceled},
		{"canceled terminal", ended, &anchorTerminalFailure{err: context.Canceled}, anchorAcquisitionCanceled},
	} {
		if got := anchorAcquisitionOutcome(tc.ctx, anchorCompletion{}, tc.err); got != tc.want {
			t.Errorf("%s: outcome %q, want %q", tc.name, got, tc.want)
		}
	}
}
