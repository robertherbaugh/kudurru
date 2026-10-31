package api

import (
	"context"
	"time"
)

// BlockingChange is a detached, fixed-size request to update canonical blocking
// status. Presence flags preserve explicit zero values without retaining input.
type BlockingChange struct {
	Blocking     bool
	HasTimer     bool
	TimerSeconds float64
	HasUntil     bool
	Until        time.Time
}

// BlockingCommand admits one original current command and acknowledges its
// complete serving publication. A committed but unacknowledged change is an error
// with an explicit committed receipt, never a claim that the write rolled back.
type BlockingCommand func(context.Context, BlockingChange) (*BlockingState, error)

func (s *Server) setBlockingCommand(ctx context.Context, in *setBlockingInput) (*blockingOutput, error) {
	if s.opts.BlockingCommand == nil {
		return nil, errUnavailable("canonical blocking command unavailable")
	}
	if ctx == nil || ctx.Err() != nil || in == nil {
		return nil, errUnavailable("original blocking command unavailable")
	}
	change := BlockingChange{Blocking: in.Body.Blocking}
	if in.Body.Timer != nil {
		change.HasTimer, change.TimerSeconds = true, *in.Body.Timer
	}
	if in.Body.Until != nil {
		change.HasUntil, change.Until = true, *in.Body.Until
	}
	state, err := s.opts.BlockingCommand(ctx, change)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, errUnavailable("canonical blocking command result unavailable")
	}
	// Detach the capability result before adding API-owned response metadata.
	result := *state
	if state.Timer != nil {
		v := *state.Timer
		result.Timer = &v
	}
	if state.Until != nil {
		v := *state.Until
		result.Until = &v
	}
	result.Node = s.opts.NodeName
	result.Scope = "cluster"
	return &blockingOutput{Body: &result}, nil
}
