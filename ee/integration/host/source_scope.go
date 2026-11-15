package host

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
)

type sourceScopeKey struct{}

type sourceScope struct {
	host      *Host
	job       *job
	guard     ExecutionGuard
	instance  *Instance
	original  context.Context
	lease     context.Context
	deadline  time.Time
	requestID string
	allowed   func() bool
	active    atomic.Bool
	used      atomic.Bool
}

// SourceCapture is an opaque capability for one original observation. Its
// lifetime includes acquisition, commit and acknowledgment; it is not durable.
// Copies retain the same lifetime and single WithSource invocation.
type SourceCapture struct{ scope *sourceScope }

// Guard returns the original execution identity without sampling current state.
func (c SourceCapture) Guard() ExecutionGuard {
	if c.scope == nil {
		return ExecutionGuard{}
	}
	return c.scope.guard
}

// RequestID identifies the observation, independently of HTTP budget attempts.
func (c SourceCapture) RequestID() string {
	if c.scope == nil {
		return ""
	}
	return c.scope.requestID
}

// Deadline returns the original observation deadline, which cannot be renewed.
func (c SourceCapture) Deadline() time.Time {
	if c.scope == nil {
		return time.Time{}
	}
	return c.scope.deadline
}

func (h *Host) beginSource(ctx context.Context, j *job, g ExecutionGuard) (context.Context, *sourceScope, context.CancelFunc, error) {
	request, ok := ctx.Value(scopeKey{}).(requestScope)
	if !ok || request.j != j || request.g != g || request.original == nil {
		return nil, nil, nil, ErrFenced
	}
	ctx, cancel := context.WithTimeout(ctx, h.opts.OperationTimeout)
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.jobs[j.key] != j || !j.active || j.leaseCtx == nil || j.ctx.Err() != nil || request.original.lease.Err() != nil || j.guard != g.Ledger || j.frame.input.BundleID != g.BundleID || j.frame.instance != request.original.instance || j.leaseCtx != request.original.lease || !h.opts.Allowed() {
		cancel()
		return nil, nil, nil, ErrFenced
	}
	deadline, _ := ctx.Deadline()
	s := &sourceScope{host: h, job: j, guard: g, instance: request.original.instance, original: ctx, lease: request.original.lease, deadline: deadline, allowed: h.opts.Allowed}
	s.requestID = kinds.Digest([]byte(fmt.Sprintf("%s/%s/%s/%d", h.incarnation, g.Ledger.Owner.UID, g.Ledger.LeaseID, h.sourceSequence.Add(1))))
	s.active.Store(true)
	stopJob := context.AfterFunc(j.ctx, cancel)
	stopLease := context.AfterFunc(s.lease, cancel)
	closeScope := func() {
		s.active.Store(false)
		stopJob()
		stopLease()
		cancel()
	}
	return context.WithValue(ctx, sourceScopeKey{}, s), s, closeScope, nil
}

// SourceCapture requires the private original context created before Observe.
// It never adopts a replacement frame, elector context or guard.
func (h *Host) SourceCapture(ctx context.Context, expected ExecutionGuard) (SourceCapture, error) {
	s, ok := ctx.Value(sourceScopeKey{}).(*sourceScope)
	if !ok || s == nil || s.host != h || s.guard != expected {
		return SourceCapture{}, ErrFenced
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if err := h.sourceCurrent(ctx, s); err != nil {
		return SourceCapture{}, err
	}
	return SourceCapture{scope: s}, nil
}

// sourceCurrent is called only while the job replacement lock is held. It
// performs no I/O, takes no locks and does not invoke the external Fence hook.
// Root holds its own original license/resource/publication exclusion separately.
func (h *Host) sourceCurrent(ctx context.Context, s *sourceScope) error {
	if err := sourceLifetime(ctx, s); err != nil {
		return err
	}
	j := s.job
	if h.jobs[j.key] != j || !j.active || j.guard != s.guard.Ledger || j.frame.instance != s.instance || j.frame.input.BundleID != s.guard.BundleID || j.leaseCtx != s.lease || !h.opts.Allowed() {
		return ErrFenced
	}
	return nil
}

// Lifetime checks use only immutable original references, so even a retained
// final predicate cannot race replacement after its callback has returned.
func sourceLifetime(ctx context.Context, s *sourceScope) error {
	if !s.active.Load() {
		return ErrFenced
	}
	if err := s.original.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !time.Now().Before(s.deadline) {
		return context.DeadlineExceeded
	}
	if s.job.ctx.Err() != nil || s.lease.Err() != nil || !s.allowed() {
		return ErrFenced
	}
	return nil
}

// WithSource retains original job replacement exclusion through one synchronous
// callback. Root composes its own original proof/writer exclusions outside this
// method, then registers final in the actual CommandTx (including no-op replay).
// Do not reenter Host lock-taking methods or retain the callback/final/context.
// final checks only original memory, cancellation and clock; it is invalid as
// soon as the callback returns. Callback errors are returned unchanged.
func (h *Host) WithSource(ctx context.Context, c SourceCapture, fn func(context.Context, func(context.Context) error) error) error {
	s := c.scope
	if fn == nil {
		return ErrInvalid
	}
	if s == nil || s.host != h || ctx.Value(sourceScopeKey{}) != s {
		return ErrFenced
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if err := h.sourceCurrent(ctx, s); err != nil {
		return err
	}
	if !s.used.CompareAndSwap(false, true) {
		return ErrFenced
	}
	commitCtx, cancel := context.WithDeadline(ctx, s.deadline)
	stopOriginal := context.AfterFunc(s.original, cancel)
	defer stopOriginal()
	defer cancel()
	var active atomic.Bool
	active.Store(true)
	defer active.Store(false)
	final := func(finalCtx context.Context) error {
		if !active.Load() {
			return ErrFenced
		}
		if err := commitCtx.Err(); err != nil {
			return err
		}
		return sourceLifetime(finalCtx, s)
	}
	if err := final(commitCtx); err != nil {
		return err
	}
	return fn(commitCtx, final)
}
