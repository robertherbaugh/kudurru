package host

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Lifecycle/execution errors are sanitized and safe to expose to operators.
var (
	ErrUnavailable  = errors.New("integration host: instance unavailable")
	ErrProvider     = errors.New("integration host: provider failed")
	ErrStalePreview = errors.New("integration host: preview changed")
	ErrUnsupported  = errors.New("integration host: capability unsupported")
	ErrBusy         = errors.New("integration host: command capacity exceeded")
	ErrFenced       = errors.New("integration host: incarnation fenced")
)

// InstanceID requires one exact captured Integration incarnation.
type InstanceID = kinds.Owner

// ExecutionGuard adds the captured publication/config bundle to the write guard.
type ExecutionGuard struct {
	Ledger   ledger.Guard
	BundleID string
}

// Publication describes a verified remote destination and its desired filters.
type Publication struct {
	Destination           kinds.Destination
	Domains               []string
	Selection             public.PublicationSelection
	Policy                plan.Policy
	AdoptUnowned          bool
	AdoptExternalDNSOwner string
}

// RemoteSnapshot must contain all destination records, including foreign rows.
type RemoteSnapshot struct {
	Records  []plan.RemoteRecord
	Complete bool
}

// Publisher is the stronger ownership/receipt seam. Legacy RecordSink alone
// cannot establish remote IDs, instance ownership, or durable partial results.
type Publisher interface {
	Destinations(context.Context) ([]Publication, error)
	Records(context.Context, kinds.Destination) (RemoteSnapshot, error)
	Adjust(context.Context, Publication, []public.Endpoint) ([]public.Endpoint, error)
	Apply(context.Context, *kinds.IntegrationOperation) (kinds.Receipt, error)
}

// RecoveryPublisher may resume an ambiguous intent only after a fresh complete
// reread and while the captured configuration still authorizes that intent.
// Adapters must reconcile observed owned IDs; replaying a create blindly is
// unsafe. Changed desired/configuration remains pending for operator recovery.
type RecoveryPublisher interface {
	Recover(context.Context, *kinds.IntegrationOperation, RemoteSnapshot) (kinds.Receipt, error)
}

// RequestHost is loaned to providers through the existing integration.Host.
// Each actual HTTP request (including retries) must reenter Request, so budgets
// and the active mutation fence cannot be bypassed by a batch/fallback loop.
type RequestHost interface {
	integration.Host
	Request(context.Context, string, string, bool, func(context.Context) error) error
}

// Preview binds all complete destination plans, not only one configured zone.
type Preview struct {
	Digest string
	Plans  []*plan.Plan
}

// Result reports only durable confirmed outcomes; ambiguity remains Degraded.
type Result struct {
	PreviewDigest                          string
	Applied, Pending, Conflicts, Unchanged int
	Clients                                int
	Source                                 *SourceOutcome
}

// SourceOutcome retains durable admission even when publication is pending.
// ReceiptID is a bounded opaque identifier, never provider response text.
type SourceOutcome struct {
	ReceiptID                          string
	Committed, Published, Deduplicated bool
}

// DesiredBatch enters root's existing admission/publication seam.
type DesiredBatch struct {
	Zones        []*public.DNSZone
	RecordSets   []*public.DNSRecordSet
	ClientGroups []*public.ClientGroup
	SourceID     string
}

// InstanceStatus contains sanitized local observations, never secret values.
type InstanceStatus struct {
	Owner                  InstanceID
	Phase                  string
	Reason                 string
	Leader                 bool
	LastSync               time.Time
	Result                 Result
	CredentialFingerprints map[string]string
}

// Reader supplies bounded background pages of private ledger objects.
type Reader interface {
	List(context.Context, store.Kind, store.ListOptions) (store.ObjectList, error)
}

// Options injects root-owned capture, authority, admission, and licensed wiring.
type Options struct {
	Capture   func(context.Context) (Input, error)
	Factory   func(string) (integration.Integration, error)
	Elector   func(string) store.Elector
	Fence     func(context.Context, ExecutionGuard) error
	Ledger    *ledger.Repository
	Reader    Reader
	Inventory *inventory.Holder
	Publish   func(context.Context, ExecutionGuard, DesiredBatch) error
	// Preferred source callbacks replace BOTH legacy Publish and Inventory.
	// ValidateSource is pure and runs before defensive copying. BeforeObserve
	// binds original root proofs once, retaining release through commit/ack.
	ValidateSource func(ExecutionGuard, SourceSnapshot) error
	CommitSource   func(context.Context, ExecutionGuard, SourceSnapshot) (*Result, error)
	BeforeObserve  func(requestCtx, originalLeaseCtx context.Context, guard ExecutionGuard) (boundCtx context.Context, release func(), err error)
	Status         func(context.Context, ExecutionGuard, InstanceStatus) error
	Resolve        func(context.Context, dns.Question) (*dns.Msg, error)
	// In preferred source mode Allowed must be a concurrency-safe, lock-free
	// memory/clock predicate: it is also invoked by the actual final check.
	// Root separately excludes original license/resource replacement.
	Allowed                                                     func() bool
	Logger                                                      *slog.Logger
	Metrics                                                     prometheus.Registerer
	Now                                                         func() time.Time
	MaxConcurrent                                               int
	PollInterval, CaptureTimeout, OperationTimeout, StopTimeout time.Duration
}

// Host is one bounded background coordinator. Run must be called only once.
type Host struct {
	opts            Options
	incarnation     string
	mu              sync.RWMutex
	jobs            map[string]*job
	statuses        map[string]InstanceStatus
	wake            chan struct{}
	slots           chan struct{}
	running         atomic.Bool
	sequence        atomic.Uint64
	requestSequence atomic.Uint64
	sourceSequence  atomic.Uint64
}
type frame struct {
	instance *Instance
	input    Input
}
type job struct {
	key          string
	frame        frame
	sourceDigest string
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	commands     chan command
	reconcile    chan struct{}
	guard        ledger.Guard
	leaseCtx     context.Context
	active       bool
	provider     integration.Integration
}
type command struct {
	ctx                    context.Context
	kind, expected, action string
	params                 json.RawMessage
	event                  *integration.Event
	reply                  chan answer
}
type answer struct {
	check   integration.Status
	preview *Preview
	result  *Result
	value   json.RawMessage
	err     error
}

// New validates bounded execution dependencies without doing I/O.
func New(o Options) (*Host, error) {
	if o.Capture == nil || o.Elector == nil || o.Fence == nil || o.Ledger == nil || o.Reader == nil {
		return nil, ErrInvalid
	}
	if (o.ValidateSource == nil) != (o.CommitSource == nil) || o.CommitSource != nil && o.BeforeObserve == nil {
		return nil, ErrInvalid
	}
	if o.Factory == nil {
		o.Factory = func(typ string) (integration.Integration, error) {
			_, ctor, ok := integration.Lookup(typ)
			if !ok || ctor == nil {
				return nil, ErrUnsupported
			}
			return ctor(), nil
		}
	}
	if o.Allowed == nil {
		o.Allowed = func() bool { return false }
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.MaxConcurrent == 0 {
		o.MaxConcurrent = 4
	}
	if o.MaxConcurrent < 1 || o.MaxConcurrent > 32 {
		return nil, ErrInvalid
	}
	if o.PollInterval == 0 {
		o.PollInterval = 2 * time.Second
	}
	if o.CaptureTimeout == 0 {
		o.CaptureTimeout = 10 * time.Second
	}
	if o.OperationTimeout == 0 {
		o.OperationTimeout = 30 * time.Second
	}
	if o.StopTimeout == 0 {
		o.StopTimeout = 5 * time.Second
	}
	if o.PollInterval < time.Millisecond || o.PollInterval > time.Minute || o.CaptureTimeout < time.Millisecond || o.CaptureTimeout > time.Minute || o.OperationTimeout < time.Millisecond || o.OperationTimeout > 2*time.Minute || o.StopTimeout < time.Millisecond || o.StopTimeout > time.Minute {
		return nil, ErrInvalid
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, ErrInvalid
	}
	return &Host{incarnation: hex.EncodeToString(nonce[:]), opts: o, jobs: map[string]*job{}, statuses: map[string]InstanceStatus{}, wake: make(chan struct{}, 1), slots: make(chan struct{}, o.MaxConcurrent)}, nil
}
