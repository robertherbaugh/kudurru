package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
)

// Every captured operation retains its original member/term/role/incarnation.
// A later fresh capture may move to a newer installed incarnation in the same
// member, term and role only when the poller's earlier operations left nothing
// that install could replay: the last acquisition completed as a normal
// observation, or no acquisition has started and the last operation only
// deferred for this node's clock. No refusal or failed acquisition can do so,
// and no operation crosses a role change (D369).
type anchorFollowerEpoch struct {
	member      string
	term        replication.Term
	mode        replication.Mode
	incarnation uint64
}

func followerEpoch(a replication.Authority) anchorFollowerEpoch {
	return anchorFollowerEpoch{member: a.MemberID, term: a.Term, mode: a.Mode, incarnation: a.Incarnation}
}

func (e anchorFollowerEpoch) same(other anchorFollowerEpoch) bool {
	return e.sameRole(other) && e.incarnation == other.incarnation
}

// sameRole ignores only the local incarnation, which every install advances.
func (e anchorFollowerEpoch) sameRole(other anchorFollowerEpoch) bool {
	return e.member == other.member && e.term.Same(other.term) && e.mode == other.mode
}

type anchorFollowerPoller struct {
	runtime *servingRuntime
	clock   *trustanchors.SessionClock
	epoch   anchorFollowerEpoch
	offset  uint64
	nextFor anchorRetrySource
	nextAt  time.Time
	stopped error
	// normalCompleted records an actual authenticated normal disposition whose
	// final original-authority check succeeded. It cannot represent an error,
	// local denial, incomplete transport or a merely staged observation.
	normalCompleted bool
	// acquired records that this poller has started a wire acquisition. It is
	// never cleared, so a clock wait after a failed acquisition changes nothing.
	acquired bool
	// clockDeferred records that this poller has started no acquisition and that
	// its last captured operation was discarded unused because this node's clock
	// had not reached the installed checkpoint. Nothing was held, received or
	// refused. Like the supervisor's readiness wait, it may then observe a newly
	// installed image; without it an ordinary install during the wait would end
	// supervision for the whole member, term and role.
	clockDeferred bool
	// wall reads this node's wall clock for the checkpoint comparison only; nil
	// means time.Now. Samples, receipts and stored state never use it.
	wall func() time.Time
}

type anchorFollowerStep struct {
	next             time.Time
	attempted        bool
	retired          bool
	result           anchorFollowerCompletion
	acquisitionError error
	// clockWait is how far this node's wall clock trails the installed
	// checkpoint when the step deferred its acquisition for that reason.
	clockWait time.Duration
}

// anchorCheckpointWait compares this node's wall clock with the wall time of
// the committed checkpoint it is about to observe against (D369). A member
// whose clock trails the writer of that checkpoint by no more than the pure
// state machine's ClockTolerance waits until its own clock reaches it; the
// wait starts no acquisition and creates no sample, receipt or state. A larger
// gap is not skew between healthy members and stays a terminal ErrClock.
func anchorCheckpointWait(now, checkpoint time.Time) (time.Duration, error) {
	behind := checkpoint.Sub(now)
	if behind <= 0 {
		return 0, nil
	}
	if behind > trustanchors.ClockTolerance {
		return 0, trustanchors.ErrClock
	}
	return behind, nil
}

func (u *anchorFollowerPoller) step(ctx context.Context) (step anchorFollowerStep, err error) {
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
	op, err := u.runtime.captureAnchorFollowerDenial(original, u.clock, u.offset)
	if err != nil {
		return step, err
	}
	epoch := followerEpoch(op.token.Authority())
	if u.epoch == (anchorFollowerEpoch{}) {
		u.epoch = epoch
	} else if !u.epoch.same(epoch) {
		if (!u.normalCompleted && !u.clockDeferred) || !u.epoch.sameRole(epoch) {
			return step, replication.ErrFenced
		}
		// The previous operation completed successfully, or was discarded before
		// any acquisition while this node waited for its clock. This new
		// operation already captured its own installed source, local carrier and
		// lifetime before any wire acquisition. Its checks never use the old token.
		u.epoch = epoch
	}
	state, err := op.owner.State()
	if err != nil {
		return step, err
	}
	now := time.Now()
	if u.wall != nil {
		now = u.wall()
	}
	checkpoint := state.Inspect().Checkpoint.Wall
	step.clockWait, err = anchorCheckpointWait(now, checkpoint)
	if err != nil {
		return step, err
	}
	if step.clockWait > 0 {
		// Nothing was acquired. A later step captures a fresh operation once
		// this node's own clock has reached the installed checkpoint.
		u.clockDeferred = !u.acquired
		step.next = checkpoint
		return step, nil
	}
	step.next = now
	if u.nextFor == op.identity && u.nextAt.After(now) {
		step.next = u.nextAt
		return step, nil
	}
	step.attempted = true
	u.offset++
	u.normalCompleted = false
	u.acquired, u.clockDeferred = true, false
	step.result, err = op.observe()
	if err == nil {
		switch step.result.branch {
		case sqlite.AnchorPollNormal:
			u.normalCompleted = true
			// This local cadence grants no hold-down/checkpoint progress. Poll at
			// the minimum RFC refresh interval, independent of the leader's next
			// observation. Never derive accepted State from the follower input.
			step.next = time.Now().Add(trustanchors.MinRefresh)
			u.nextFor, u.nextAt = op.identity, step.next
			return step, nil
		case sqlite.AnchorPollRevoke:
			switch step.result.completion.Outcome() {
			case sqlite.AnchorCommitted, sqlite.AnchorUnchanged:
				step.retired = true
				u.stopped = sqlite.ErrAnchorUnavailable
				return step, nil
			}
		}
		return step, errAnchorPolicy
	}
	if ctx.Err() != nil {
		return step, ctx.Err()
	}
	var acquisition *anchorAcquisitionFailure
	if !errors.As(err, &acquisition) {
		return step, err
	}
	step.next, err = state.RetryDue(u.clock.Sample())
	if err != nil {
		return step, err
	}
	u.nextFor, u.nextAt = op.identity, step.next
	step.acquisitionError = acquisition
	return step, nil
}

// The supervisor joins the original epoch before allowing a successor. This
// worker never creates canonical state and stops after denial or refusal.
func (s *servingRuntime) runAnchorFollowerPoller(ctx context.Context) error {
	if s == nil || s.opts.Store == nil {
		return errAnchorPolicy
	}
	return s.runAnchorFollowerEpoch(ctx, followerEpoch(s.opts.Store.Authority()))
}

func (s *servingRuntime) runAnchorFollowerEpoch(ctx context.Context, epoch anchorFollowerEpoch) error {
	if s == nil || s.life == nil || s.opts.Store == nil || ctx == nil {
		return errAnchorPolicy
	}
	ctx, cancel := context.WithCancel(ctx)
	joined := make(chan struct{})
	stop := context.AfterFunc(s.life, func() { cancel(); close(joined) })
	defer func() {
		cancel()
		if !stop() {
			<-joined
		}
	}()
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		return err
	}
	u := anchorFollowerPoller{runtime: s, clock: clock, epoch: epoch, wall: s.opts.AnchorWall}
	for {
		changed, authority := s.opts.Store.GenerationChanges(), s.opts.Store.AuthorityChanges()
		step, err := u.step(ctx)
		if err != nil || step.retired {
			return err
		}
		if step.acquisitionError != nil {
			slog.Warn("follower root anchor acquisition failed; new attempt scheduled", "next", step.next)
		}
		if step.clockWait > 0 {
			slog.Warn("root anchor supervision waits for this node's clock to reach the installed checkpoint; check time synchronization", "behind", step.clockWait.Round(time.Millisecond))
		}
		timer := time.NewTimer(min(time.Minute, max(time.Second, time.Until(step.next))))
		woke := false
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-changed:
			woke = true
		case <-authority:
			woke = true
		case <-timer.C:
		}
		timer.Stop()
		if woke {
			// Coalesce changes without an unbounded capture/read loop.
			timer.Reset(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
}
