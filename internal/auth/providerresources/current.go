package providerresources

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// CurrentDependencies is a detached provider-only projection from the actual
// CommandTx plus the pinned host/resource cut. It does not authorize a caller.
type CurrentDependencies struct {
	Identity, RegistryDigest string
	ConfigProviders          []servingauth.ProviderSource
	ProviderResources        []servingauth.ProviderResource
}

// DependenciesCurrent reads the CURRENT canonical registry and object sources
// from the supplied concrete command view; cached ConfigProviders cannot enter.
// It also registers a memory-only final resource/clock check. Root must keep
// WithCurrent (or Replacement.WithCurrent) around the ENTIRE Store.Command.
func (p *Pinned) DependenciesCurrent(ctx context.Context, tx *sqlite.CommandTx, host *HostCut) (CurrentDependencies, error) {
	if p == nil || !p.active.Load() || tx == nil {
		return CurrentDependencies{}, ErrInvalid
	}
	if err := p.CurrentAt(p.owner.opts.Now()); err != nil {
		return CurrentDependencies{}, err
	}
	if err := tx.CheckSectionBounds(ctx, "security", servingauth.MaxRows, servingauth.MaxSecurityBytes); err != nil {
		return CurrentDependencies{}, err
	}
	rows, err := tx.ListSections(ctx, "security")
	if err != nil {
		return CurrentDependencies{}, err
	}
	registry, err := providerregistry.Decode(rows, tx.Authority().ClusterID)
	if err != nil {
		return CurrentDependencies{}, err
	}
	config := registry.ConfigSources()
	sources, err := servingauth.ProviderSourcesCurrent(ctx, tx, config)
	if err != nil {
		return CurrentDependencies{}, err
	}
	desired := Desired{RegistryDigest: registry.Digest(), Host: host, Sources: sources, Authorities: registry.Authorities()}
	if err = p.Match(desired); err != nil {
		return CurrentDependencies{}, err
	}
	if err = tx.RequireCommitCheck(func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return p.CurrentAt(p.owner.opts.Now())
	}); err != nil {
		return CurrentDependencies{}, err
	}
	return CurrentDependencies{p.snapshot.identity, registry.Digest(), config, p.Resources()}, nil
}

// WithCurrent admits a synchronous prior-current callback under an already held
// exclusive replacement guard. It avoids a read-to-write upgrade for root's
// admitted registry/host mutations. SetDesired must follow Command completion
// before the exclusive guard is released. No nested owner locking is permitted.
func (r *Replacement) WithCurrent(fn func(*Pinned) error) error {
	if r == nil || !r.active.Load() || fn == nil {
		return ErrInvalid
	}
	o := r.owner
	s := o.current.Load()
	if err := validAt(s, o.opts.Now()); err != nil {
		return err
	}
	p := &Pinned{owner: o, snapshot: s}
	p.active.Store(true)
	defer p.active.Store(false)
	return fn(p)
}
