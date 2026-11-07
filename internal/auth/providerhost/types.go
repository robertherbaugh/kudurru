// Package providerhost owns immutable local provider policy and explicit cluster
// entitlement continuity. Root supplies loaded trust, real key exclusion, durable
// withdrawal, source publication and elector/peer lifecycle. No auth path does I/O.
package providerhost

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providercoordinator"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/credentials"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Safe errors do not include upstream, license, credential or key material.
var (
	ErrInvalid    = errors.New("providerhost: invalid input")
	ErrBounds     = errors.New("providerhost: bounds exceeded")
	ErrStale      = errors.New("providerhost: dependency changed")
	ErrContinuity = errors.New("providerhost: continuity unavailable")
	ErrWithdrawal = errors.New("providerhost: invalid withdrawal")
	ErrClosed     = errors.New("providerhost: closed")
)

// StaticInput contains already loaded trusted root resources. Preparation does
// not open files, read environment, resolve credentials or consult licensing.
// Current/Release and crypto handles must be pure immutable owner capabilities.
type StaticInput struct {
	Identity                                 string
	LocalProviders                           []providerregistry.DesiredConfig
	Auth                                     servingauth.Dependencies
	ObjectCredentials                        *credentials.Resolver
	ConfigCredentials                        map[string]*credentials.Resolver
	CredentialPolicy, FingerprintKeyID       string
	AllowedOrigins, LocalOrigins, Algorithms []string
	TrustPEM                                 []byte
	AuthKeyIdentity                          string
	Current                                  func() bool
	Release                                  func()
}

// StaticResources owns detached prevalidated input. Release relinquishes the
// preparer's reference; New/ReplaceStatic retain their own on success.
type StaticResources struct {
	input       StaticInput
	refs        atomic.Int64
	releaseOnce sync.Once
}

// WithdrawalFunc must stage empty BOTH-origin authority plus exact retired UID
// floors using root's existing security command. It must preserve other state.
type WithdrawalFunc func(context.Context, *sqlite.CommandTx, time.Time) error

// Options requires actual root owners and explicit withdrawal/key exclusion.
// There is no positive default admission or platform trust loading.
type Options struct {
	Store       *sqlite.Store
	Resources   *providerresources.Owner
	Publisher   *serving.Publisher
	License     *license.Manager
	Static      *StaticResources
	Schema      servingauth.Schema
	Now         func() time.Time
	PinAuthKeys func(context.Context, func() error) error
	Withdraw    WithdrawalFunc
}

type phase uint8

const (
	denied phase = iota
	waiting
	pending
	active
)

type generation struct {
	serial      uint64
	cluster     string
	static      *StaticResources
	license     license.Cut
	profile     string
	until       time.Time
	phase       phase
	entitlement string
	baseline    map[string]bool
	historyFull bool
	observed    bool
	writer      context.Context
	target      serving.TermActivation
}
type memo struct {
	key, identity string
	providers     *providerresources.HostCut
}

// Owner has one local immutable cursor, no separate serving or durable pointer.
// Host/CheckAdmission are guarded internal callbacks. All replacement methods
// acquire resource exclusion; never invoke them from PinHost/Command callbacks.
type Owner struct {
	opts      Options
	gate      sync.RWMutex
	value     atomic.Pointer[generation]
	closed    atomic.Bool
	changesMu sync.Mutex
	changes   chan struct{}
	memoMu    sync.Mutex
	memos     []memo
}

// String redacts private capabilities.
func (*Owner) String() string { return "private provider host" }

// GoString redacts private capabilities.
func (o *Owner) GoString() string { return o.String() }

// MarshalJSON redacts private capabilities.
func (*Owner) MarshalJSON() ([]byte, error) { return []byte(`"private provider host"`), nil }

// String redacts private resource material.
func (*StaticResources) String() string { return "private provider host resources" }

// GoString redacts private resource material.
func (s *StaticResources) GoString() string { return s.String() }

// MarshalJSON redacts private resource material.
func (*StaticResources) MarshalJSON() ([]byte, error) { return []byte(`"private host resources"`), nil }

// Compile-time callback shape remains the approved coordinator seam.
var _ providercoordinator.HostCaptureFunc = (*Owner)(nil).Host
