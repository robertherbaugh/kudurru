package main

import (
	"context"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// primaryWriter retains the original elector context through one joined driver
// run. Binding a request never obtains a fresh Store.WriterToken or adopts a
// successor. Complete serving eligibility and driver readiness are separate
// checks at ingress; current transaction admission remains mandatory.
type primaryWriter struct {
	store  *sqlite.Store
	active atomic.Pointer[primaryWriterTerm]
}

type primaryWriterTerm struct {
	ctx   context.Context
	token sqlite.WriterToken
	fence primaryruntime.PublicationFence
}

func (w *primaryWriter) run(ctx context.Context, run func(context.Context, primaryruntime.PublicationFence) error) error {
	token, ok := sqlite.WriterTokenFromContext(ctx)
	if w == nil || w.store == nil || !ok || run == nil || ctx.Err() != nil {
		return primaryruntime.ErrUnavailable
	}
	t := &primaryWriterTerm{ctx: ctx, token: token, fence: primaryruntime.PublicationFence{Term: token.Term(), Incarnation: token.Incarnation()}}
	if !w.active.CompareAndSwap(nil, t) {
		return primaryruntime.ErrConflict
	}
	defer w.active.CompareAndSwap(t, nil)
	if !w.current(t) {
		return primaryruntime.ErrUnavailable
	}
	// run must join its workers before returning. Driver.Run supplies that
	// guarantee for its NOTIFY worker and restore/reconciliation loop.
	return run(primaryruntime.WithPublicationFence(ctx, t.fence), t.fence)
}

func (w *primaryWriter) current(t *primaryWriterTerm) bool {
	if w == nil || w.store == nil || t == nil || t.ctx.Err() != nil || w.active.Load() != t {
		return false
	}
	a, blocked := w.store.ServingAuthority()
	return !blocked && a.Writable() && a.Term.Same(t.fence.Term) && a.Incarnation == t.fence.Incarnation
}

// bind preserves the native request's values, deadline and cancellation, and
// adds cancellation from the exact elected driver context. Its stop hook must
// be called after the complete operation, including publication acknowledgment.
func (w *primaryWriter) bind(request context.Context, want primaryruntime.PublicationFence) (context.Context, func(), error) {
	if w == nil || request == nil || request.Err() != nil {
		return nil, nil, primaryruntime.ErrUnavailable
	}
	t := w.active.Load()
	if !w.current(t) || !t.fence.Term.Same(want.Term) || t.fence.Incarnation != want.Incarnation {
		return nil, nil, primaryruntime.ErrUnavailable
	}
	ctx, cancel := context.WithCancel(request)
	stop := context.AfterFunc(t.ctx, cancel)
	release := func() { stop(); cancel() }
	if !w.current(t) || ctx.Err() != nil {
		release()
		return nil, nil, primaryruntime.ErrUnavailable
	}
	return primaryruntime.WithPublicationFence(sqlite.WithWriterToken(ctx, t.token), t.fence), release, nil
}
