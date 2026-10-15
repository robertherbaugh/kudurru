// Package providercoordinator connects canonical provider authority, background
// acquisition and complete serving capture. Root supplies immutable host policy,
// explicit authority admission and publication/HTTP adapters. No auth path does I/O.
package providercoordinator

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Safe errors never include credential, license or upstream response data.
var (
	ErrInvalid   = errors.New("providercoordinator: invalid input")
	ErrClosed    = errors.New("providercoordinator: closed")
	ErrStale     = errors.New("providercoordinator: dependency changed")
	ErrAdmission = errors.New("providercoordinator: authority admission denied")
	ErrRunning   = errors.New("providercoordinator: already running")
)

// HostCapture is an owned root installation. Host must be pure, memoized and
// called under resource exclusion: no Store/network calls or nested owner lock.
// Current includes installation identity and the actual entitlement deadline.
// Root must serialize ALL host/provider mutators with that same exclusion.
type HostCapture struct {
	Identity   string
	Providers  *providerresources.HostCut
	Auth       servingauth.Dependencies
	ValidUntil time.Time
	Current    func() bool
	Release    func()
}

// HostCaptureFunc projects this exact registry onto the pinned local host.
type HostCaptureFunc func(context.Context, *providerregistry.Registry) (HostCapture, error)

// Capture transfers one provider lease and one host reference to the complete
// builder. HostIdentity must match its other captured host/auth dependencies.
type Capture struct {
	Identity, HostIdentity string
	ConfigProviders        []servingauth.ProviderSource
	ProviderResources      []servingauth.ProviderResource
	// Browser selects from this SAME retained resource lease and exact host.
	// Selection is pure; Exchange is explicit I/O outside all gates. A current
	// handle is NOT writer admission. Release invalidates every borrowed handle.
	Browser func(servingauth.ProviderRef, string) (*providerresources.Browser, error)
	Current func() bool
	Release func()
}

// CurrentCommandFunc is trusted root composition, NOT caller authorization.
// Root verifies RAW proof via ReauthorizeCurrent before mutations/dedup responses.
// Provider/registry/host mutations require root's exclusive replacement sequence.
type CurrentCommandFunc func(context.Context, *sqlite.CommandTx, servingauth.Dependencies) error

// AuthorityAdmissionFunc authorizes a complete proposed BOTH-origin inventory
// in the actual tx. It cannot borrow a principal from the unavailable provider.
// It performs no external effects, and nil ALWAYS denies durable maintenance.
type AuthorityAdmissionFunc func(context.Context, *sqlite.CommandTx, *Admission) error

// Options requires real SQLite and the complete publisher, never fake Stores.
// Run is always-on; RunWriter must receive the ORIGINAL elector token/context.
type Options struct {
	Store       *sqlite.Store
	Resources   *providerresources.Owner
	Schema      servingauth.Schema
	Publisher   *serving.Publisher
	Host        HostCaptureFunc
	HostChanges func() <-chan struct{}
	// PinHost retains actual root license/host replacement exclusion THROUGH
	// full Command/commit/no-op or token check+SetDesired, inside resource guard.
	PinHost                         func(context.Context, func() error) error
	Admit                           AuthorityAdmissionFunc
	Now                             func() time.Time
	RetryInterval, OperationTimeout time.Duration
}

// Coordinator has no independent serving or auth pointer. Notifications fan out
// from the single buffered resource consumer to complete publisher subscribers.
type Coordinator struct {
	opts                Options
	life                context.Context
	cancel              context.CancelFunc
	closed              atomic.Bool
	mu                  sync.Mutex
	changed             chan struct{}
	runDone, writerDone chan struct{}
	writerRunning       bool
}

// Admission is callback-scoped, opaque and detached; it is not a forged plan or
// resource provenance. Current candidates are verified privately before invoking it.
type Admission struct {
	active                       atomic.Bool
	registryDigest, hostIdentity string
	receipts                     []providerregistry.DesiredAuthority
	sources                      []servingauth.ProviderDescriptor
}

func (*Admission) String() string { return "private provider admission" }

// GoString redacts generic diagnostics.
func (a *Admission) GoString() string { return a.String() }

// MarshalJSON prevents accidental serialization of admission capabilities.
func (*Admission) MarshalJSON() ([]byte, error) { return []byte(`"private provider admission"`), nil }

// RegistryDigest identifies the exact canonical registry during the callback.
func (a *Admission) RegistryDigest() string {
	if a == nil || !a.active.Load() {
		return ""
	}
	return a.registryDigest
}

// HostIdentity identifies the pinned local installation during the callback.
func (a *Admission) HostIdentity() string {
	if a == nil || !a.active.Load() {
		return ""
	}
	return a.hostIdentity
}

// Sources returns the detached complete source projection during the callback.
func (a *Admission) Sources() []servingauth.ProviderDescriptor {
	if a == nil || !a.active.Load() {
		return nil
	}
	return cloneSources(a.sources)
}

// Receipts returns the detached complete proposed authority inventory.
func (a *Admission) Receipts() []providerregistry.DesiredAuthority {
	if a == nil || !a.active.Load() {
		return nil
	}
	out := make([]providerregistry.DesiredAuthority, len(a.receipts))
	for i, r := range a.receipts {
		out[i] = r
		out[i].Source = cloneSource(r.Source)
	}
	return out
}
