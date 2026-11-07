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

// Write must implement create-only/exact CAS AND Guard checks in the same
// commit boundary. A preliminary IsLeader/Fence followed by ordinary Apply
// is insufficient. It must not use Force or silently retry with fresh RVs.
type Write func(context.Context, Guard, store.Object, store.ApplyOptions) (store.Object, error)

// Reader provides background-only durable reads.
type Reader interface {
	Get(context.Context, store.Key, store.Object) error
}

// Options requires an injected commit-fenced writer and deterministic clock.
type Options struct {
	Now    func() time.Time
	Reader Reader
	Write  Write
}

// Repository has no mutable per-instance state or provider access.
type Repository struct {
	read  Reader
	write Write
	now   func() time.Time
}

// New rejects a missing reader or commit-fenced writer.
func New(o Options) (*Repository, error) {
	if o.Reader == nil || o.Write == nil {
		return nil, fmt.Errorf("%w: reader and fenced writer required", ErrInvalid)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Repository{read: o.Reader, write: o.Write, now: o.Now}, nil
}

// Reservation is the exact durable pending baseline and intent. It permits a
// caller to proceed only after Begin succeeds and its provider fence succeeds.
// Returned objects are caller-owned copies, not repository mutable state.
type Reservation struct {
	Ledger    *kinds.IntegrationLedger
	Operation *kinds.IntegrationOperation
}

// Observation is fresh provider evidence for abandoning an untouched intent.
// Host must verify complete remote content and ownership in the same capture;
// the repository cannot authenticate remote evidence itself.
type Observation struct {
	BeforeHash string
	ObservedAt time.Time
}
