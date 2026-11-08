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

// A follower episode retains its original member/term/incarnation. New complete
// installs within that episode supply new captured owners, never a primary
// writer or a replacement episode after promotion/demotion.
type anchorFollowerEpoch struct {
	member      string
	term        replication.Term
	incarnation uint64
}

func followerEpoch(a replication.Authority) anchorFollowerEpoch {
	return anchorFollowerEpoch{member: a.MemberID, term: a.Term, incarnation: a.Incarnation}
}

func (e anchorFollowerEpoch) same(other anchorFollowerEpoch) bool {
	return e.member == other.member && e.term.Same(other.term) && e.incarnation == other.incarnation
}

type anchorFollowerPoller struct {
	runtime *servingRuntime
	clock   *trustanchors.SessionClock
	epoch   anchorFollowerEpoch
	offset  uint64
	nextFor anchorRetrySource
	nextAt  time.Time
	stopped error
}

type anchorFollowerStep struct {
	next             time.Time
	attempted        bool
	retired          bool
	result           anchorFollowerCompletion
	acquisitionError error
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
		return step, replication.ErrFenced
	}
	state, err := op.owner.State()
	if err != nil {
		return step, err
	}
	now := time.Now()
	if now.Before(state.Inspect().Checkpoint.Wall) {
		return step, trustanchors.ErrClock
	}
	step.next = now
	if u.nextFor == op.identity && u.nextAt.After(now) {
		step.next = u.nextAt
		return step, nil
	}
	step.attempted = true
	u.offset++
	step.result, err = op.observe()
	if err == nil {
		switch step.result.branch {
		case sqlite.AnchorPollNormal:
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
	u := anchorFollowerPoller{runtime: s, clock: clock, epoch: epoch}
	for {
		changed, authority := s.opts.Store.GenerationChanges(), s.opts.Store.AuthorityChanges()
		step, err := u.step(ctx)
		if err != nil || step.retired {
			return err
		}
		if step.acquisitionError != nil {
			slog.Warn("follower root anchor acquisition failed; new attempt scheduled", "next", step.next)
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
