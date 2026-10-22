package providercoordinator

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func carriedWriter(ctx context.Context, target serving.TermActivation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t, ok := sqlite.WriterTokenFromContext(ctx)
	if !ok || target.Term.Validate() != nil || target.Incarnation == 0 || !t.Term().Same(target.Term) || t.Incarnation() != target.Incarnation {
		return replication.ErrFenced
	}
	return nil
}
func (c *Coordinator) writerCurrent(ctx context.Context, target serving.TermActivation) error {
	if err := carriedWriter(ctx, target); err != nil {
		return err
	}
	a, blocked := c.opts.Store.ServingAuthority()
	if blocked || !a.Writable() || !a.Term.Same(target.Term) || a.Incarnation != target.Incarnation {
		return replication.ErrFenced
	}
	return nil
}
func (c *Coordinator) checkHost(ctx context.Context, tx *sqlite.CommandTx, h HostCapture, target serving.TermActivation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !tx.Authority().Same(target.Term) || tx.Incarnation() != target.Incarnation {
		return replication.ErrFenced
	}
	return tx.RequireCommitCheck(func(ctx context.Context) error {
		if err := c.writerCurrent(ctx, target); err != nil {
			return err
		}
		if !c.hostCurrent(h) {
			return ErrStale
		}
		return nil
	})
}

// WithCommand holds publication -> resource -> actual SQLite through final
// validation/commit/no-op. It requires the original carried opaque writer token;
// it NEVER calls Store.WriterToken to sample a successor or mint authority.
// Root callback must perform current raw-proof auth before mutations/dedup.
func (c *Coordinator) WithCommand(ctx context.Context, target serving.TermActivation, fn CurrentCommandFunc) error {
	if fn == nil {
		return ErrInvalid
	}
	ctx, stop, err := c.context(ctx)
	if err != nil {
		return err
	}
	defer stop()
	if err := c.writerCurrent(ctx, target); err != nil {
		return err
	}
	var h HostCapture
	defer func() { releaseHost(h) }()
	return c.opts.Publisher.WithWriterGate(ctx, target, func() error {
		return c.opts.Resources.WithCurrent(ctx, func(p *providerresources.Pinned) error {
			return c.opts.PinHost(ctx, func() error {
				return c.opts.Store.Command(ctx, target.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
					r, err := registryCurrent(ctx, tx)
					if err != nil {
						return err
					}
					h, err = c.host(ctx, r)
					if err != nil {
						return err
					}
					d, err := p.DependenciesCurrent(ctx, tx, h.Providers)
					if err != nil {
						return err
					}
					if err := c.checkHost(ctx, tx, h, target); err != nil {
						return err
					}
					deps := h.Auth
					deps.Identity = identity(h.Identity, d.Identity)
					deps.ExpectedIdentity = deps.Identity
					deps.ConfigProviders, deps.ProviderResources = d.ConfigProviders, d.ProviderResources
					return fn(ctx, tx, deps)
				})
			})
		})
	})
}
