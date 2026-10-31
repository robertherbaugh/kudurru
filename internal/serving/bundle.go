// Package serving publishes one complete immutable owner-prepared serving cut.
// It does not implement DNS, authentication, settings, or public API adapters.
package serving

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Errors describe local publication gates, never a failed canonical commit.
var (
	ErrPending    = errors.New("serving: generation pending")
	ErrClosed     = errors.New("serving: publisher closed")
	ErrComponents = errors.New("serving: incomplete or invalid components")
	ErrDependency = errors.New("serving: dependency changed")
	ErrPrepare    = errors.New("serving: owner preparation failed")
	ErrReleased   = errors.New("serving: lease released")
	ErrSuperseded = errors.New("serving: requested term superseded")
)

// CompleteManifest returns the minimum complete serving component manifest.
// Deployments may require additional named owners; none of these can be omitted.
func CompleteManifest() []string {
	return []string{"objects", "zones", "policy", "resolution", "response", "cache", "blocking", "inventory", "auth", "runtime", "control", "logging"}
}

// Component transfers one immutable owner value and ONE resource reference to
// the publisher. Value and all reachable configuration must remain immutable.
// Base binds even node-local data to its captured canonical configuration.
// Identity binds exact dependency bytes/specs/compiler/keyring versions; the
// owner must verify those dependencies before returning. Release may be nil for
// resource-free values; otherwise it releases this component's owned reference.
// Shared resources need a separate owner reference for each returned component.
type Component struct {
	Name     string
	Base     replication.Generation
	Identity string
	Value    any
	Release  func()
}

// Prepared transfers ownership even on an error return. Current must be a cheap
// thread-safe, side-effect-free dependency cursor check (no I/O or Store calls).
// It runs at publication and protected-read capture. It is required even for
// static dependencies, which can explicitly return true. Preparation must honor
// cancellation and clean up allocations not transferred in Components itself.
type Prepared struct {
	Components []Component
	Current    func() bool
}

// Identity keeps canonical generation and local dependency publication distinct.
type Identity struct {
	Base         replication.Generation
	Publication  uint64
	Dependencies string
}

// Bundle is a whole immutable cut. Owner values are available only by name;
// typed wrappers may assert their owner type. Values cannot outlive their Lease.
type Bundle struct {
	identity   Identity
	token      sqlite.GenerationToken
	dnsOnly    bool
	components map[string]Component
	current    func() bool
	refs       atomic.Int64
}

// Identity returns the actual published generation/dependency identity.
func (b *Bundle) Identity() Identity { return b.identity }

// DNSOnly marks explicit fenced/conflicted last-good publication. Even an older
// normal bundle becomes protected-read ineligible immediately on a new fence.
func (b *Bundle) DNSOnly() bool { return b.dnsOnly }

// Component returns an immutable owner value. The caller must hold a Lease.
func (b *Bundle) Component(name string) (any, bool) {
	c, ok := b.components[name]
	return c.Value, ok
}

func validName(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func nilValue(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func newBundle(token sqlite.GenerationToken, prepared Prepared, required []string) (*Bundle, error) {
	base := token.Generation()
	if err := base.Validate(); err != nil {
		return nil, err
	}
	if prepared.Current == nil {
		return nil, ErrComponents
	}
	components := make(map[string]Component, len(prepared.Components))
	type dependency struct{ Name, Identity string }
	deps := make([]dependency, 0, len(prepared.Components))
	for _, c := range prepared.Components {
		if !validName(c.Name) || len(c.Identity) == 0 || len(c.Identity) > 4096 || nilValue(c.Value) || !c.Base.Same(base) {
			return nil, fmt.Errorf("%w: owner identity/value/base", ErrComponents)
		}
		if _, found := components[c.Name]; found {
			return nil, fmt.Errorf("%w: duplicate owner %q", ErrComponents, c.Name)
		}
		components[c.Name] = c
		deps = append(deps, dependency{c.Name, c.Identity})
	}
	for _, name := range required {
		if _, found := components[name]; !found {
			return nil, fmt.Errorf("%w: missing owner %q", ErrComponents, name)
		}
	}
	slices.SortFunc(deps, func(a, b dependency) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	raw, err := json.Marshal(deps)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	b := &Bundle{identity: Identity{Base: base, Dependencies: hex.EncodeToString(digest[:])}, token: token, dnsOnly: token.DNSOnly(), components: components, current: prepared.Current}
	b.refs.Store(1) // publisher reference (or candidate before publication)
	return b, nil
}

func dependenciesCurrent(current func() bool) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return current()
}

func releaseHook(release func()) {
	if release == nil {
		return
	}
	// An owner panic cannot prevent the other owners from retiring. Hooks must
	// implement their own error reporting and may not assume a publication lock.
	defer func() { _ = recover() }()
	release()
}

func releasePrepared(prepared Prepared) {
	for _, c := range prepared.Components {
		releaseHook(c.Release)
	}
}

func (b *Bundle) retain() bool {
	for {
		refs := b.refs.Load()
		if refs == 0 {
			return false
		}
		if b.refs.CompareAndSwap(refs, refs+1) {
			return true
		}
	}
}

func (b *Bundle) release() {
	if b.refs.Add(-1) == 0 {
		for _, c := range b.components {
			releaseHook(c.Release)
		}
	}
}

// Lease keeps all resources of one captured bundle alive until Release.
// Subrequests normally share the outer Lease; Clone creates independent ownership.
type Lease struct {
	bundle   *Bundle
	released atomic.Bool
	once     sync.Once
}

// Bundle returns the captured immutable cut, or nil after Release.
func (l *Lease) Bundle() *Bundle {
	if l == nil || l.released.Load() {
		return nil
	}
	return l.bundle
}

// Clone returns another owned lease. Do not race resource use against Release.
func (l *Lease) Clone() (*Lease, error) {
	if l == nil || l.released.Load() || !l.bundle.retain() {
		return nil, ErrReleased
	}
	return &Lease{bundle: l.bundle}, nil
}

// Release is idempotent. Owner hooks run after the last reference, outside all
// publisher/store locks. No Bundle/Component access is permitted after release.
func (l *Lease) Release() {
	if l != nil {
		l.once.Do(func() {
			l.released.Store(true)
			l.bundle.release()
		})
	}
}
