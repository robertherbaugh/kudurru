package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providercoordinator"
	"github.com/robertherbaugh/kudurru/internal/serving"
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
	stopped  error
}

type anchorPollStep struct {
	next             time.Time
	attempted        bool
	completion       anchorCompletion
	acquisitionError error
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
	if err != nil {
		return step, err
	}
	now := time.Now()
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
		if now.Before(view.Checkpoint.Wall) {
			return step, trustanchors.ErrClock
		}
		step.next = view.Schedule.NextRefresh
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
	if err == nil {
		switch step.completion.Update.Outcome() {
		case sqlite.AnchorCommitted, sqlite.AnchorUnchanged:
			u.retryFor, u.retryAt = anchorRetrySource{}, time.Time{}
			// Re-read accepted state after this effect. Do not derive a poll
			// schedule from the old observation or its proposed postimage.
			step.next = time.Now().Add(time.Second)
			return step, nil
		default:
			return step, errAnchorPolicy
		}
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

// runAnchorUpdater is deliberately not mounted by writerEpisode yet. Its
// eventual caller must first compose original installed edge activation
// admission and join this worker before releasing the borrowed episode host.
// It never enables a builder or initializes/replaces accepted history itself.
func (s *servingRuntime) runAnchorUpdater(ctx context.Context, target serving.TermActivation, host providercoordinator.HostCapture) error {
	if s == nil || s.anchors == nil || s.anchors.source == nil || ctx == nil {
		return errAnchorPolicy
	}
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		return err
	}
	u := anchorUpdater{runtime: s, target: target, host: host, clock: clock}
	for {
		// Subscribe before the read. The local anchor notice has a single
		// publisher consumer; use the Store broadcast so this worker cannot
		// steal a denial/publication wake. Accepted schedules and policy are
		// canonical changes, while bounded rechecks cover other refusals.
		changed := s.opts.Store.GenerationChanges()
		step, err := u.step(ctx)
		if err != nil {
			return err // no replay after authentication/admission/completion refusal
		}
		if step.acquisitionError != nil {
			slog.Warn("root anchor transport acquisition failed; new attempt scheduled", "next", step.next)
		}
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
