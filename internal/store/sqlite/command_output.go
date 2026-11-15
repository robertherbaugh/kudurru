package sqlite

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// ErrCommandCompletionUnknown means the SQL driver could not confirm the
// command's disposition. It must never be treated as rollback or retried.
var ErrCommandCompletionUnknown = errors.New("store/sqlite: command completion unknown")

// CommandOutputRequest binds one command and its synchronous response to an
// original Store-issued serving cut. Capture it before body acquisition. It
// grants no domain permission: the command must reverify original raw proof.
// Copies share consumption. Output is available only inside CommandWithOutput,
// after confirmed completion and before releasing the actual writer exclusion.
type CommandOutputRequest struct{ original *commandOutputRequest }

type commandOutputRequest struct {
	s         *Store
	ctx       context.Context
	token     GenerationToken
	deadline  time.Time
	state     atomic.Uint32
	committed atomic.Bool
	completed atomic.Pointer[replication.Authority]
}

const (
	commandOutputFresh uint32 = iota
	commandOutputActive
	commandOutputEmitting
	commandOutputTerminal
)

type commandOutputKey struct{}
type commandOutputScopeKey struct{}
type commandOutputInvocation struct {
	request *commandOutputRequest
	emit    func(context.Context) error
}

// CaptureCommandOutput retains the supplied original cut and finite request.
// It performs no replacement source acquisition. The real transaction repeats
// source admission under wmu before invoking any command callback.
func (s *Store) CaptureCommandOutput(ctx context.Context, token GenerationToken) (*CommandOutputRequest, error) {
	if ctx == nil || token.source != s || !token.authority.Writable() || token.DNSOnly() {
		return nil, replication.ErrInvalid
	}
	deadline, finite := ctx.Deadline()
	now := time.Now()
	if !finite || !now.Before(deadline) || deadline.Sub(now) > 30*time.Second {
		return nil, replication.ErrInvalid
	}
	o := &commandOutputRequest{s: s, ctx: ctx, token: token, deadline: deadline}
	if err := o.check(); err != nil {
		return nil, err
	}
	if !token.Matches(s.Authority()) {
		return nil, replication.ErrCAS
	}
	return &CommandOutputRequest{original: o}, nil
}

func (r *commandOutputRequest) check() error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if !time.Now().Before(r.deadline) {
		return context.DeadlineExceeded
	}
	a, blocked := r.s.ServingAuthority()
	b := r.token.authority
	if blocked || !a.Writable() || a.Mode != b.Mode || a.MemberID != b.MemberID || a.Incarnation != b.Incarnation || !a.Term.Same(b.Term) {
		return replication.ErrFenced
	}
	return nil
}

func (r *commandOutputRequest) lock(ctx context.Context) error {
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.check(); err != nil {
			return err
		}
		if r.s.wmu.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// CommandWithOutput executes once, then emits only after the actual SQL
// completion. committed is independent of subsequent output refusal/error.
// The original writer must already be carried in ctx; no writer is recaptured.
// All dependency pins must enclose this call. emit may perform bounded transport
// I/O only, explicitly flushing under the supplied deadline and joining any
// cancellation callbacks. It must never call Store, acquire dependency locks,
// spawn detached work, or retain the output context for later use.
func (s *Store) CommandWithOutput(ctx context.Context, request *CommandOutputRequest, fn func(context.Context, *CommandTx) error, emit func(context.Context) error) (bool, error) {
	if ctx == nil || request == nil || request.original == nil || fn == nil || emit == nil {
		return false, replication.ErrInvalid
	}
	o := request.original
	if o.s != s || !o.state.CompareAndSwap(commandOutputFresh, commandOutputActive) {
		return false, replication.ErrInvalid
	}
	defer o.state.Store(commandOutputTerminal)
	writer, ok := WriterTokenFromContext(ctx)
	a := o.token.authority
	if !ok || writer != (WriterToken{a.MemberID, a.Term, a.Incarnation}) {
		return false, replication.ErrFenced
	}
	ctx, cancel := context.WithDeadline(ctx, o.deadline)
	done := make(chan struct{})
	stop := context.AfterFunc(o.ctx, func() { cancel(); close(done) })
	defer func() {
		if !stop() {
			<-done
		}
		cancel()
	}()
	ctx = context.WithValue(ctx, commandOutputKey{}, &commandOutputInvocation{request: o, emit: emit})
	err := s.Command(ctx, a.Term, fn)
	return o.committed.Load(), err
}

// CheckOutput is a memory-only authentication of this request's active output
// callback. Plain, foreign, escaped, canceled and failed-command contexts refuse.
// It does not establish permission from caller-supplied completion information.
func (r *CommandOutputRequest) CheckOutput(ctx context.Context) error {
	if r == nil || r.original == nil || ctx == nil {
		return replication.ErrInvalid
	}
	o := r.original
	if ctx.Value(commandOutputScopeKey{}) != o || o.state.Load() != commandOutputEmitting || !o.committed.Load() {
		return replication.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, finite := ctx.Deadline()
	if !finite || deadline.After(o.deadline) || !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	a := o.completed.Load()
	if a == nil || !reflect.DeepEqual(*a, o.s.Authority()) {
		return replication.ErrCAS
	}
	return o.check()
}

// finish runs in Command's finalizer, after the SQL disposition is resolved and
// while wmu is still held. No proposal or callback return can manufacture this
// completion receipt. Even no-ops require confirmed transaction closure.
func (o *commandOutputInvocation) finish(ctx context.Context, succeeded bool, boundary anchorSQL, result *error) {
	if boundary.outcome == AnchorUnknown {
		*result = errors.Join(*result, ErrCommandCompletionUnknown)
	}
	if !succeeded || *result != nil || (boundary.outcome != AnchorCommitted && boundary.outcome != AnchorUnchanged) {
		return
	}
	r := o.request
	r.committed.Store(true)
	a := r.s.Authority()
	r.completed.Store(&a)
	if err := r.check(); err != nil {
		*result = err
		return
	}
	if err := ctx.Err(); err != nil {
		*result = err
		return
	}
	r.state.Store(commandOutputEmitting)
	defer r.state.Store(commandOutputTerminal)
	ctx = context.WithValue(ctx, commandOutputScopeKey{}, r)
	if err := o.emit(ctx); err != nil {
		*result = err
		return
	}
	*result = (&CommandOutputRequest{original: r}).CheckOutput(ctx)
}
