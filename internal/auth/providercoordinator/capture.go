package providercoordinator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func cloneSource(s servingauth.ProviderDescriptor) servingauth.ProviderDescriptor {
	data, _ := json.Marshal(s)
	var out servingauth.ProviderDescriptor
	_ = json.Unmarshal(data, &out)
	return out
}
func cloneSources(sources []servingauth.ProviderDescriptor) []servingauth.ProviderDescriptor {
	out := make([]servingauth.ProviderDescriptor, len(sources))
	for i, s := range sources {
		out[i] = cloneSource(s)
	}
	return out
}
func identity(host, resource string) string {
	data, _ := json.Marshal([]string{"provider-coordinator/v1", host, resource})
	d := sha256.Sum256(data)
	return hex.EncodeToString(d[:])
}
func releaseHost(h HostCapture) {
	if h.Release != nil {
		h.Release()
	}
}
func (c *Coordinator) host(ctx context.Context, r *providerregistry.Registry) (HostCapture, error) {
	h, err := c.opts.Host(ctx, r)
	if err != nil {
		return h, err
	}
	if h.Identity == "" || len(h.Identity) > 512 || h.Providers == nil || h.Current == nil || h.Release == nil || h.Auth.Permissions == nil || h.Auth.Identity != h.Identity || h.Auth.ExpectedIdentity != h.Identity {
		return h, ErrInvalid
	}
	if !c.hostCurrent(h) {
		return h, ErrStale
	}
	return h, nil
}
func (c *Coordinator) hostCurrent(h HostCapture) bool {
	return !c.closed.Load() && h.Current != nil && h.Current() && (h.ValidUntil.IsZero() || c.opts.Now().Before(h.ValidUntil))
}
func registryCurrent(ctx context.Context, tx *sqlite.CommandTx) (*providerregistry.Registry, error) {
	if err := tx.CheckSectionBounds(ctx, "security", servingauth.MaxRows, servingauth.MaxSecurityBytes); err != nil {
		if errors.Is(err, replication.ErrTooLarge) {
			return nil, errors.Join(servingauth.ErrBounds, err)
		}
		return nil, err
	}
	rows, err := tx.ListSections(ctx, "security")
	if err != nil {
		return nil, err
	}
	return providerregistry.Decode(rows, tx.Authority().ClusterID)
}
func desiredCurrent(ctx context.Context, tx *sqlite.CommandTx, h *providerresources.HostCut) (providerresources.Desired, error) {
	r, err := registryCurrent(ctx, tx)
	if err != nil {
		return providerresources.Desired{}, err
	}
	s, err := servingauth.ProviderSourcesCurrent(ctx, tx, r.ConfigSources())
	if err != nil {
		return providerresources.Desired{}, err
	}
	return providerresources.Desired{RegistryDigest: r.Digest(), Host: h, Sources: s, Authorities: r.Authorities()}, nil
}
func (c *Coordinator) image(ctx context.Context, image *replication.Image) (*providerregistry.Registry, []servingauth.ProviderDescriptor, error) {
	if image == nil {
		return nil, nil, ErrInvalid
	}
	// Bound and seal the complete IAM graph BEFORE full local-validator cloning.
	cut, err := servingauth.CaptureImage(ctx, image, c.opts.Schema)
	if err != nil {
		return nil, nil, err
	}
	validated, err := c.opts.Store.ValidateImage(ctx, *image)
	if err != nil {
		return nil, nil, err
	}
	copyImage := validated.Image()
	if err := security.ValidateImage(&copyImage); err != nil {
		return nil, nil, err
	}
	r, err := providerregistry.Decode(copyImage.Payload.Security, copyImage.Term.ClusterID)
	if err != nil {
		return nil, nil, err
	}

	s, err := servingauth.ProviderSources(ctx, cut, r.ConfigSources())
	return r, s, err
}

// Capture is pure exact acquisition for this candidate. It never reconciles,
// writes, loads credentials or starts network work. All partial refs release.
func (c *Coordinator) Capture(ctx context.Context, image *replication.Image) (_ Capture, err error) {
	ctx, stop, err := c.context(ctx)
	if err != nil {
		return Capture{}, err
	}
	defer stop()
	r, sources, err := c.image(ctx, image)
	if err != nil {
		return Capture{}, err
	}
	var h HostCapture
	var d providerresources.Desired
	transferred := false
	defer func() {
		if !transferred {
			releaseHost(h)
		}
	}()
	err = c.opts.Resources.WithCurrent(ctx, func(p *providerresources.Pinned) error {
		return c.opts.PinHost(ctx, func() error {
			var e error
			h, e = c.host(ctx, r)
			if e != nil {
				return e
			}
			d = providerresources.Desired{RegistryDigest: r.Digest(), Host: h.Providers, Sources: sources, Authorities: r.Authorities()}
			return p.Match(d)
		})
	})
	if err != nil {
		return Capture{}, err
	}
	// Do not reenter Owner.Acquire while holding its current callback guard.
	lease, err := c.opts.Resources.Acquire(ctx, d)
	if err != nil {
		return Capture{}, err
	}
	if !c.hostCurrent(h) {
		lease.Release()
		return Capture{}, ErrStale
	}
	var once sync.Once
	transferred = true
	return Capture{Identity: identity(h.Identity, lease.Identity()), HostIdentity: h.Identity,
		ConfigProviders: r.ConfigSources(), ProviderResources: lease.Resources(),
		Browser: func(ref servingauth.ProviderRef, resourceID string) (*providerresources.Browser, error) {
			if !c.hostCurrent(h) {
				return nil, ErrStale
			}
			browser, err := lease.Browser(ref, resourceID)
			if err != nil {
				return nil, err
			}
			return browser.BindCurrent(func() bool { return c.hostCurrent(h) })
		},
		Current: func() bool { return c.hostCurrent(h) && lease.Current() },
		Release: func() { once.Do(func() { lease.Release(); releaseHost(h) }) }}, nil
}

func deadlineDelay(now, deadline time.Time, fallback time.Duration) time.Duration {
	if !deadline.IsZero() && deadline.After(now) && deadline.Sub(now) < fallback {
		return deadline.Sub(now)
	}
	return fallback
}
