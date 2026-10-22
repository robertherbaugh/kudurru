package servingbuild

import (
	"context"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ProviderSourceOptions supplies the same resource owner and host replacement
// pin used by provider commands. PinHost must retain all independent host/license
// exclusion through its entire synchronous callback. It must not acquire the
// publisher or resource guard again. Now must be the resource owner's clock.
type ProviderSourceOptions struct {
	Resources *providerresources.Owner
	PinHost   func(context.Context, func() error) error
	Now       func() time.Time
}

// GuardProviderSource composes publication -> providers -> host/license ->
// source. Wrap KeyringOwner.GuardSource with this adapter to place key replacement
// before the actual SQLite barrier. The complete builder must retain the exact
// provider and host cuts in Prepared.Current; acquiring a current pin cannot
// make a previously prepared stale candidate current.
func GuardProviderSource(source serving.Source, opts ProviderSourceOptions) (serving.Source, error) {
	if source == nil || opts.Resources == nil || opts.PinHost == nil {
		return nil, serving.ErrComponents
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return providerSource{Source: source, opts: opts}, nil
}

type providerSource struct {
	serving.Source
	opts ProviderSourceOptions
}

func (s providerSource) PublishIfCurrent(ctx context.Context, token sqlite.GenerationToken, opts sqlite.PublicationOptions, publish func() error) error {
	if ctx == nil || publish == nil {
		return serving.ErrComponents
	}
	return s.opts.Resources.WithCurrent(ctx, func(p *providerresources.Pinned) error {
		return s.opts.PinHost(ctx, func() error {
			return s.Source.PublishIfCurrent(ctx, token, opts, func() error {
				// This is inside the actual store barrier, after any writer wait.
				// Pointer exclusion does not freeze expiry or request cancellation.
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := p.CurrentAt(s.opts.Now()); err != nil {
					return err
				}
				return publish()
			})
		})
	})
}
