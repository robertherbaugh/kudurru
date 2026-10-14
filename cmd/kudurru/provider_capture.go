package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/auth/providercoordinator"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// providerDependencyCapture joins two owned cuts from the SAME candidate. The
// coordinator's host installation must be exactly the one that supplied base
// authentication policy, keys and permissions. A resource capture cannot replace
// a different host cut merely because the public provider configuration matches.
// References transfer to the returned Dependencies even on failure, matching
// Builder.Capture's unconditional Release contract.
func providerDependencyCapture(
	base func(context.Context, *replication.Image) (servingbuild.Dependencies, error),
	providers func(context.Context, *replication.Image) (providercoordinator.Capture, error),
) (func(context.Context, *replication.Image) (servingbuild.Dependencies, error), error) {
	if base == nil || providers == nil {
		return nil, serving.ErrComponents
	}
	return func(ctx context.Context, image *replication.Image) (servingbuild.Dependencies, error) {
		d, err := base(ctx, image)
		if err != nil {
			return d, err
		}
		p, err := providers(ctx, image)
		baseRelease, baseCurrent := d.Release, d.Current
		var once sync.Once
		var released atomic.Bool
		d.Release = func() {
			once.Do(func() {
				released.Store(true)
				if p.Release != nil {
					p.Release()
				}
				if baseRelease != nil {
					baseRelease()
				}
			})
		}
		// Deny even an accidental use of a failed, partly assembled result.
		d.Current = func() bool { return false }
		if err != nil {
			return d, err
		}
		if d.Identity == "" || len(d.Identity) > 512 || p.Identity == "" || len(p.Identity) > 512 || p.HostIdentity == "" || len(p.HostIdentity) > 512 || d.Auth.Identity != p.HostIdentity || d.Auth.ExpectedIdentity != p.HostIdentity || baseCurrent == nil || p.Current == nil || p.Release == nil || p.ConfigProviders == nil || p.ProviderResources == nil {
			return d, serving.ErrDependency
		}
		if err := ctx.Err(); err != nil {
			return d, err
		}
		if !baseCurrent() || !p.Current() {
			return d, serving.ErrDependency
		}
		data, _ := json.Marshal([]string{"serving-provider-dependencies/v1", d.Identity, p.Identity})
		digest := sha256.Sum256(data)
		d.Identity = hex.EncodeToString(digest[:])
		// Use the coordinator's identity directly, matching WithCommand's
		// current transaction projection for this same host/resource pair.
		d.Auth.Identity, d.Auth.ExpectedIdentity = p.Identity, p.Identity
		d.Auth.ConfigProviders, d.Auth.ProviderResources = p.ConfigProviders, p.ProviderResources
		d.Current = func() bool { return !released.Load() && baseCurrent() && p.Current() }
		return d, nil
	}, nil
}
