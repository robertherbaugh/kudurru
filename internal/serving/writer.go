package serving

import (
	"context"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// WithWriterGate holds the publication/promotion gate for an exact admitted
// writer, including during startup or while a newer complete bundle is pending.
// It does not provide a serving lease or imply that HTTP or DNS owners are ready.
//
// The callback must pin CURRENT dependency replacement next, then use the actual
// fenced Store.Command. Retain both gates through the whole command; never wait
// for publication, perform network I/O, or reenter a publication pin inside it.
// An outside authority sample cannot replace the transaction's final fence.
// The callback runs synchronously once and its result is returned unchanged.
func (p *Publisher) WithWriterGate(ctx context.Context, target TermActivation, fn func() error) error {
	return p.withWriterGate(ctx, target, fn)
}

// WithWriterOutputGate retains publication exclusion through a bounded command
// and its synchronous response. Unlike WithWriterGate, it permits transport
// output, with an original finite deadline of at most 30 seconds. Callers must
// retain dependency and Store-issued output admission in that order, install
// the earlier actual deadline, interrupt canceled I/O and flush before return.
// No publication wait, nested pin, acquisition or replacement is permitted.
// This gate supplies neither domain permission nor proof of command completion.
func (p *Publisher) WithWriterOutputGate(ctx context.Context, target TermActivation, fn func() error) error {
	if ctx == nil {
		return replication.ErrInvalid
	}
	deadline, finite := ctx.Deadline()
	now := time.Now()
	if !finite || !now.Before(deadline) || deadline.Sub(now) > 30*time.Second {
		return replication.ErrInvalid
	}
	return p.withWriterGate(ctx, target, fn)
}

func (p *Publisher) withWriterGate(ctx context.Context, target TermActivation, fn func() error) error {
	if fn == nil || target.Term.Validate() != nil || target.Incarnation == 0 {
		return replication.ErrInvalid
	}
	if err := p.lock(ctx); err != nil {
		return err
	}
	defer p.unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.closed.Load() {
		return ErrClosed
	}
	authority, blocked := p.opts.Source.ServingAuthority()
	if blocked || !authority.Writable() || !authority.Term.Same(target.Term) || authority.Incarnation != target.Incarnation {
		return ErrSuperseded
	}
	return fn()
}
