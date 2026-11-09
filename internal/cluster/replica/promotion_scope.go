package replica

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// PromotionAuthorizationScope verifies the original operator proof and holds
// every lower policy dependency through one synchronous apply call. Coordinator
// already holds its queue and real serving publication pin. The callback cannot
// reenter either, refresh the source, perform network work or retain apply.
// It must join all work before returning; check uses only pinned memory/time.
// The actor and reason passed to apply must come from that verified operation.
type PromotionAuthorizationScope func(context.Context, PromoteRequest, func(func(context.Context) error, sqlite.AuditEntry) error) error

func (c *Coordinator) promoteScoped(ctx context.Context, req PromoteRequest) (replication.Authority, error) {
	served, release, err := c.opts.Served(ctx)
	if release != nil {
		defer release()
	}
	if err != nil || release == nil {
		return replication.Authority{}, errors.Join(ErrIneligible, err)
	}
	// Copies of apply share one consumption state. A call after root's scope
	// returns cannot start or finish a transition under released dependencies.
	var state atomic.Uint32
	var result replication.Authority
	var applyErr error
	err = c.opts.WithOperator(ctx, req, func(check func(context.Context) error, audit sqlite.AuditEntry) error {
		if !state.CompareAndSwap(0, 1) {
			return ErrForbidden
		}
		applyErr = func() error {
			if check == nil {
				return ErrForbidden
			}
			original := func(ctx context.Context) error {
				if state.Load() != 1 {
					return ErrForbidden
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				return check(ctx)
			}
			if err := original(ctx); err != nil {
				return err
			}
			if req.ConfirmNode != c.opts.NodeName {
				return ErrIneligible
			}
			a := c.opts.Store.Authority()
			storeRequest := replication.PromoteRequest{ExpectedTerm: req.ExpectedTerm, ExpectedInstalled: req.ExpectedInstalled, RequestID: req.RequestID}
			duplicate := a.Mode == replication.Primary && a.LastPromotionRequest == req.RequestID && a.Parent != nil && a.Parent.Same(req.ExpectedInstalled) && a.Parent.Term.Same(req.ExpectedTerm)
			if !duplicate {
				status := c.statusLocked(ctx, served, nil)
				if !status.Eligible {
					return ErrIneligible
				}
				if !status.Authority.Term.Same(req.ExpectedTerm) || !status.Installed.Same(req.ExpectedInstalled) {
					return replication.ErrCAS
				}
			}
			var err error
			result, err = c.opts.Store.PromoteAudited(ctx, storeRequest, original, audit)
			return err
		}()
		return applyErr
	})
	used := state.Swap(2)
	if used == 0 {
		return replication.Authority{}, errors.Join(ErrForbidden, err)
	}
	return result, errors.Join(err, applyErr)
}
