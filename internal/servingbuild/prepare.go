package servingbuild

import (
	"context"
	"fmt"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingzones"
)

// Builder prepares all ten real owners before a publisher can swap its pointer.
// It has no independent serving pointer and never substitutes old owner values.
type Builder struct{ opts Options }

// New checks infrastructure without reading the store or starting workers.
func New(opts Options) (*Builder, error) {
	if opts.Schema == nil || opts.Lists == nil || opts.Capture == nil || opts.Authority == nil {
		return nil, serving.ErrComponents
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Namespace == "" {
		opts.Namespace = "default"
	}
	return &Builder{opts: opts}, nil
}

// RuntimeOwner retains the exact primary versions and bound local runtime part.
// Preparing a version does not acknowledge actual publication to an UPDATE.
type RuntimeOwner struct {
	primary *primaryruntime.PreparedImage
	zones   *servingzones.Prepared
}

// Primary returns an immutable owner for exact publication acknowledgment.
func (r *RuntimeOwner) Primary() *primaryruntime.PreparedImage { return r.primary }

// Zones returns the immutable, credential-bound runtime selection owner.
func (r *RuntimeOwner) Zones() *servingzones.Prepared { return r.zones }

// Prepare implements serving.PrepareFunc. The returned resource references
// transfer to the publisher. Every earlier failure releases all acquired owners.
func (b *Builder) Prepare(ctx context.Context, candidate serving.Candidate) (_ serving.Prepared, err error) {
	if err := ctx.Err(); err != nil {
		return serving.Prepared{}, err
	}
	if candidate.Image == nil || !candidate.Image.Generation().Same(candidate.Token.Generation()) {
		return serving.Prepared{}, serving.ErrComponents
	}
	opts := b.opts
	now := opts.Now()
	base := candidate.Image.Generation()
	primary, err := primaryruntime.PrepareImage(ctx, *candidate.Image, primaryruntime.ImageOptions{ClusterID: base.Term.ClusterID, Scheme: opts.Schema.scheme, Validation: opts.Schema.validation(ctx), Limits: opts.ZoneLimits, MaxZones: opts.MaxZones, Now: now})
	if err != nil {
		return serving.Prepared{}, fmt.Errorf("servingbuild: primary image: %w", err)
	}
	registry, err := providerregistry.Decode(candidate.Image.Payload.Security, base.Term.ClusterID)
	if err != nil {
		return serving.Prepared{}, err
	}
	deps, err := opts.Capture(ctx, candidate.Image)
	transferred := false
	var owners *semanticOwners
	defer func() {
		if !transferred {
			owners.release()
			if deps.Release != nil {
				deps.Release()
			}
		}
	}()
	if err != nil {
		return serving.Prepared{}, err
	}
	if deps.Identity == "" || len(deps.Identity) > 512 || deps.Host == nil || deps.Current == nil || deps.Keys == nil {
		return serving.Prepared{}, serving.ErrDependency
	}
	// Persisted config identity/precedence belongs to this candidate. A local
	// resource capture cannot omit a configured provider to reveal an object
	// with the same ID; absent or mismatched resources remain unavailable.
	deps.Auth.ConfigProviders = registry.ConfigSources()
	owners, err = prepareSemantic(ctx, candidate, opts, deps, now)
	if err != nil {
		owners = nil // prepareSemantic already retired its partial resources.
		return serving.Prepared{}, err
	}
	zones, err := servingzones.Prepare(servingzones.Options{Base: base, Objects: owners.objects.Objects, Primary: primary, Secondary: deps.Secondary, Stub: deps.Stub, RPZ: deps.RPZ, Keys: deps.Keys, Now: now, MaxZones: opts.MaxZones})
	if err != nil {
		return serving.Prepared{}, err
	}
	if err := ctx.Err(); err != nil {
		return serving.Prepared{}, err
	}
	if !deps.Current() {
		return serving.Prepared{}, serving.ErrDependency
	}
	component := func(name string, value any, release func()) serving.Component {
		return serving.Component{Name: name, Base: base, Identity: componentIdentity(base, deps.Identity, name), Value: value, Release: release}
	}
	parts := []serving.Component{
		component("objects", owners.objects, nil),
		component("zones", zones, nil),
		component("policy", owners.policy, nil),
		component("resolution", owners.resolution, func() { _ = owners.upstreams.Close() }),
		component("response", owners.response, nil),
		component("cache", owners.cache, nil),
		component("blocking", owners.blocking, nil),
		component("inventory", &InventoryOwner{Snapshot: deps.Inventory}, nil),
		component("auth", owners.auth, owners.auth.Release),
		component("runtime", &RuntimeOwner{primary: primary, zones: zones}, deps.Release),
	}
	// List content and pure auth policy have exact owner identities in addition
	// to the captured host dependency revision and canonical image identity.
	parts[6].Identity = componentIdentity(base, deps.Identity, "blocking:"+owners.lists)
	parts[8].Identity = owners.auth.DependencyIdentity()
	transferred = true
	return serving.Prepared{Components: parts, Current: deps.Current}, nil
}
