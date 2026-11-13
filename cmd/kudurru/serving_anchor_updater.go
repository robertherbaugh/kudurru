package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providercoordinator"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

// Only the raw acquisition boundary issues this category. Authentication,
// policy admission, SQL and completion failures never receive an automatic
// protocol retry. Unwrap preserves original cancellation/error identity.
type anchorAcquisitionFailure struct{ err error }

func (e *anchorAcquisitionFailure) Error() string { return "root anchor acquisition failed" }
func (e *anchorAcquisitionFailure) Unwrap() error { return e.err }

// A completed receipt must remain terminal even when caller cancellation races
// authentication or the final command. Outer workers may join, but cannot turn
// that cancellation into permission to acquire the same source again.
type anchorTerminalFailure struct{ err error }

func (e *anchorTerminalFailure) Error() string { return e.err.Error() }
func (e *anchorTerminalFailure) Unwrap() error { return e.err }

type anchorRetrySource struct {
	owner                        accepted.Version
	policyID, seeds              string
	settingsUID, settingsVersion string
}

func anchorRetryIdentity(op *anchorOperation) anchorRetrySource {
	return anchorRetrySource{owner: op.owner.Version(), policyID: op.policy.PolicyID,
		seeds: op.policy.SeedDigest, settingsUID: string(op.settings.UID), settingsVersion: op.settings.ResourceVersion}
}

type anchorUpdater struct {
	runtime  *servingRuntime
	target   serving.TermActivation
	host     providercoordinator.HostCapture
	clock    *trustanchors.SessionClock
	offset   uint64
	retryFor anchorRetrySource
	retryAt  time.Time
	// raced counts consecutive acquisitions rolled back because an ordinary
	// commit moved the canonical generation first; it paces their retries.
	raced int
	// captureRaced counts consecutive captures refused the same way before
	// any acquisition (not backed off; logged at powers of two); a
	// successful capture resets it.
	captureRaced int
	stopped      error
	// wall reads this node's wall clock for its due and checkpoint
	// comparisons only; nil means time.Now. Samples, receipts and stored
	// state never use it.
	wall func() time.Time
}

// anchorUpdaterBuildWall replaces the mounted writer updater's wall clock (its
// due and checkpoint comparisons only) in every runtime of this process. It
// is nil in every build except one with -tags licensetest, where
// anchor_updater_wall_licensetest.go may set it for the separate-process
// tests (D369). A release binary never carries it:
// TestDefaultBuildHasNoAnchorUpdaterWall.
var anchorUpdaterBuildWall func() time.Time

type anchorPollStep struct {
	next             time.Time
	attempted        bool
	completion       anchorCompletion
	acquisitionError error
	// clockWait is how far this node's wall clock trails the committed
	// checkpoint when the step could not be due for that reason.
	clockWait time.Duration
	// raced reports that an ordinary commit moved the canonical generation
	// under this step (at capture or while the reply was in flight) and that
	// a new acquisition is scheduled at next.
	raced bool
}

func (u *anchorUpdater) now() time.Time {
	if u.wall != nil {
		return u.wall()
	}
	return time.Now()
}

// step creates and cancels one finite original attempt before returning any
// scheduling decision. Only a subsequent captured committed owner supplies the
// next success schedule; a staged proposal or completion guess never does.
func (u *anchorUpdater) step(ctx context.Context) (step anchorPollStep, err error) {
	if u.stopped != nil {
		return step, u.stopped
	}
	defer func() {
		if err != nil {
			u.stopped = err
		}
	}()
	original, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	op, err := u.runtime.captureAnchorOperation(original, u.target, u.host, u.clock, u.offset)
	now := u.now()
	if errors.Is(err, replication.ErrCAS) {
		// An ordinary commit landed between the image read and the capture.
		// Nothing was acquired; a later step captures the new generation one
		// second later. This pace is deliberately not backed off: a capture
		// is a local read with no root traffic, and on a fresh node the
		// bootstrap it delays is what lets the node publish at all (first-boot
		// status writes race it several times in a row).
		step.next, step.raced = now.Add(time.Second), true
		u.captureRaced++
		return step, nil
	}
	if err != nil {
		return step, err
	}
	u.captureRaced = 0
	if op == nil {
		step.next = now.Add(time.Minute)
		return step, nil
	}
	step.next = now
	var state trustanchors.State
	managed := op.owner.Inspect().Phase == accepted.Managed
	if managed {
		state, err = op.owner.State()
		if err != nil {
			return step, err
		}
		view := state.Inspect()
		// A newly elected writer whose clock trails the previous writer's last
		// checkpoint by a small skew must not lose the updater for its whole
		// term (D369). Within the tolerance it is simply not due yet.
		step.clockWait, err = anchorCheckpointWait(now, view.Checkpoint.Wall)
		if err != nil {
			return step, err
		}
		step.next = view.Schedule.NextRefresh
		if step.clockWait > 0 && step.next.Before(view.Checkpoint.Wall) {
			step.next = view.Checkpoint.Wall
		}
	}
	identity := anchorRetryIdentity(op)
	if u.retryFor == identity && u.retryAt.After(step.next) {
		step.next = u.retryAt
	}
	if now.Before(step.next) {
		return step, nil
	}
	step.attempted = true
	u.offset++ // endpoint rotation, never an owner/receipt identity
	step.completion, err = op.run()
	u.runtime.anchorStatus.countAcquisition(anchorAcquisitionOutcome(step.completion, err))
	if err == nil {
		switch step.completion.Update.Outcome() {
		case sqlite.AnchorCommitted, sqlite.AnchorUnchanged:
			u.retryFor, u.retryAt, u.raced = anchorRetrySource{}, time.Time{}, 0
			// Re-read accepted state after this effect. Do not derive a poll
			// schedule from the old observation or its proposed postimage.
			step.next = time.Now().Add(time.Second)
			return step, nil
		default:
			return step, errAnchorPolicy
		}
	}
	if anchorUpdateRaced(step.completion, err) {
		// An ordinary commit (a zone status write, an API change) moved the
		// canonical generation while the reply was in flight, and the
		// transaction rolled back. Nothing durable changed and the reply is
		// discarded, never replayed. A later step makes a NEW acquisition
		// against the new generation, paced 1s, 2s, 4s ... up to 64 seconds.
		// The pace is read from the same wall clock the due check uses.
		step.next, step.raced = u.now().Add(time.Second<<min(u.raced, 6)), true
		u.raced++
		u.retryFor, u.retryAt = identity, step.next
		return step, nil
	}
	var acquisition *anchorAcquisitionFailure
	if !errors.As(err, &acquisition) {
		return step, &anchorTerminalFailure{err: err}
	}
	if ctx.Err() != nil {
		return step, ctx.Err()
	}
	// No authenticated input or state mutation has entered this failure path.
	// The pure API supplies only the next NEW acquisition's wall schedule.
	if managed {
		step.next, err = state.RetryDue(u.clock.Sample())
		if err != nil {
			return step, err
		}
	} else {
		step.next = time.Now().Add(trustanchors.MinRefresh)
	}
	u.retryFor, u.retryAt = identity, step.next
	step.acquisitionError = acquisition
	return step, nil
}

// anchorAcquisitionOutcome classifies one acquisition the way step acts on
// it, for kudurru_root_anchor_updater_acquisitions_total.
func anchorAcquisitionOutcome(completion anchorCompletion, err error) string {
	var acquisition *anchorAcquisitionFailure
	switch {
	case err == nil && (completion.Update.Outcome() == sqlite.AnchorCommitted || completion.Update.Outcome() == sqlite.AnchorUnchanged):
		return anchorAcquisitionApplied
	case err == nil:
		return anchorAcquisitionRefused
	case anchorUpdateRaced(completion, err):
		return anchorAcquisitionRaced
	case errors.As(err, &acquisition):
		return anchorAcquisitionFailed
	default:
		return anchorAcquisitionRefused
	}
}

// anchorCaptureRaceLogged reports whether the n-th consecutive raced capture
// logs its warning: the first, second, fourth, eighth and so on. A capture
// that keeps racing ordinary commits keeps its one-second pace (it is a
// local read, and before the bootstrap it is what lets a node publish), so
// its log must be bounded instead (F28-OPEN6). The status board shows every
// race as "raced".
func anchorCaptureRaceLogged(n int) bool {
	return n > 0 && n&(n-1) == 0
}

// anchorWriterWall is the mounted writer updater's wall clock for its due and
// checkpoint comparisons: the runtime's test override, else the licensetest
// build seam; nil (time.Now) in a release binary.
func (s *servingRuntime) anchorWriterWall() func() time.Time {
	if s.anchorUpdaterWall != nil {
		return s.anchorUpdaterWall
	}
	return anchorUpdaterBuildWall // nil except in a -tags licensetest binary
}

// anchorWriterNow reads anchorWriterWall.
func (s *servingRuntime) anchorWriterNow() time.Time {
	if wall := s.anchorWriterWall(); wall != nil {
		return wall()
	}
	return time.Now()
}

// anchorUpdateRaced reports a definite rollback caused only by a canonical
// generation change: the update transaction rolled back with the store's
// authority CAS and no denial was committed first. An accepted-owner change
// (accepted.ErrCAS), an uncertain outcome or a committed denial stays terminal.
// So does a revocation denial that an ordinary commit rolled back (Denial
// rolled back, Update unknown): the failed denial closes the anchor source
// and leaves its withdrawal failed-or-unknown, which fails closed for the
// writer term. That case is a recorded follow-up, not a retry.
func anchorUpdateRaced(completion anchorCompletion, err error) bool {
	return errors.Is(err, replication.ErrCAS) && !errors.Is(err, accepted.ErrCAS) &&
		completion.Update.Outcome() == sqlite.AnchorRolledBack && completion.Denial.Outcome() == sqlite.AnchorUnknown
}

// runAnchorUpdater is mounted through the root-admitted writer adapter, which
// joins this worker before releasing the borrowed episode host.
// It never enables a builder or initializes/replaces accepted history itself.
func (s *servingRuntime) runAnchorUpdater(ctx context.Context, target serving.TermActivation, host providercoordinator.HostCapture) error {
	return s.runAnchorUpdaterReady(ctx, target, host, func() {})
}

func (s *servingRuntime) runAnchorUpdaterReady(ctx context.Context, target serving.TermActivation, host providercoordinator.HostCapture, ready func()) error {
	if s == nil || s.anchors == nil || s.anchors.source == nil || ctx == nil {
		return errAnchorPolicy
	}
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		return err
	}
	u := anchorUpdater{runtime: s, target: target, host: host, clock: clock, wall: s.anchorWriterWall()}
	for {
		// Subscribe before the read. The local anchor notice has a single
		// publisher consumer; use the Store broadcast so this worker cannot
		// steal a denial/publication wake. Accepted schedules and policy are
		// canonical changes, while bounded rechecks cover other refusals.
		changed := s.opts.Store.GenerationChanges()
		step, err := u.step(ctx)
		ready() // release startup sequencing only after the original step joins
		if err != nil {
			return err // no replay after authentication/admission/completion refusal
		}
		state := anchorWorkerRunning
		if step.acquisitionError != nil {
			state = anchorWorkerRetry
			slog.Warn("root anchor transport acquisition failed; new attempt scheduled", "next", step.next)
		}
		if step.raced {
			state = anchorWorkerRaced
			// A capture-phase race acquired nothing; an update-phase race
			// discarded one reply. Each logs its own consecutive count.
			if step.attempted {
				slog.Warn("root anchor update raced an ordinary commit; new acquisition scheduled", "next", step.next, "consecutive", u.raced)
			} else if anchorCaptureRaceLogged(u.captureRaced) {
				slog.Warn("root anchor capture raced an ordinary commit; capturing again", "next", step.next, "consecutive", u.captureRaced)
			}
		}
		if step.clockWait > 0 {
			state = anchorWorkerClockWait
			slog.Warn("root anchor updater waits for this node's clock to reach the committed checkpoint; check time synchronization", "behind", step.clockWait.Round(time.Millisecond))
		}
		s.anchorStatus.setUpdater(state, step.next, nil)
		// Periodic rechecks bound reaction to clock or canonical policy changes.
		// A source wake cannot turn into an unbounded acquisition/read loop.
		delay := min(time.Minute, max(time.Second, time.Until(step.next)))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-changed:
			timer.Stop()
			timer = time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		case <-timer.C:
		}
	}
}
