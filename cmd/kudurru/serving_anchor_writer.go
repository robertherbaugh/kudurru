package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providercoordinator"
	"github.com/robertherbaugh/kudurru/internal/serving"
)

// This state belongs to the runtime, not a writerEpisode or one invocation of
// RunWriter. Both the local host loop and cluster Runner can restart those
// callers within the same elected term. Neither may erase a terminal poll.
type anchorWriterControl struct {
	mu       sync.Mutex
	target   serving.TermActivation
	running  *anchorWriterRun
	terminal error
}

type anchorWriterRun struct {
	owner   *anchorWriterControl
	acquire bool
}

func (c *anchorWriterControl) begin(ctx context.Context, target serving.TermActivation) (*anchorWriterRun, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running != nil {
		return nil, errAnchorPolicy // the previous borrowed host must be joined
	}
	if !c.target.Term.Same(target.Term) || c.target.Incarnation != target.Incarnation {
		c.target, c.terminal = target, nil
	}
	run := &anchorWriterRun{owner: c, acquire: c.terminal == nil}
	c.running = run
	return run, nil
}

// terminalError is the retained terminal result of the current target.
func (c *anchorWriterControl) terminalError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.terminal
}

func (r *anchorWriterRun) stopped(err error) {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if r.owner.running == r {
		r.owner.terminal = err
	}
}

func (r *anchorWriterRun) release() {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if r.owner.running == r {
		r.owner.running = nil
	}
}

// runAnchorWriter is mounted only after root topology admission. Its caller
// retains the borrowed host until this returns. Terminal failure parks only
// this child, preserving other writer work
// and preventing either outer retry loop from replaying the same elected source.
// A real new elected term/incarnation may acquire again after the old child joins.
func (s *servingRuntime) runAnchorWriter(ctx context.Context, target serving.TermActivation, host providercoordinator.HostCapture) error {
	return s.runAnchorWriterReady(ctx, target, host, func() {})
}

// ready ends only the bounded first-attempt startup phase. It is not a success
// acknowledgment: both terminal refusal and transport cooldown release other
// writer services, while the original anchor result remains retained.
func (s *servingRuntime) runAnchorWriterReady(ctx context.Context, target serving.TermActivation, host providercoordinator.HostCapture, ready func()) error {
	signal := sync.OnceFunc(ready)
	defer signal()
	if s == nil || ctx == nil {
		return errAnchorPolicy
	}
	run, err := s.anchorWriter.begin(ctx, target)
	if err != nil {
		return err
	}
	defer run.release()
	// The terminal record outlives this child; the board shows the worker as
	// ended once its writer context ends, and keeps the last error.
	defer s.anchorStatus.setUpdater(anchorWorkerNotRunning, time.Time{}, nil)
	if run.acquire {
		s.anchorStatus.setUpdater(anchorWorkerRunning, time.Time{}, nil)
		if s.anchors != nil && s.anchors.admission != nil {
			err = s.prepareAnchorWriter(ctx, target, host)
		}
		if err == nil {
			err = s.runAnchorUpdaterReady(ctx, target, host, signal)
		}
		var terminal *anchorTerminalFailure
		if !errors.As(err, &terminal) && ctx.Err() != nil && (err == nil || errors.Is(err, ctx.Err())) {
			return ctx.Err() // owner cancellation joins; no original input is retained
		}
		if err == nil {
			err = errAnchorPolicy // an unexpected early completion cannot enable retry
		}
		run.stopped(err)
		s.anchorStatus.setUpdater(anchorWorkerStopped, time.Time{}, err)
		slog.Error("root anchor updater stopped for this writer term", "err", err)
	} else {
		s.anchorStatus.setUpdater(anchorWorkerStopped, time.Time{}, s.anchorWriter.terminalError())
	}
	signal()
	// Returning the terminal error would make cluster Runner restart the whole
	// writer. Keep the original child joined to its caller instead, while the
	// runtime-owned terminal record survives host/caller replacement.
	<-ctx.Done()
	return ctx.Err()
}
