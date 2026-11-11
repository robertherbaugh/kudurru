package servingbuild

import (
	"context"
	"fmt"
	"time"

	internal "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servingzones"
)

// Builder prepares every required real owner before a publisher can swap its pointer.
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
	migrations          []*internal.MigrationRun
	unblockRequests     []*public.UnblockRequest
	primary             *primaryruntime.PreparedImage
	zones               *servingzones.Prepared
	keys                *dnsserver.StaticKeyring
	keyringIdentity     string
	tsig                *TSIGManagement
	current             func() bool
	dependenciesCurrent func() bool
	dnsDependencies     func() bool
	browser             func(servingauth.ProviderRef, string) (*providerresources.Browser, error)
	cli                 func(servingauth.ProviderRef, string) (*providerresources.CLI, error)
}

// Primary returns an immutable owner for exact publication acknowledgment.
func (r *RuntimeOwner) Primary() *primaryruntime.PreparedImage { return r.primary }

// Zones returns the immutable, credential-bound runtime selection owner.
func (r *RuntimeOwner) Zones() *servingzones.Prepared { return r.zones }

// NotifyKeys returns credentials from this owner's complete prepared dependency
// cut. The caller must retain the containing serving lease throughout use. The
// predicate covers every captured dependency; it takes no locks and does no I/O.
// It does not grant current writer admission or exclude dependency replacement.
func (r *RuntimeOwner) NotifyKeys() (string, *dnsserver.StaticKeyring, func() bool) {
	if r == nil {
		return "", nil, nil
	}
	return r.keyringIdentity, r.keys, r.current
}

// DependenciesCurrent checks the retained dependency installations independently
// of the canonical image revision. A command's own commit retires that image;
// acknowledgment must instead check its exact receipt and original writer against
// a new complete publication. This predicate grants neither read nor write
// admission. The caller must retain the original serving lease throughout use.
func (r *RuntimeOwner) DependenciesCurrent() bool {
	return r != nil && r.dependenciesCurrent != nil && r.dependenciesCurrent()
}

// DNSDependenciesCurrent is DependenciesCurrent restricted to the
// installations that DNS answers are computed from (Dependencies.DNSCurrent):
// a replaced provider host, license cut or provider resource set does not
// retire it (D378). It is for DNS output of the publication that is served
// only; acknowledgments, transfers and every HTTP use keep
// DependenciesCurrent. It grants neither read nor write admission.
func (r *RuntimeOwner) DNSDependenciesCurrent() bool {
	return r != nil && r.dnsDependencies != nil && r.dnsDependencies()
}

// Browser selects only this complete owner's provider capability and binds its
// currency to every captured dependency. The caller must retain the containing
// HTTP lease through use. Exchange runs outside guards; this is not admission
// to a writer command and cannot capture a newer provider installation.
func (r *RuntimeOwner) Browser(ref servingauth.ProviderRef, resourceID string) (*providerresources.Browser, error) {
	if r == nil || r.browser == nil || r.current == nil || !r.current() {
		return nil, serving.ErrDependency
	}
	b, err := r.browser(ref, resourceID)
	if err != nil {
		return nil, err
	}
	return b.BindCurrent(r.current)
}

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
	inventory, err := prepareInventory(candidate.Image, now)
	var groups []*public.ClientGroup
	if err == nil && opts.PrepareIntegrations != nil {
		inventory, groups, err = opts.PrepareIntegrations(ctx, candidate.Image, primary, now)
	}
	if err != nil {
		return serving.Prepared{}, fmt.Errorf("servingbuild: inventory image: %w", err)
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
	if deps.Identity == "" || len(deps.Identity) > 512 || deps.Host == nil || deps.Current == nil || deps.Keys == nil || deps.KeyringIdentity == "" || len(deps.KeyringIdentity) > 512 {
		return serving.Prepared{}, serving.ErrDependency
	}
	// Persisted config identity/precedence belongs to this candidate. A local
	// resource capture cannot omit a configured provider to reveal an object
	// with the same ID; absent or mismatched resources remain unavailable.
	deps.Auth.ConfigProviders = registry.ConfigSources()
	owners, err = prepareSemantic(ctx, candidate, opts, deps, now, integrationOverlay{groups: groups, grants: inventory.MACGrants})
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
	if !owners.current() {
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
		component("inventory", inventory, nil),
		component("auth", owners.auth, owners.auth.Release),
		component("runtime", &RuntimeOwner{migrations: owners.migrations, unblockRequests: owners.unblockRequests, primary: primary, zones: zones, keys: deps.Keys, keyringIdentity: deps.KeyringIdentity, tsig: deps.TSIG, current: owners.current, dependenciesCurrent: owners.dependenciesCurrent, dnsDependencies: owners.dnsDependenciesCurrent, browser: deps.Browser, cli: deps.CLI}, deps.Release),
		component("control", owners.control, nil),
		component("logging", owners.logging, owners.logging.Release),
	}
	// List content and pure auth policy have exact owner identities in addition
	// to the captured host dependency revision and canonical image identity.
	parts[6].Identity = componentIdentity(base, deps.Identity, "blocking:"+owners.lists)
	parts[8].Identity = owners.auth.DependencyIdentity()
	timeline, err := newCacheTimeline(owners, inventory, zones)
	if err != nil {
		return serving.Prepared{}, err
	}
	parts = append(parts, component("edge-cache", timeline, nil))
	transferred = true
	return serving.Prepared{Components: parts, Current: owners.current, Anchor: owners.anchor}, nil
}

// CLI selects only the retained runtime's exact public-client capability. This
// is read-only correlation, never command or writer authority.
func (r *RuntimeOwner) CLI(ref servingauth.ProviderRef, id string) (*providerresources.CLI, error) {
	if r == nil || r.cli == nil || r.current == nil || !r.current() {
		return nil, serving.ErrDependency
	}
	c, e := r.cli(ref, id)
	if e != nil {
		return nil, e
	}
	return c.BindCurrent(r.current)
}
