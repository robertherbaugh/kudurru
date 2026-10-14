// Package primaryruntime coordinates committed primary lifecycle, admission and
// publication. Store and network operations occur only in background/command
// methods; Capture returns an immutable memory-only routing/membership cut.
package primaryruntime

import (
	"context"
	"errors"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

// Runtime errors fail closed when admission, identity or finite bounds fail.
var (
	ErrUnavailable = errors.New("primaryruntime: required adapter/publication unavailable")
	ErrInvalid     = errors.New("primaryruntime: invalid identity or transition")
	ErrConflict    = errors.New("primaryruntime: admission conflict")
	ErrCapacity    = errors.New("primaryruntime: capacity exceeded")
)

// LifecycleCAS must check the EXACT prior object RV/spec, next transition,
// actual current source equality token and HA authority at the driver boundary.
// Nil current is CREATE ONLY. It cannot substitute a later RV or retry itself.
type LifecycleCAS func(context.Context, *zk.ZoneLifecycle, *zk.ZoneLifecycle) (*zk.ZoneLifecycle, error)

// Admission covers head CAS, fencing, retirement, notification acknowledgment,
// no-op and dedup. Every call rechecks lifecycle/source/domain/HA in the driver's
// commit boundary. Prepared nil means authorize/check the exact existing head;
// an empty Expected with nil Prepared is a nonprimary membership admission.
type Admission func(context.Context, Command) (*zonestate.View, error)

// Command binds a single admission to exact lifecycle and head captures.
type Command struct {
	Lifecycle *zk.ZoneLifecycle
	Parent    *zk.ZoneLifecycle
	Expected  zonestate.Expected
	Prepared  *zonestate.Prepared
	Reason    string
	Proof     *ProofIdentity
	// Desired is the original complete actual source read for reconciliation,
	// before effective-head UID mapping. The driver rechecks every equality
	// token AND the complete relevant set in its command transaction.
	Desired *zonestate.DesiredInputs
}

// Publication returns success only after root's COMPLETE serving bundle has
// acknowledged the exact lifecycle incarnation/head and routing cut supplied.
type Publication func(context.Context, Cut, Member) error

// Options supplies bounded background adapters; nil adapters disable writes.
type Options struct {
	Store                 store.Store
	Repository            *zonestate.Repository
	Reader                zonestate.DesiredReader
	Compiler              zonestate.DesiredCompiler
	LifecycleCAS          LifecycleCAS
	Admit                 Admission
	Publish               Publication
	Now                   func() time.Time
	Limits                zonestate.Limits
	UpdateLimits          dynamicupdate.Limits
	MaxZones, MaxAttempts int
	Timeout               time.Duration
	Proofs                ProofVerifier
	Notify                NotifyAdapter
}

// ProofVerifier is root's immutable verification capability. It must bind the
// verification-time provider version, canonical key/algorithm, transport and
// immutable message semantics. A public key string is not a production proof.
type ProofVerifier interface {
	Verify(context.Context, any, []byte) (ProofIdentity, error)
}

// ProofIdentity is verification-time metadata from the opaque root issuer.
type ProofIdentity struct {
	Principal                                                                       dynamicupdate.Principal
	ProviderVersion, Algorithm, Transport, Domain, Subject, RetryID, SemanticDigest string
	Term                                                                            replication.Term
	AuthorityIncarnation                                                            uint64
	// NotAfter is the verification-time signature deadline. Admission carries
	// it through the entire transaction, including writer-lock waits and commit.
	NotAfter time.Time
}

// VerifiedUpdate carries detached semantics and an opaque verification proof.
type VerifiedUpdate struct {
	Zone  zk.ZoneID
	Wire  []byte
	Proof any
}

// UpdateOutcome separates wire response from internal durable/publication state.
type UpdateOutcome struct {
	Rcode                              int
	Committed, Published, Deduplicated bool
	Version                            zonestate.Version
	Serial                             uint32
	Err                                error
}

// NotifyAdapter is root's bounded sender using frozen model policy and current
// credential publication. Success is an attempt result, NEVER a durable ack.
type NotifyAdapter interface {
	Send(context.Context, Member, *zone.Zone) error
}

// Member is a value with detached obligations; callers cannot mutate the cut.
type Member struct {
	Zone                  zk.ZoneID
	Type                  public.ZoneType
	Incarnation, Sequence uint64
	Phase                 string
	Version               zonestate.Version
	Published             bool
	Parents               []zk.ParentFence
}

type entry struct {
	lifecycle       *zk.ZoneLifecycle
	view            *zonestate.View
	model, coverage *zone.Zone
	member          Member
	expiry          time.Time
}

func version(v *zonestate.View) zonestate.Version {
	if v == nil || v.Head == nil {
		return zonestate.Version{}
	}
	return zonestate.Version{Zone: v.Head.Spec.Zone, CommitSequence: v.Head.Spec.CommitSequence, RootDigest: v.Head.Spec.RootDigest}
}
func outcome(err error) UpdateOutcome { return UpdateOutcome{Rcode: dns.RcodeServerFailure, Err: err} }
