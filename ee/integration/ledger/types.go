// Package ledger implements a durable, optimistic publication protocol. It
// never invokes providers and never substitutes newer resource versions.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// Repository errors distinguish recovery from validation and CAS conflicts.
var (
	ErrInvalid  = errors.New("integration ledger: invalid input")
	ErrPending  = errors.New("integration ledger: recovery required")
	ErrReceipt  = errors.New("integration ledger: receipt regression")
	ErrOverflow = errors.New("integration ledger: sequence overflow")
	// ErrBaseline refuses to forget a record whose last confirmed state still
	// lists provider content and whose absence the host has not observed.
	ErrBaseline = errors.New("integration ledger: baseline still records provider content")
)

// Guard pins one captured instance and lease incarnation. Root's writer must
// check these against current authority at its commit boundary, including
// canceled/replaced leases and config A-B-A. CredentialDigest is a keyed
// fingerprint, never the credential value.
type Guard struct {
	Owner                          kinds.Owner
	ConfigDigest, CredentialDigest string
	IntegrationResourceVersion     string
	LeaseID                        string
	Term                           replication.Term
}

// Validate checks complete captured identity and authority tokens.
func (g Guard) Validate() error {
	if g.Owner.Validate() != nil || !kinds.ValidDigest(g.ConfigDigest) || !kinds.ValidDigest(g.CredentialDigest) || g.IntegrationResourceVersion == "" || len(g.IntegrationResourceVersion) > 512 || g.LeaseID == "" || len(g.LeaseID) > 256 || g.Term.Validate() != nil || g.Term.ClusterID != g.Owner.ClusterID {
		return ErrInvalid
	}
	return nil
}

// Write must implement create-only/exact CAS AND Guard checks at its commit
// boundary. A preliminary IsLeader/Fence followed by ordinary Apply is
// insufficient. It must not use Force or silently retry with fresh RVs.
//
// A carrier with transactions (SQLite) checks Guard in the same commit as
// the write. A carrier without them (Kubernetes, D384) checks Guard by a
// linearizable read of the Lease and a live read of the Integration
// immediately before the one exact create or exact-resourceVersion write,
// and writes only the private integration kinds. A write already sent after
// a passing check may still land once the Lease is lost; it is then either a
// serializable predecessor of the successor's CAS or a conflict, and a
// pending intent left by it forces a fresh complete reread by any writer.
type Write func(context.Context, Guard, store.Object, store.ApplyOptions) (store.Object, error)

// Delete must remove exactly the requested objects AND check Guard in the same
// commit boundary, like Write. Every request carries the UID and resource
// version the repository observed; a mismatch or a missing object fails the
// call. A carrier with multi-object transactions commits all requests or
// none. Any other applies them in the given order and stops at the first
// failure, which is why the ledger row comes first: its exact-version delete
// is the point a concurrent reservation of the same record conflicts with.
// It must not use Force or retry against fresh identities.
type Delete func(context.Context, Guard, []store.DeleteRequest) error

// Reader provides background-only durable reads.
type Reader interface {
	Get(context.Context, store.Key, store.Object) error
}

// Options requires an injected commit-fenced writer and deterministic clock.
// Delete is needed only by Forget; a repository without it cannot forget.
type Options struct {
	Now    func() time.Time
	Reader Reader
	Write  Write
	Delete Delete
}

// Repository has no mutable per-instance state or provider access.
type Repository struct {
	read   Reader
	write  Write
	delete Delete
	now    func() time.Time
}

// New rejects a missing reader or commit-fenced writer.
func New(o Options) (*Repository, error) {
	if o.Reader == nil || o.Write == nil {
		return nil, fmt.Errorf("%w: reader and fenced writer required", ErrInvalid)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Repository{read: o.Reader, write: o.Write, delete: o.Delete, now: o.Now}, nil
}

// Reservation is the exact durable pending baseline and intent. It permits a
// caller to proceed only after Begin succeeds and its provider fence succeeds.
// Returned objects are caller-owned copies, not repository mutable state.
type Reservation struct {
	Ledger    *kinds.IntegrationLedger
	Operation *kinds.IntegrationOperation
}

// ErrorUndone is the receipt error class of an acknowledged effect a later
// complete capture proved gone, recorded when its intent is abandoned.
const ErrorUndone = "undone"

// Observation is fresh provider evidence for abandoning an untouched intent,
// or for forgetting a record the provider no longer holds: BeforeHash is the
// content digest observed for the RRset, empty for absence.
// Host must verify complete remote content and ownership in the same capture;
// the repository cannot authenticate remote evidence itself.
//
// Gone lists provider IDs the same complete capture shows absent. Abandon
// accepts an intent with acknowledged (Applied) receipt facts only when every
// such fact names a provider ID listed here and outside the intent's baseline
// (D401): every acknowledged effect is then proven undone.
type Observation struct {
	BeforeHash string
	ObservedAt time.Time
	Gone       []string
}
