package servingbuild

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/serving"
)

// PrimaryPinOptions names every independently mutable dependency used by the
// complete serving assembly. PinHostCurrent holds the actual host/license/auth
// key guards and supplies their original memory-only deadline/lifetime predicate.
// It must not reenter the publisher or provider resource owner. Now must be the
// resource owner's clock. Empty managers still require an explicit runtime.
type PrimaryPinOptions struct {
	Publisher      *serving.Publisher
	Resources      *providerresources.Owner
	PinHostCurrent func(context.Context, func(func() bool) error) error
	Keyrings       *KeyringOwner
	Catalog        *CatalogRuntime
	Transferred    *TransferredRuntime
	Now            func() time.Time
}

// NewPrimaryPin uses the same order as complete publication: publisher,
// providers, host/license/auth keys, TSIG, catalog, secondary, stub, RPZ, SQLite.
// The returned pin performs no Store or network I/O and remains usable for
// background recovery while protected serving is pending. The caller retains
// its original writer token/context and uses the concrete command's HA fence.
func NewPrimaryPin(o PrimaryPinOptions) (currentguard.PinFunc, error) {
	if o.Publisher == nil || o.Resources == nil || o.PinHostCurrent == nil || o.Keyrings == nil || o.Catalog == nil || o.Transferred == nil {
		return nil, serving.ErrComponents
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return func(ctx context.Context, fence pr.PublicationFence, fn func(currentguard.Pinned) error) error {
		if ctx == nil || fn == nil {
			return serving.ErrComponents
		}
		return o.Publisher.WithWriterGate(ctx, serving.TermActivation{Term: fence.Term, Incarnation: fence.Incarnation}, func() error {
			return o.Resources.WithCurrent(ctx, func(resources *providerresources.Pinned) error {
				return o.PinHostCurrent(ctx, func(hostCurrent func() bool) error {
					if hostCurrent == nil || !hostCurrent() {
						return serving.ErrDependency
					}
					return o.Keyrings.WithCurrent(ctx, func(keys KeyringCut) error {
						return o.Catalog.WithCurrent(ctx, func() error {
							catalog := o.Catalog.holder.Capture()
							return o.Transferred.WithCurrent(ctx, func() error {
								transferred := o.Transferred.capture()
								active := new(atomic.Bool)
								active.Store(true)
								defer active.Store(false)
								current := func() bool {
									return active.Load() && ctx.Err() == nil && hostCurrent() &&
										resources.CurrentAt(o.Now()) == nil && keys.Current() &&
										catalog.Current() && transferred.current()
								}
								// Lower gates can wait long enough for an earlier cut's
								// deadline to pass without a pointer replacement.
								if !current() {
									return serving.ErrDependency
								}
								return fn(currentguard.Pinned{Identity: keys.Identity(), Keys: keys.Keys(), Current: current})
							})
						})
					})
				})
			})
		})
	}, nil
}
