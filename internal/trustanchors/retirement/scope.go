package retirement

import (
	"context"
	"sync/atomic"
	"time"
)

type lifetime struct {
	original context.Context
	done     <-chan struct{}
	deadline time.Time
	at       time.Time
	until    time.Time
}

func captureLifetime(ctx context.Context) (*lifetime, error) {
	if ctx == nil {
		return nil, ErrInvalid
	}
	// All context-interface calls happen here or at the unlocked outward error
	// boundary. Cached Done/deadline are the only context inputs to final hooks.
	deadline, ok := ctx.Deadline()
	done := ctx.Done()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrInvalid
	}
	at := time.Now()
	remaining := deadline.Sub(at)
	if remaining <= 0 {
		return nil, context.DeadlineExceeded
	}
	life := &lifetime{original: ctx, done: done, deadline: deadline, at: at, until: at.Add(min(remaining, MaxLifetime))}
	if err := life.check(); err != nil {
		return nil, stopped(err, life)
	}
	return life, nil
}

func (l *lifetime) check() error {
	select {
	case <-l.done:
		return errOriginalDone
	default:
	}
	now := time.Now()
	if now.UTC().Before(l.at.UTC()) || !now.UTC().Before(l.deadline.UTC()) || !now.Before(l.until) {
		return context.DeadlineExceeded
	}
	return nil
}

type attempt struct {
	used     atomic.Bool
	gate     *Gate
	life     *lifetime
	snapshot *Snapshot
	expected Mark
}

// ReadAttempt is one original acquisition; copying it shares its consumed cell.
type ReadAttempt struct{ cell *attempt }

// WriteAttempt retains the exact original expected cursor, never a successor.
type WriteAttempt struct{ cell *attempt }

// CaptureRead retains only an already-issued mechanical registration. It grants
// no source/response authority and allocates no gate-resident pending slot.
func (g *Gate) CaptureRead(ctx context.Context, snapshot *Snapshot) (*ReadAttempt, error) {
	if !g.usable() || snapshot == nil || snapshot.gate != g {
		return nil, ErrInvalid
	}
	life, err := captureLifetime(ctx)
	if err != nil {
		return nil, err
	}
	return &ReadAttempt{cell: &attempt{gate: g, life: life, snapshot: snapshot}}, nil
}

// CaptureWrite must precede the future owner's acquisition/effect. No context
// or expected-version replacement is accepted by WithWrite after capture.
func (g *Gate) CaptureWrite(ctx context.Context, expected Mark) (*WriteAttempt, error) {
	if !g.usable() || (expected != (Mark{}) && !validMark(expected)) {
		return nil, ErrInvalid
	}
	life, err := captureLifetime(ctx)
	if err != nil {
		return nil, err
	}
	return &WriteAttempt{cell: &attempt{gate: g, life: life, expected: expected}}, nil
}

type readLease struct {
	gate   *Gate
	life   *lifetime
	active atomic.Bool
}

type writeLease struct {
	gate *Gate
	life *lifetime
}

// ReadScope is a borrowed lock hold, not a response or a manual unlock token.
type ReadScope struct{ lease *readLease }

// WriteScope is live only while the Gate's single CAS state names this lease.
// Scope closure and every plan/cursor mutation compare that SAME atomic state.
type WriteScope struct{ lease *writeLease }

// Check performs memory/time work only. A cached original Done signal returns
// an opaque interruption marker; WithRead classifies it through original.Err
// only after releasing exclusion. Return this error unchanged from final hooks.
func (s ReadScope) Check() error {
	if s.lease == nil || !s.lease.active.Load() {
		return ErrScope
	}
	if s.lease.gate.state.Load().closed {
		return ErrClosed
	}
	return s.lease.life.check()
}

// Check performs no context-interface call, lock acquisition, callback, or I/O.
// WithWrite classifies an opaque original interruption only after scope release.
func (s *WriteScope) Check() error {
	if s == nil || s.lease == nil {
		return ErrScope
	}
	state := s.lease.gate.state.Load()
	if state.writer != s.lease {
		return ErrScope
	}
	if state.closed {
		return ErrClosed
	}
	return s.lease.life.check()
}

func (g *Gate) consume(cell *attempt) error {
	if cell == nil || cell.gate != g || !g.usable() {
		return ErrInvalid
	}
	if !cell.used.CompareAndSwap(false, true) {
		return ErrUsed
	}
	return cell.life.check()
}

// WithRead invokes fn at most once, synchronously. Cancellation never unlocks
// beneath an active callback; actual output must have its original bounded I/O.
func (g *Gate) WithRead(a *ReadAttempt, fn func(ReadScope) error) error {
	if a == nil {
		return ErrInvalid
	}
	if err := g.consume(a.cell); err != nil {
		if a.cell != nil {
			return stopped(err, a.cell.life)
		}
		return err
	}
	if fn == nil {
		return ErrInvalid
	}
	if err := g.enter(false, a.cell.life); err != nil {
		return stopped(err, a.cell.life)
	}
	lease := &readLease{gate: g, life: a.cell.life}
	lease.active.Store(true)
	var result error
	func() {
		defer g.release(false)
		defer lease.active.Store(false)
		state := g.state.Load()
		if state.closed {
			result = ErrClosed
		} else if state.current != a.cell.snapshot || state.current.frame.Mark != state.installed || state.installed.EnvelopeGeneration <= state.floor {
			result = ErrRetired
		} else if result = (ReadScope{lease: lease}).Check(); result == nil {
			result = fn(ReadScope{lease: lease})
			if result == nil {
				result = (ReadScope{lease: lease}).Check()
			}
		}
	}()
	return stopped(result, a.cell.life)
}

// Effect describes a reported mechanical effect, never a durable/source receipt.
type Effect uint8

// PublicationApplied is intentionally distinct from confirmed publication.
const (
	NoEffect Effect = iota
	InstalledEffect
	PublishedEffect
	DeniedEffect
	PublicationAppliedEffect
	UnknownEffect
)

// EffectError retains completed/uncertain effects despite an outer callback
// refusal. It must not be treated as rollback or a reason to replay an effect.
type EffectError struct {
	effect Effect
	cause  error
}

func (e *EffectError) Error() string {
	return "retirement: effect completed or uncertain; callback refused"
}

// Unwrap preserves the original refusal category without disclosing it in Error.
func (e *EffectError) Unwrap() error { return e.cause }

// Effect is the original scope's mechanical outcome, not authority to grant.
func (e *EffectError) Effect() Effect { return e.effect }

// WithWrite releases only after one common CAS has ended this scope and retained
// its exact effect/cursor. A canceled completed effect is never made rollback.
func (g *Gate) WithWrite(a *WriteAttempt, fn func(*WriteScope) error) error {
	if a == nil {
		return ErrInvalid
	}
	if err := g.consume(a.cell); err != nil {
		if a.cell != nil {
			return stopped(err, a.cell.life)
		}
		return err
	}
	if fn == nil {
		return ErrInvalid
	}
	if err := g.enter(true, a.cell.life); err != nil {
		return stopped(err, a.cell.life)
	}
	effect, result := g.runWrite(a.cell, fn)
	// runWrite has closed the scope AND released counted exclusion before any
	// arbitrary context-interface method is used to classify the original Done.
	result = stopped(result, a.cell.life)
	if effect != NoEffect && result != nil {
		result = &EffectError{effect: effect, cause: result}
	}
	return result
}

func (g *Gate) runWrite(cell *attempt, fn func(*WriteScope) error) (effect Effect, result error) {
	lease := &writeLease{gate: g, life: cell.life}
	defer g.release(true)
	for {
		old := g.state.Load()
		if old.closed {
			return NoEffect, ErrClosed
		}
		if old.installed != cell.expected {
			return NoEffect, ErrMismatch
		}
		if err := lease.life.check(); err != nil {
			return NoEffect, err
		}
		next := *old
		next.writer, next.plan = lease, planState{}
		if g.state.CompareAndSwap(old, &next) {
			break
		}
	}
	defer func() {
		// This CAS is the scope-close linearization, BEFORE queue release. Every
		// escaped mutation compares the same writer pointer in this same state.
		for {
			old := g.state.Load()
			if old.writer != lease {
				result = ErrScope
				break
			}
			next := *old
			effect = old.plan.effect
			if old.closed && result == nil {
				result = ErrClosed
			}
			if old.plan.token != nil && (!old.plan.resolved || old.plan.invalid) {
				next.closed, next.current = true, nil
				if effect == NoEffect {
					effect = UnknownEffect
				}
				if result == nil {
					result = ErrClosed
				}
			}
			next.writer, next.plan = nil, planState{}
			if g.state.CompareAndSwap(old, &next) {
				break
			}
		}
		if effect == NoEffect && result == nil {
			result = lease.life.check()
		}
	}()
	scope := &WriteScope{lease: lease}
	if result = scope.Check(); result == nil {
		result = fn(scope)
	}
	return effect, result
}
