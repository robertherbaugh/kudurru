package ledger

import (
	"context"
	"errors"
	"math"
	"reflect"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const manager = "integration-ledger"

// ReadLedger returns validated, isolated baseline state with its exact RV.
func (r *Repository) ReadLedger(ctx context.Context, id kinds.RecordIdentity) (*kinds.IntegrationLedger, error) {
	if id.Validate() != nil {
		return nil, ErrInvalid
	}
	out := &kinds.IntegrationLedger{}
	if err := r.read.Get(ctx, store.Key{Kind: kinds.KindIntegrationLedger, Name: kinds.LedgerName(id)}, out); err != nil {
		return nil, err
	}
	if out.Validate() != nil || out.ResourceVersion == "" || out.Spec.Record != id {
		return nil, ErrInvalid
	}
	return out, nil
}

// ReadOperation returns a validated durable intent and monotone outcome.
func (r *Repository) ReadOperation(ctx context.Context, id string) (*kinds.IntegrationOperation, error) {
	if !kinds.ValidDigest(id) {
		return nil, ErrInvalid
	}
	out := &kinds.IntegrationOperation{}
	if err := r.read.Get(ctx, store.Key{Kind: kinds.KindIntegrationOperation, Name: kinds.OperationName(id)}, out); err != nil {
		return nil, err
	}
	if out.Validate() != nil || out.ResourceVersion == "" || out.Spec.OperationID != id {
		return nil, ErrInvalid
	}
	return out, nil
}
func guarded(g Guard, o *kinds.IntegrationOperation) bool {
	return g.Validate() == nil && o != nil && o.Validate() == nil && g.Owner == o.Spec.Record.Owner
}
func sameIntent(a, b *kinds.IntegrationOperation) bool {
	x, y := a.DeepCopy(), b.DeepCopy()
	x.Spec.Receipt, y.Spec.Receipt = nil, nil
	x.Spec.AbandonedAt, y.Spec.AbandonedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(x.Spec, y.Spec)
}
func (r *Repository) putOperation(ctx context.Context, g Guard, o *kinds.IntegrationOperation, create bool) (*kinds.IntegrationOperation, error) {
	if !guarded(g, o) || !create && o.ResourceVersion == "" {
		return nil, ErrInvalid
	}
	out, err := r.write(ctx, g, o.DeepCopy(), store.ApplyOptions{Manager: manager, ExpectedResourceVersion: o.ResourceVersion, CreateOnly: create})
	if err != nil {
		return nil, err
	}
	next, ok := out.(*kinds.IntegrationOperation)
	if !ok || next.Validate() != nil || next.ResourceVersion == "" || !reflect.DeepEqual(next.Spec, o.Spec) {
		return nil, ErrInvalid
	}
	return next.DeepCopy(), nil
}
func (r *Repository) putLedger(ctx context.Context, g Guard, l *kinds.IntegrationLedger, create bool) (*kinds.IntegrationLedger, error) {
	if g.Validate() != nil || l == nil || l.Validate() != nil || l.Spec.Record.Owner != g.Owner || !create && l.ResourceVersion == "" {
		return nil, ErrInvalid
	}
	out, err := r.write(ctx, g, l.DeepCopy(), store.ApplyOptions{Manager: manager, ExpectedResourceVersion: l.ResourceVersion, CreateOnly: create})
	if err != nil {
		return nil, err
	}
	next, ok := out.(*kinds.IntegrationLedger)
	if !ok || next.Validate() != nil || next.ResourceVersion == "" || !reflect.DeepEqual(next.Spec, l.Spec) {
		return nil, ErrInvalid
	}
	return next.DeepCopy(), nil
}

// Begin persists an immutable intent before reserving the exact ledger version.
// Empty expectedRV means an absent ledger, not permission to overwrite one.
// An orphaned intent from a failed reservation confers no provider permission.
func (r *Repository) Begin(ctx context.Context, g Guard, intent *kinds.IntegrationOperation, expectedRV string) (*Reservation, error) {
	if !guarded(g, intent) || intent.Spec.Receipt != nil || intent.ResourceVersion != "" || intent.Spec.ConfigDigest != g.ConfigDigest || intent.Spec.CredentialDigest != g.CredentialDigest || !intent.Spec.AbandonedAt.IsZero() {
		return nil, ErrInvalid
	}
	l, err := r.ReadLedger(ctx, intent.Spec.Record)
	absent := errors.Is(err, store.ErrNotFound)
	if err != nil && !absent {
		return nil, err
	}
	if absent {
		if expectedRV != "" {
			return nil, store.ErrConflict
		}
		l = &kinds.IntegrationLedger{ObjectMeta: metav1.ObjectMeta{Name: kinds.LedgerName(intent.Spec.Record)}, Spec: kinds.IntegrationLedgerSpec{FormatVersion: kinds.FormatVersion, Record: intent.Spec.Record, Sequence: 1}}
	} else if expectedRV == "" || l.ResourceVersion != expectedRV {
		return nil, store.ErrConflict
	}
	if l.Spec.Pending != nil && l.Spec.Pending.OperationID != intent.Spec.OperationID {
		return nil, ErrPending
	}
	if l.Spec.LastOperationID == intent.Spec.OperationID && l.Spec.Pending == nil {
		return nil, ErrPending
	}
	op, err := r.ReadOperation(ctx, intent.Spec.OperationID)
	if errors.Is(err, store.ErrNotFound) {
		op, err = r.putOperation(ctx, g, intent, true)
		// Another node may win create-only; immutable equality permits replay.
		if errors.Is(err, store.ErrConflict) {
			op, err = r.ReadOperation(ctx, intent.Spec.OperationID)
		}
	}
	if err != nil {
		return nil, err
	}
	if !sameIntent(op, intent) {
		return nil, ErrInvalid
	}
	pending := &kinds.Pending{OperationID: op.Spec.OperationID, PlanDigest: op.Spec.PlanDigest, BeforeHash: l.Spec.ContentHash, AfterHash: op.Spec.AfterHash}
	if l.Spec.Pending != nil {
		if !reflect.DeepEqual(l.Spec.Pending, pending) {
			return nil, ErrInvalid
		}
		return &Reservation{Ledger: l.DeepCopy(), Operation: op.DeepCopy()}, nil
	}
	if op.Spec.Receipt != nil || !op.Spec.AbandonedAt.IsZero() {
		return nil, ErrPending
	}
	if !absent {
		if l.Spec.Sequence == math.MaxUint64 {
			return nil, ErrOverflow
		}
		l.Spec.Sequence++
	}
	l.Spec.Pending = pending
	l, err = r.putLedger(ctx, g, l, absent)
	if err != nil {
		return nil, err
	}
	return &Reservation{Ledger: l, Operation: op}, nil
}

func (r *Repository) currentOperation(ctx context.Context, g Guard, op *kinds.IntegrationOperation) (*kinds.IntegrationOperation, error) {
	if !guarded(g, op) || op.ResourceVersion == "" {
		return nil, ErrInvalid
	}
	current, err := r.ReadOperation(ctx, op.Spec.OperationID)
	if err != nil {
		return nil, err
	}
	if current.ResourceVersion != op.ResourceVersion {
		return nil, store.ErrConflict
	}
	if !reflect.DeepEqual(current.Spec, op.Spec) {
		return nil, ErrInvalid
	}
	return current, nil
}

// RecordReceipt saves monotone partial/confirmed facts with the supplied exact
// operation RV. It never advances the ledger's last-good baseline.
func (r *Repository) RecordReceipt(ctx context.Context, g Guard, op *kinds.IntegrationOperation, receipt kinds.Receipt) (*kinds.IntegrationOperation, error) {
	current, err := r.currentOperation(ctx, g, op)
	if err != nil {
		return nil, err
	}
	if !current.Spec.AbandonedAt.IsZero() {
		return nil, ErrPending
	}
	next := current.DeepCopy()
	next.Spec.Receipt = &receipt
	// Deep-copy caller slices before passing to the writer.
	next = next.DeepCopy()
	if next.Validate() != nil {
		return nil, ErrInvalid
	}
	if !receiptProgress(current.Spec.Receipt, next.Spec.Receipt) {
		return nil, ErrReceipt
	}
	return r.putOperation(ctx, g, next, false)
}
func receiptProgress(old, next *kinds.Receipt) bool {
	if old == nil {
		return true
	}
	if next.ObservedAt.Before(old.ObservedAt) {
		return false
	}
	if old.Complete {
		return reflect.DeepEqual(old, next)
	}
	for _, before := range old.Targets {
		found := false
		for _, after := range next.Targets {
			if before.Target == after.Target {
				found = true
				if before.Applied && (!after.Applied || after.ProviderID != before.ProviderID) {
					return false
				}
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func (r *Repository) currentReservation(ctx context.Context, g Guard, res *Reservation) (*kinds.IntegrationLedger, *kinds.IntegrationOperation, error) {
	if res == nil || res.Ledger == nil || res.Operation == nil || res.Ledger.Validate() != nil || res.Ledger.ResourceVersion == "" {
		return nil, nil, ErrInvalid
	}
	op, err := r.currentOperation(ctx, g, res.Operation)
	if err != nil {
		return nil, nil, err
	}
	l, err := r.ReadLedger(ctx, op.Spec.Record)
	if err != nil {
		return nil, nil, err
	}
	if l.ResourceVersion != res.Ledger.ResourceVersion {
		return nil, nil, store.ErrConflict
	}
	if !reflect.DeepEqual(l.Spec, res.Ledger.Spec) || l.Spec.Record != op.Spec.Record || l.Spec.Pending == nil || l.Spec.Pending.OperationID != op.Spec.OperationID || l.Spec.Pending.PlanDigest != op.Spec.PlanDigest || l.Spec.Pending.AfterHash != op.Spec.AfterHash {
		return nil, nil, ErrInvalid
	}
	if l.Spec.Sequence == math.MaxUint64 {
		return nil, nil, ErrOverflow
	}
	return l, op, nil
}

// Confirm advances baseline only from a complete durable matching receipt and
// the exact still-pending ledger. A stale lease/config never publishes Synced.
func (r *Repository) Confirm(ctx context.Context, g Guard, res *Reservation) (*kinds.IntegrationLedger, error) {
	l, op, err := r.currentReservation(ctx, g, res)
	if err != nil {
		return nil, err
	}
	receipt := op.Spec.Receipt
	if receipt == nil || !receipt.Complete || !op.Spec.AbandonedAt.IsZero() {
		return nil, ErrPending
	}
	l.Spec.Sequence++
	l.Spec.ContentHash = receipt.ContentHash
	l.Spec.ProviderIDs = append([]string(nil), receipt.ProviderIDs...)
	l.Spec.Policy = op.Spec.Policy
	l.Spec.LastOperationID = op.Spec.OperationID
	l.Spec.LastApplied = receipt.ObservedAt
	l.Spec.Pending = nil
	return r.putLedger(ctx, g, l, false)
}

// Abandon requires a fresh complete provider observation of untouched Before
// state. Ambiguous/partial applied work stays pending for explicit recovery.
func (r *Repository) Abandon(ctx context.Context, g Guard, res *Reservation, proof Observation) (*kinds.IntegrationLedger, error) {
	l, op, err := r.currentReservation(ctx, g, res)
	if err != nil {
		return nil, err
	}
	if !ValidateObservationTime(proof.ObservedAt, r.now(), time.Minute) || !proof.ObservedAt.After(op.Spec.CreatedAt) || proof.BeforeHash != op.Spec.BeforeHash {
		return nil, ErrPending
	}
	if receipt := op.Spec.Receipt; receipt != nil {
		if receipt.Complete || proof.ObservedAt.Before(receipt.ObservedAt) {
			return nil, ErrPending
		}
		for _, t := range receipt.Targets {
			if t.Applied {
				return nil, ErrPending
			}
		}
	}
	// Terminalize first with operation CAS; a receipt racing this transition
	// conflicts or observes AbandonedAt, so it cannot resurrect released work.
	if op.Spec.AbandonedAt.IsZero() {
		op.Spec.AbandonedAt = proof.ObservedAt
		if _, err := r.putOperation(ctx, g, op, false); err != nil {
			return nil, err
		}
	}
	l.Spec.Sequence++
	l.Spec.Pending = nil
	return r.putLedger(ctx, g, l, false)
}

// ValidateObservationTime is available to hosts that pin their current time.
// Repository methods accept only authenticated host evidence, not user input.
func ValidateObservationTime(observed, now time.Time, maxAge time.Duration) bool {
	return maxAge > 0 && !observed.IsZero() && !observed.After(now) && now.Sub(observed) <= maxAge
}
