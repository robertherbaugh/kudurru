package providerhost

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providercoordinator"
	"github.com/robertherbaugh/kudurru/internal/auth/providerentitlement"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
)

type attachment struct {
	Ref       servingauth.ProviderRef
	CreatedAt time.Time
}

// Old/new validated images may be prepared concurrently. Keep a small bounded
// MRU rather than evicting the current projection on every older-image capture.
const maxHostMemos = 4

func (o *Owner) eligible(g *generation, r *providerregistry.Registry) bool {
	if g.profile == "" || g.entitlement == "" {
		return false
	}
	if g.phase == pending {
		if g.writer == nil || g.writer.Err() != nil {
			return false
		}
		for _, a := range r.Authorities() {
			if a.Active && a.Binding.EntitlementIdentity != g.entitlement {
				return false
			}
		}
		return true
	}
	if g.phase == active {
		e, err := providerentitlement.Installed(r, g.cluster, g.profile)
		return err == nil && e == g.entitlement
	}
	return false
}

// Host purely projects a validated registry under resources -> PinHost. It
// never reads SQLite/license/files/network or acquires the resource owner again.
// Local spec bytes stay local; canonical UID/time attach without hiding mismatch.
func (o *Owner) Host(ctx context.Context, r *providerregistry.Registry) (providercoordinator.HostCapture, error) {
	if err := o.check(ctx); err != nil {
		return providercoordinator.HostCapture{}, err
	}
	g := o.value.Load()
	if r == nil || r.Digest() == "" || r.ClusterID() != g.cluster {
		return providercoordinator.HostCapture{}, ErrContinuity
	}
	if !o.current(g) {
		return providercoordinator.HostCapture{}, ErrStale
	}
	canonical := r.ConfigSources()
	projection := []attachment{}
	local := []servingauth.ProviderSource{}
	for _, s := range canonical {
		projection = append(projection, attachment{s.Ref, s.CreatedAt})
		for _, in := range g.static.input.LocalProviders {
			if s.Ref.ID == in.ID {
				s.Spec = clone(in.Spec)
				local = append(local, s)
				break
			}
		}
	}
	allowed := o.eligible(g, r)
	e := "provider-denied/v1"
	if allowed {
		e = g.entitlement
	}
	key := digest(struct {
		Serial                            uint64
		Cluster, E, Static, Keys, License string
		Allowed                           bool
		Config                            []attachment
	}{g.serial, g.cluster, e, g.static.input.Identity, g.static.input.AuthKeyIdentity, g.license.Identity(), allowed, projection})
	o.memoMu.Lock()
	var m memo
	for i, cached := range o.memos {
		if cached.key == key {
			m = cached
			copy(o.memos[1:i+1], o.memos[:i])
			o.memos[0] = m
			break
		}
	}
	if m.key == "" {
		in := g.static.input
		p, err := providerresources.NewHost(providerresources.HostOptions{Identity: key, CredentialPolicy: in.CredentialPolicy, EntitlementIdentity: e, FingerprintKeyID: in.FingerprintKeyID, ProvidersAllowed: allowed,
			ConfigSources: local, ObjectCredentials: in.ObjectCredentials, ConfigCredentials: in.ConfigCredentials, AllowedOrigins: in.AllowedOrigins, LocalOrigins: in.LocalOrigins, Algorithms: in.Algorithms, TrustPEM: in.TrustPEM})
		if err != nil {
			o.memoMu.Unlock()
			return providercoordinator.HostCapture{}, err
		}
		m = memo{key: key, identity: key, providers: p}
		if len(o.memos) < maxHostMemos {
			o.memos = append(o.memos, memo{})
		}
		copy(o.memos[1:], o.memos[:len(o.memos)-1])
		o.memos[0] = m
	}
	o.memoMu.Unlock()
	if !g.static.retain() {
		return providercoordinator.HostCapture{}, ErrStale
	}
	deps := cloneAuth(g.static.input.Auth)
	deps.Identity = m.identity
	deps.ExpectedIdentity = m.identity
	deps.ConfigProviders = canonical
	deps.License = servingauth.LicensePolicy{RBAC: g.license.Status().Allows(license.FeatureRBAC), Providers: allowed}
	var once sync.Once
	var released atomic.Bool
	return providercoordinator.HostCapture{Identity: m.identity, Providers: m.providers, Auth: deps, ValidUntil: g.until,
		Current: func() bool { return !released.Load() && o.current(g) },
		Release: func() { once.Do(func() { released.Store(true); g.static.release() }) }}, nil
}
