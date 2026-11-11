package providercoordinator

import (
	"context"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// WithCommandOutput retains publication, provider resources, host dependencies
// and the actual Store writer exclusion through confirmed completion AND output.
// Root supplies its original carried writer and reauthorizes raw proof in fn.
// emit must obey CommandWithOutput's synchronous bounded transport contract;
// root also retains the verified credential lifetime through emission.
func (c *Coordinator) WithCommandOutput(ctx context.Context, target serving.TermActivation, request *sqlite.CommandOutputRequest, fn CurrentCommandFunc, emit func(context.Context) error) (committed bool, result error) {
	if ctx == nil || request == nil || fn == nil || emit == nil {
		return false, ErrInvalid
	}
	deadline, finite := ctx.Deadline()
	now := time.Now()
	if !finite || !now.Before(deadline) || deadline.Sub(now) > 30*time.Second {
		return false, ErrInvalid
	}
	ctx, stop, err := c.context(ctx)
	if err != nil {
		return false, err
	}
	defer stop()
	if err := c.writerCurrent(ctx, target); err != nil {
		return false, err
	}
	var host HostCapture
	defer func() { releaseHost(host) }()
	err = c.opts.Publisher.WithWriterOutputGate(ctx, target, func() error {
		ctx, done, err := c.track(ctx)
		if err != nil {
			return err
		}
		defer done()
		return c.opts.Resources.WithCurrent(ctx, func(pin *providerresources.Pinned) error {
			return c.opts.PinHost(ctx, func() error {
				until, err := pin.Deadline()
				if err != nil {
					return err
				}
				if !until.IsZero() && until.Before(deadline) {
					deadline = until
				}
				operation, cancel := context.WithDeadline(ctx, deadline)
				defer cancel()
				committed, err = c.opts.Store.CommandWithOutput(operation, request, func(ctx context.Context, tx *sqlite.CommandTx) error {
					registry, err := registryCurrent(ctx, tx)
					if err != nil {
						return err
					}
					host, err = c.host(ctx, registry)
					if err != nil {
						return err
					}
					deps, err := pin.DependenciesCurrent(ctx, tx, host.Providers)
					if err != nil {
						return err
					}
					if err := c.checkHost(ctx, tx, host, target); err != nil {
						return err
					}
					auth := host.Auth
					auth.Identity = identity(host.Identity, deps.Identity)
					auth.ExpectedIdentity = auth.Identity
					auth.ConfigProviders, auth.ProviderResources = deps.ConfigProviders, deps.ProviderResources
					return fn(ctx, tx, auth)
				}, func(scope context.Context) error {
					if !host.ValidUntil.IsZero() && host.ValidUntil.Before(deadline) {
						deadline = host.ValidUntil
					}
					output, cancel := context.WithDeadline(scope, deadline)
					defer cancel()
					check := func() error {
						if err := request.CheckOutput(output); err != nil {
							return err
						}
						if err := c.writerCurrent(output, target); err != nil {
							return err
						}
						if !c.hostCurrent(host) {
							return ErrStale
						}
						return pin.CurrentAt(c.opts.Now())
					}
					if err := check(); err != nil {
						return err
					}
					if err := emit(output); err != nil {
						return err
					}
					return check()
				})
				return err
			})
		})
	})
	return committed, err
}
