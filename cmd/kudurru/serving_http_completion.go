package main

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// currentAuthResponseCommand admits fn through the unchanged original command
// gates. Only actual successful completion returns a response-currency check.
// That check retains the original writer and dependencies, but not the database
// revision retired by this command's own commit. It grants no authentication,
// provider I/O or mutation: callers must observe their exact committed receipt
// in a complete publication before sealing a response. Keep the HTTP lease alive.
func (h *servingHTTP) currentAuthResponseCommand(ctx context.Context, fn func(context.Context, *sqlite.CommandTx, servingauth.Dependencies) error) (func() bool, error) {
	owner, err := servinghttp.Component[*servingbuild.RuntimeOwner](ctx, "runtime")
	if err != nil {
		return nil, err
	}
	s := h.opts.Runtime
	original := s.primary.writer.active.Load()
	var target serving.TermActivation
	current := func() bool {
		if ctx.Err() != nil || !s.primary.writer.current(original) || !owner.DependenciesCurrent() {
			return false
		}
		a, blocked := s.opts.Store.ServingAuthority()
		return !blocked && a.Writable() && a.Term.Same(target.Term) && a.Incarnation == target.Incarnation
	}
	_, err = s.currentAuthCommandThen(ctx, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		if !s.primary.writer.current(original) {
			return serving.ErrSuperseded
		}
		return fn(ctx, tx, deps)
	}, func(_ context.Context, committed serving.TermActivation) error {
		target = committed
		if !current() {
			return serving.ErrSuperseded
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return current, nil
}
