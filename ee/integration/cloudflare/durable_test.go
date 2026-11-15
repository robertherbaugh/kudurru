package cloudflare

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// The carrier here is a test fixture, not production admission. It checks exact
// Guard identity at each durable write and carries reservations/receipts through
// repository recreation, while all provider requests use the real SDK.
func TestSDKDurableReservationReceiptFailureAndRestart(t *testing.T) {
	f := newFixture(t)
	op := f.intent(f.adjusted(endpoint("192.0.2.1", "192.0.2.2")))
	st := memory.New()
	guard := ledger.Guard{Owner: f.h.owner, ConfigDigest: op.Spec.ConfigDigest, CredentialDigest: op.Spec.CredentialDigest, IntegrationResourceVersion: "1", LeaseID: "lease-A", Term: replication.Term{ClusterID: f.h.owner.ClusterID, Epoch: 1, PrimaryID: "primary", PromotionID: "promotion"}}
	var mu sync.Mutex
	current := guard
	rejectReceipt := false
	write := func(ctx context.Context, g ledger.Guard, o store.Object, a store.ApplyOptions) (store.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if g != current || ctx.Err() != nil {
			return nil, host.ErrFenced
		}
		if rejectReceipt && store.KindOf(o) == "IntegrationOperation" {
			return nil, errors.New("fixture receipt failure")
		}
		return st.Apply(ctx, o, a)
	}
	repository, err := ledger.New(ledger.Options{Reader: st, Write: write})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := repository.Begin(f.ctx, guard, op, "")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := repository.ReadOperation(f.ctx, op.Spec.OperationID)
	if err != nil || saved.Spec.Receipt != nil || saved.ResourceVersion == "" {
		t.Fatal("intent not durable", saved, err)
	}
	receipt, err := f.p.Apply(f.ctx, reservation.Operation)
	if err != nil || !receipt.Complete {
		t.Fatal(receipt, err)
	}
	mu.Lock()
	rejectReceipt = true
	mu.Unlock()
	if _, err = repository.RecordReceipt(f.ctx, guard, reservation.Operation, receipt); err == nil {
		t.Fatal("receipt failure bypassed")
	}
	// A restarted executor has no remembered acknowledgment; it must reread IDs.
	repository, err = ledger.New(ledger.Options{Reader: st, Write: write})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := repository.ReadLedger(f.ctx, op.Spec.Record)
	if err != nil || pending.Spec.Pending == nil || pending.Spec.ContentHash != "" {
		t.Fatal("premature ledger confirmation", pending, err)
	}
	receipt, err = f.p.Recover(f.ctx, saved, host.RemoteSnapshot{Complete: true})
	if err != nil || !receipt.Complete {
		t.Fatal(receipt, err)
	}
	mu.Lock()
	rejectReceipt = false
	mu.Unlock()
	saved, err = repository.RecordReceipt(f.ctx, guard, saved, receipt)
	if err != nil {
		t.Fatal(err)
	}
	reservation.Operation = saved
	confirmed, err := repository.Confirm(f.ctx, guard, reservation)
	if err != nil || confirmed.Spec.Pending != nil || confirmed.Spec.ContentHash != op.Spec.AfterHash || len(confirmed.Spec.ProviderIDs) != 2 {
		t.Fatal(confirmed, err)
	}
	f.mu.Lock()
	batches := len(f.batches)
	f.mu.Unlock()
	if batches != 1 {
		t.Fatal("restart duplicated create", batches)
	}
	mu.Lock()
	current.LeaseID = "lease-B"
	mu.Unlock()
	if _, err = repository.RecordReceipt(f.ctx, guard, saved, receipt); err == nil || strings.Contains(err.Error(), testToken) {
		t.Fatal("obsolete guard or unsafe error", err)
	}
}
