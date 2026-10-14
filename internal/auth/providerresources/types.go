// Package providerresources acquires bounded provider capabilities in owned
// background workers. Authentication receives immutable offline verifiers;
// root retains durable authority admission, current commands and publication.
package providerresources

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/credentials"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
)

// Resource acquisition limits are independent of public policy/claim limits.
const (
	MaxDiscoveryBytes     = 64 << 10
	MaxCredentialBytes    = 4 << 10
	MaxTokenResponseBytes = 64 << 10
	MaxTrustBytes         = 2 << 20
	MaxTrustCertificates  = 256
)

// Errors are safe diagnostics without upstream data or credentials.
var (
	ErrInvalid     = errors.New("providerresources: invalid input")
	ErrBounds      = errors.New("providerresources: bounds exceeded")
	ErrStale       = errors.New("providerresources: resource cut changed")
	ErrUnavailable = errors.New("providerresources: capability unavailable")
	ErrClosed      = errors.New("providerresources: owner closed")
)

// HostOptions is trusted root input. Resolver grants remain origin-specific;
// stored objects never inherit a trusted config source's credential capability.
type HostOptions struct {
	ConfigSources                                                     []servingauth.ProviderSource
	Identity, CredentialPolicy, EntitlementIdentity, FingerprintKeyID string
	ProvidersAllowed                                                  bool
	ObjectCredentials                                                 *credentials.Resolver
	ConfigCredentials                                                 map[string]*credentials.Resolver
	AllowedOrigins, LocalOrigins, Algorithms                          []string
	TrustPEM                                                          []byte
}

// HostCut captures private immutable root host capabilities, never public claims.
type HostCut struct {
	serial                                                  uint64
	configSpecs                                             map[string]string
	identity, credentialPolicy, entitlement, fingerprintKey string
	allowed                                                 bool
	objects                                                 *credentials.Resolver
	configs                                                 map[string]*credentials.Resolver
	network                                                 *networkPolicy
}

// Desired pins a complete source/registry/host cut and admitted authority receipts.
type Desired struct {
	RegistryDigest string
	Host           *HostCut
	Sources        []servingauth.ProviderDescriptor
	Authorities    []providerregistry.AuthorityRecord
}

// Options bounds background work and lifetime. Now must be a pure thread-safe clock.
type Options struct {
	Now                                                            func() time.Time
	MaxConcurrent                                                  int
	OperationTimeout, RefreshInterval, MaxFreshness, RetryInterval time.Duration
}

// Owner has one memory cursor, separate from the complete serving publisher.
type Owner struct {
	opts                      Options
	mu                        sync.RWMutex
	current                   atomic.Pointer[snapshot]
	revision                  uint64
	desiredRevision           uint64
	desired                   Desired
	desiredID                 string
	candidates                map[string]*AuthorityCandidate
	running, closed           atomic.Bool
	wake                      chan struct{}
	stop                      chan struct{}
	stopOnce                  sync.Once
	changed, candidateChanges chan struct{}
	done                      chan struct{}
	cancel                    context.CancelFunc
}

type snapshot struct {
	desiredID, identity string
	revision            uint64
	desiredRevision     uint64
	entries             []entry
	until               time.Time
	observed            time.Time
}
type entry struct {
	descriptor servingauth.ProviderDescriptor
	resource   servingauth.ProviderResource
	metadata   metadata
	until      time.Time
}

// Lease retains one immutable public-key/endpoint cut. It does not pin replacements.
type Lease struct {
	owner    *Owner
	snapshot *snapshot
	released atomic.Bool
}

// Replacement is scoped to an exclusive root replacement callback.
type Replacement struct {
	owner  *Owner
	active atomic.Bool
}

// Pinned is valid only inside a scoped current-exclusion callback.
type Pinned struct {
	owner    *Owner
	snapshot *snapshot
	active   atomic.Bool
}

// AuthorityCandidate contains private verified acquisition provenance. It is
// not durable admission, and cannot be fabricated by editing its Receipt result.
type AuthorityCandidate struct {
	owner                             *Owner
	desiredRevision                   uint64
	desiredID, registryDigest, hostID string
	receipt                           providerregistry.DesiredAuthority
}

// EndpointInfo is detached metadata, with no client credential or network handle.
type EndpointInfo struct {
	Provider                                              servingauth.ProviderRef
	ResourceIdentity, AuthorizationURL, TokenURL, JWKSURL string
}

func (*HostCut) String() string { return "private provider host cut" }

// GoString redacts host capabilities.
func (h *HostCut) GoString() string { return h.String() }

// MarshalJSON prevents accidental credential/host capability serialization.
func (*HostCut) MarshalJSON() ([]byte, error) { return []byte(`"private host cut"`), nil }

func (*AuthorityCandidate) String() string { return "private provider authority candidate" }

// GoString redacts verified acquisition details.
func (c *AuthorityCandidate) GoString() string { return c.String() }

// MarshalJSON prevents serializing an acquisition as durable admission.
func (*AuthorityCandidate) MarshalJSON() ([]byte, error) { return []byte(`"private candidate"`), nil }

// Receipt returns detached root-admission input without secret or verifier bytes.
func (c *AuthorityCandidate) Receipt() providerregistry.DesiredAuthority {
	if c == nil {
		return providerregistry.DesiredAuthority{}
	}
	d := c.receipt
	d.Source = cloneDescriptor(d.Source)
	return d
}

// Match verifies that a candidate still belongs to the exact current desired cut.
func (c *AuthorityCandidate) Match(d Desired) error {
	id, err := desiredIdentity(d)
	if err != nil {
		return err
	}
	if c == nil || c.desiredID != id || c.registryDigest != d.RegistryDigest || c.hostID != d.Host.identity {
		return ErrStale
	}
	if c.owner == nil || c.owner.closed.Load() {
		return ErrStale
	}
	s := c.owner.current.Load()
	if s == nil || s.desiredRevision != c.desiredRevision || s.desiredID != id {
		return ErrStale
	}
	return nil
}

// CurrentAt validates the pinned current cut at the final actual admission time.
func (p *Pinned) CurrentAt(now time.Time) error {
	if p == nil || !p.active.Load() || p.owner.closed.Load() {
		return ErrClosed
	}
	if p.owner.current.Load() != p.snapshot {
		return ErrStale
	}
	return validAt(p.snapshot, now)
}

// Match checks a transaction-reprojected complete source/registry/host cut.
func (p *Pinned) Match(d Desired) error {
	if p == nil || !p.active.Load() {
		return ErrClosed
	}
	id, err := desiredIdentity(d)
	if err != nil {
		return err
	}
	if id != p.snapshot.desiredID {
		return ErrStale
	}
	return nil
}

// Resources returns detached offline resources during the exclusion callback.
func (p *Pinned) Resources() []servingauth.ProviderResource {
	if p == nil || !p.active.Load() {
		return nil
	}
	return resources(p.snapshot)
}

// WithCurrent excludes ALL owner replacement through fn's entire synchronous
// lifetime, including root's actual Store.Command wait/commit/no-op or publication.
func (o *Owner) WithCurrent(ctx context.Context, fn func(*Pinned) error) error {
	if fn == nil {
		return ErrInvalid
	}
	if err := o.readLock(ctx); err != nil {
		return err
	}
	defer o.mu.RUnlock()
	s := o.current.Load()
	if o.closed.Load() || s == nil {
		return ErrClosed
	}
	if err := validAt(s, o.opts.Now()); err != nil {
		return err
	}
	p := &Pinned{owner: o, snapshot: s}
	p.active.Store(true)
	defer p.active.Store(false)
	return fn(p)
}
