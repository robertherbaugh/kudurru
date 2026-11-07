package servingbuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// CatalogRuntime binds one holder to its original restart-required source
// configuration. Replacement of the configuration requires a new outer assembly;
// a candidate's desired URL can never relabel an old acquisition.
type CatalogRuntime struct {
	holder *catalogue.Holder
	source config.BlockingCatalogue
}

// NewCatalogRuntime preserves the documented embedded/absent-import fallback.
// An acquired document must have the exact original configured source. Holder
// digest no-ops retain their actual provenance, even for equal remote content.
func NewCatalogRuntime(holder *catalogue.Holder, source config.BlockingCatalogue) (*CatalogRuntime, error) {
	if holder == nil || len(source.URL) > 8192 || len(source.AdGuardServicesURL) > 8192 {
		return nil, serving.ErrComponents
	}
	o := &CatalogRuntime{holder: holder, source: source}
	if !o.matches(holder.Capture()) {
		return nil, serving.ErrDependency
	}
	return o, nil
}

func (o *CatalogRuntime) matches(c catalogue.Cut) bool {
	if o == nil || !c.Current() || c.Identity() == "" {
		return false
	}
	s := c.Sources()
	if s.BaseOrigin == catalogue.OriginEmbedded {
		if s.BaseDigest != catalogue.Embedded().Digest() {
			return false
		}
	} else if o.source.URL == "" || s.BaseOrigin != o.source.URL || s.BaseDigest == "" {
		return false
	}
	if s.HasImport {
		return o.source.AdGuardServicesURL != "" && s.ImportOrigin == o.source.AdGuardServicesURL && s.ImportDigest != ""
	}
	return s.ImportOrigin == "" && s.ImportDigest == ""
}

// CaptureDependencies prepares a detached catalog outside all publication gates.
// The result carries original configured policy AND actual acquired provenance;
// semantic preparation still requires exact candidate configuration equality.
func (o *CatalogRuntime) CaptureDependencies(base func(context.Context, *replication.Image) (Dependencies, error)) (func(context.Context, *replication.Image) (Dependencies, error), error) {
	if o == nil || base == nil {
		return nil, serving.ErrComponents
	}
	return func(ctx context.Context, image *replication.Image) (Dependencies, error) {
		d, err := base(ctx, image)
		if err != nil {
			return d, err
		}
		c := o.holder.Capture()
		if d.Identity == "" || len(d.Identity) > 512 || d.Current == nil || !o.matches(c) {
			return d, serving.ErrDependency
		}
		catalog := c.Catalogue()
		if catalog == nil {
			return d, serving.ErrDependency
		}
		if err := ctx.Err(); err != nil {
			return d, err
		}
		data, _ := json.Marshal(struct {
			Domain, Base, Installation string
			Source                     config.BlockingCatalogue
		}{"catalog-runtime/v1", d.Identity, c.Identity(), o.source})
		digest := sha256.Sum256(data)
		d.Identity = hex.EncodeToString(digest[:])
		d.Catalog = CatalogResource{Catalogue: catalog, Source: o.source}
		current := d.Current
		d.Current = func() bool { return current() && c.Current() }
		return d, nil
	}, nil
}

// WithCurrent pins the catalog installation through the full callback, after
// host/keyring and before transferred managers/SQLite. It performs no copies,
// parsing or acquisition while pinned. Original prepared cuts still check Current.
func (o *CatalogRuntime) WithCurrent(ctx context.Context, fn func() error) error {
	if o == nil || ctx == nil || fn == nil {
		return serving.ErrComponents
	}
	c := o.holder.Capture()
	if !o.matches(c) {
		return serving.ErrDependency
	}
	return o.holder.WithCurrent(ctx, c, fn)
}

// GuardSource belongs inside keyring exclusion and outside transferred managers.
func (o *CatalogRuntime) GuardSource(source serving.Source) (serving.Source, error) {
	if o == nil || source == nil {
		return nil, serving.ErrComponents
	}
	return catalogSource{Source: source, owner: o}, nil
}

type catalogSource struct {
	serving.Source
	owner *CatalogRuntime
}

func (s catalogSource) PublishIfCurrent(ctx context.Context, token sqlite.GenerationToken, options sqlite.PublicationOptions, publish func() error) error {
	return s.owner.WithCurrent(ctx, func() error { return s.Source.PublishIfCurrent(ctx, token, options, publish) })
}

// PrimaryPin uses the same installation gate through complete current commands.
func (o *CatalogRuntime) PrimaryPin(base currentguard.PinFunc) currentguard.PinFunc {
	return func(ctx context.Context, fence pr.PublicationFence, fn func(currentguard.Pinned) error) error {
		if o == nil || base == nil || fn == nil {
			return serving.ErrComponents
		}
		return base(ctx, fence, func(p currentguard.Pinned) error {
			return o.WithCurrent(ctx, func() error { return fn(p) })
		})
	}
}

// Watch wakes on replacements using close-and-replace broadcasts. It registers
// no legacy permanent subscriptions and subscribes before every rebuild hint.
func (o *CatalogRuntime) Watch(ctx context.Context, wake func()) error {
	if o == nil || ctx == nil || wake == nil {
		return serving.ErrComponents
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		changed := o.holder.Changes()
		wake()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
