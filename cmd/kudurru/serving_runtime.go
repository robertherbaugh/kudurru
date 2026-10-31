package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/providercoordinator"
	"github.com/robertherbaugh/kudurru/internal/auth/providerhost"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/listfetcher"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

// servingRuntimeOptions borrows already constructed local infrastructure. Host
// configuration is detached once; canonical singleton settings come from each
// complete candidate. Secret/trust loading finishes before any owner is pinned.
type servingRuntimeOptions struct {
	Store       *sqlite.Store
	Config      *config.Config
	Validator   *validator.Validator
	License     *license.Manager
	AuthKeys    *authKeyOwner
	Keyrings    *servingbuild.KeyringOwner
	Catalog     *catalogue.Holder
	Transferred *servingbuild.TransferredRuntime
	Lists       *listfetcher.DiskCache
	Namespace   string
	Upstreams   upstream.Options
	Now         func() time.Time
}

// servingRuntime owns the complete publisher and provider/primary workers.
// Manager acquisition, the store, license loader and key owners remain borrowed.
// The root joins RunWriter before stopping public serving and calling Close.
type servingRuntime struct {
	opts         servingRuntimeOptions
	publisher    *serving.Publisher
	providers    *providercoordinator.Coordinator
	host         *providerhost.Owner
	resources    *providerresources.Owner
	primary      *primaryRuntime
	status       *servingbuild.PrimaryStatus
	integrations *integrationRuntime
	catalog      *servingbuild.CatalogRuntime
	local        []providerregistry.DesiredConfig
	life         context.Context
	cancel       context.CancelFunc
	lifecycle    chan struct{}
	mu           sync.Mutex
	workers      sync.WaitGroup
	running      bool
	writing      bool
	closing      bool
}

func newServingRuntime(ctx context.Context, o servingRuntimeOptions) (_ *servingRuntime, err error) {
	if ctx == nil || o.Store == nil || o.Config == nil || o.Validator == nil || o.License == nil || o.AuthKeys == nil || o.Keyrings == nil || o.Catalog == nil || o.Transferred == nil || o.Lists == nil {
		return nil, serving.ErrComponents
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	input, err := loadProviderStatic(ctx, providerStaticOptions{Config: o.Config, Keys: o.AuthKeys, Namespace: o.Namespace})
	if err != nil {
		return nil, err
	}
	prepared, err := providerhost.PrepareStatic(ctx, input)
	if err != nil {
		input.Release()
		return nil, err
	}
	defer prepared.Release()
	// Parse into a fresh value so neither caller edits nor nested maps can change
	// a held serving dependency. loadProviderStatic bounded provider input first.
	raw, err := json.Marshal(o.Config)
	if err != nil {
		return nil, serving.ErrDependency
	}
	var host config.Config
	if err = json.Unmarshal(raw, &host); err != nil {
		return nil, serving.ErrDependency
	}
	host.EnvLocked, host.Source = slices.Clone(o.Config.EnvLocked), o.Config.Source
	o.Config = &host
	s := &servingRuntime{opts: o, local: input.LocalProviders, lifecycle: make(chan struct{}, 1)}
	s.lifecycle <- struct{}{}
	s.life, s.cancel = context.WithCancel(context.Background())
	defer func() {
		if err != nil {
			_ = s.Close(context.Background())
		}
	}()
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		return nil, err
	}
	schema, err := servingbuild.NewSchema(scheme, o.Validator, runtimeowners.ValidateEntry, runtimeowners.ValidateImage)
	if err != nil {
		return nil, err
	}
	authSchema := servingauth.Schema{Scheme: scheme, Kinds: schema.PrimaryImageOptions(ctx, o.Store.Info().ClusterID).Validation.Kinds, SchemaVersion: store.SchemaVersion}
	s.resources, err = providerresources.New(providerresources.Options{Now: o.Now})
	if err != nil {
		return nil, err
	}
	s.catalog, err = servingbuild.NewCatalogRuntime(o.Catalog, host.Blocking.Catalogue)
	if err != nil {
		return nil, err
	}
	capture, err := providerDependencyCapture(s.captureHost, func(ctx context.Context, image *replication.Image) (providercoordinator.Capture, error) {
		return s.providers.Capture(ctx, image)
	})
	if err != nil {
		return nil, err
	}
	capture, err = s.catalog.CaptureDependencies(capture)
	if err != nil {
		return nil, err
	}
	capture, err = o.Transferred.CaptureDependencies(capture)
	if err != nil {
		return nil, err
	}
	builder, err := servingbuild.New(servingbuild.Options{Schema: schema, Lists: o.Lists, Namespace: o.Namespace, Capture: capture, Authority: o.Store.ServingAuthority, Now: o.Now, Upstreams: o.Upstreams, PrepareIntegrations: func(ctx context.Context, image *replication.Image, primary *primaryruntime.PreparedImage, at time.Time) (*servingbuild.InventoryOwner, []*public.ClientGroup, error) {
		return prepareIntegrationImage(ctx, image, primary, schema, at)
	}})
	if err != nil {
		return nil, err
	}
	// Wrap inside-out; actual acquisition is publication -> providers -> host
	// (license/auth keys) -> TSIG -> catalog -> managers -> SQLite.
	source, err := o.Transferred.GuardSource(o.Store)
	if err != nil {
		return nil, err
	}
	source, err = s.catalog.GuardSource(source)
	if err != nil {
		return nil, err
	}
	source, err = o.Keyrings.GuardSource(source)
	if err != nil {
		return nil, err
	}
	source, err = servingbuild.GuardProviderSource(source, servingbuild.ProviderSourceOptions{Resources: s.resources, PinHost: func(ctx context.Context, fn func() error) error { return s.host.PinHost(ctx, fn) }, Now: o.Now})
	if err != nil {
		return nil, err
	}
	s.publisher, err = serving.New(serving.Options{Source: source, Prepare: builder.Prepare, Required: serving.CompleteManifest(), DependencyChanges: func() <-chan struct{} { return s.providers.Changes() }})
	if err != nil {
		return nil, err
	}
	s.host, err = providerhost.New(providerhost.Options{Store: o.Store, Publisher: s.publisher, Resources: s.resources, License: o.License, Schema: authSchema, Static: prepared, Now: o.Now, PinAuthKeys: o.AuthKeys.withCurrent, Withdraw: func(ctx context.Context, tx *sqlite.CommandTx, at time.Time) error {
		return security.AcceptProviderAuthorities(ctx, tx, []providerregistry.DesiredAuthority{}, at)
	}})
	if err != nil {
		return nil, err
	}
	s.providers, err = providercoordinator.New(providercoordinator.Options{Store: o.Store, Publisher: s.publisher, Resources: s.resources, Schema: authSchema, Host: s.host.Host, HostChanges: s.host.Changes, PinHost: s.pinProviderHost, Admit: s.host.CheckAdmission, Now: o.Now})
	if err != nil {
		return nil, err
	}
	pin, err := servingbuild.NewPrimaryPin(servingbuild.PrimaryPinOptions{Publisher: s.publisher, Resources: s.resources, PinHostCurrent: s.host.PinHostCurrent, Keyrings: o.Keyrings, Catalog: s.catalog, Transferred: o.Transferred, Now: o.Now})
	if err != nil {
		return nil, err
	}
	var notifySource netip.Addr
	if host.Notify.Source != "" {
		notifySource, err = netip.ParseAddr(host.Notify.Source)
		if err != nil {
			return nil, err
		}
	}
	s.primary, err = newPrimaryRuntime(s.life, primaryRuntimeOptions{Store: o.Store, Publisher: s.publisher, Schema: schema, Pin: pin, NotifySource: notifySource})
	if err != nil {
		return nil, err
	}
	s.status, err = servingbuild.NewPrimaryStatus(servingbuild.PrimaryStatusOptions{Store: o.Store, Publisher: s.publisher, Pin: pin, Now: o.Now})
	if err != nil {
		return nil, err
	}
	s.integrations, err = newIntegrationRuntime(s, schema, pin)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *servingRuntime) captureHost(ctx context.Context, image *replication.Image) (servingbuild.Dependencies, error) {
	registry, err := providerregistry.Decode(image.Payload.Security, image.Term.ClusterID)
	if err != nil {
		return servingbuild.Dependencies{}, err
	}
	var host providercoordinator.HostCapture
	err = s.resources.WithCurrent(ctx, func(_ *providerresources.Pinned) error {
		return s.host.PinHost(ctx, func() error { var e error; host, e = s.host.Host(ctx, registry); return e })
	})
	if err != nil {
		if host.Release != nil {
			host.Release()
		}
		return servingbuild.Dependencies{}, err
	}
	keys := s.opts.Keyrings.Capture()
	id := sha256.Sum256([]byte("serving-host/v1/" + host.Identity + "/" + keys.Identity()))
	return servingbuild.Dependencies{Identity: hex.EncodeToString(id[:]), Host: s.opts.Config, Auth: host.Auth, Keys: keys.Keys(), KeyringIdentity: keys.Identity(), Current: func() bool { return s.life.Err() == nil && host.Current() && keys.Current() }, Release: host.Release}, nil
}

// Close cancels both root lifetimes and joins before retiring owned resources.
// Held bundles retain their own immutable references after this returns.
func (s *servingRuntime) Close(ctx context.Context) error {
	if s == nil || ctx == nil {
		return serving.ErrComponents
	}
	s.mu.Lock()
	s.closing = true
	s.cancel()
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.workers.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
	}
	if s.publisher != nil {
		s.publisher.Close()
	}
	var err error
	if s.host != nil {
		err = s.host.Close(ctx)
	}
	if s.providers != nil {
		err = errors.Join(err, s.providers.Close(ctx))
	} else if s.resources != nil {
		err = errors.Join(err, s.resources.Close())
	}
	return err
}
