package providercoordinator

import (
	"context"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// WithMemberCommandOutput retains publication -> resource -> host exclusions
// through the original MemberCommand and its one Store-admitted response.
// committed means actual successful command completion (including a no-op),
// independently of later cancellation, output refusal or a transport error.
//
// Root must carry its original elected writer and reverify raw domain proof in
// fn before effects or dedup reads. Membership supplies no user permission.
// Root also retains the original credential bounds through emit. emit may only
// write and explicitly flush its prepared response under the supplied deadline;
// it must interrupt canceled I/O, never reenter Store, acquire locks, wait for
// publication or call MemberRequest.WithCommandOutput again. No callback may
// escape this synchronous scope. Authority/dependency replacements use their
// separate exclusive protocols, not this ordinary member-command path.
func (c *Coordinator) WithMemberCommandOutput(ctx context.Context, target serving.TermActivation, request *sqlite.MemberRequest, fn CurrentCommandFunc, emit func(context.Context) error) (committed bool, result error) {
	if request == nil || fn == nil || emit == nil || ctx == nil {
		return false, ErrInvalid
	}
	deadline, finite := ctx.Deadline()
	now := time.Now()
	if !finite || !now.Before(deadline) || deadline.Sub(now) > 30*time.Second {
		return false, ErrInvalid
	}
	if !target.Term.Same(request.OriginalTerm()) {
		return false, replication.ErrFenced
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
		return c.opts.Resources.WithCurrent(ctx, func(pin *providerresources.Pinned) error {
			return c.opts.PinHost(ctx, func() error {
				until, err := pin.Deadline()
				if err != nil {
					return err
				}
				for _, bound := range []time.Time{request.Deadline(), until} {
					if !bound.IsZero() && bound.Before(deadline) {
						deadline = bound
					}
				}
				operation, cancel := context.WithDeadline(ctx, deadline)
				defer cancel()
				err = c.opts.Store.MemberCommand(operation, request, func(ctx context.Context, tx *sqlite.CommandTx) error {
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
				})
				if err != nil {
					return err
				}
				committed = true
				if !host.ValidUntil.IsZero() && host.ValidUntil.Before(deadline) {
					deadline = host.ValidUntil
				}
				output, cancelOutput := context.WithDeadline(operation, deadline)
				defer cancelOutput()
				check := func() error {
					if err := c.writerCurrent(output, target); err != nil {
						return err
					}
					if !c.hostCurrent(host) {
						return ErrStale
					}
					return pin.CurrentAt(c.opts.Now())
				}
				return request.WithCommandOutput(func(original context.Context) error {
					// A separately carried command context must not detach the
					// original peer cancellation after MemberCommand returns.
					// Keep both contexts joined until actual emission has ended.
					// Derive from the Store-issued context so the transport can
					// authenticate this exact active output scope. Join the root
					// operation as well; neither parent may detach the other.
					emission, cancelEmission := context.WithDeadline(original, deadline)
					defer cancelEmission()
					joined := make(chan struct{})
					stop := context.AfterFunc(output, func() { cancelEmission(); close(joined) })
					defer func() {
						if !stop() {
							<-joined
						}
					}()
					if err := original.Err(); err != nil {
						return err
					}
					if err := check(); err != nil {
						return err
					}
					if err := emit(emission); err != nil {
						return err
					}
					return check()
				})
			})
		})
	})
	return committed, err
}
