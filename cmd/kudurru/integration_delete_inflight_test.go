package main

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Mounted acceptance of the SQLite Delete lifecycle (D367) for a delete batch
// whose outcome is not known while the same runtime keeps running: the
// fixture, adapter, SDK and TLS peer are those of integration_delete_test.go.

// TestIntegrationDeleteSettlesAnAmbiguousBatchWithoutRestart is the
// ambiguous case of TestIntegrationDeleteResumesAfterRestart without the
// restart: the provider applies the delete batch but answers 503 and then
// cannot list, so the intent stays pending. Once the provider answers again,
// the same runtime's next run confirms the absence by a reread, sends no
// second batch, and finishes.
func TestIntegrationDeleteSettlesAnAmbiguousBatchWithoutRestart(t *testing.T) {
	f := newIntegrationDeleteFixture(t, func(d *public.Integration) { d.Spec.Resync.Duration = time.Second })
	_, writes, _ := f.traffic()
	f.peer.mu.Lock()
	f.peer.ambiguous = true
	f.peer.mu.Unlock()
	f.requestDelete(t)
	awaitServingRuntime(t, f.s, func() bool {
		_, after, _ := f.traffic()
		return after == writes+1 && f.ledgerPending(t)
	})
	time.Sleep(time.Second)
	if _, after, _ := f.traffic(); after != writes+1 || !f.ledgerPending(t) {
		t.Fatal("an unacknowledged batch was sent again or settled without a reread", after-writes)
	}
	if held := integrationStored(t, f.s, f.d); held == nil || !store.HasFinalizer(held, store.IntegrationCleanupFinalizer) {
		t.Fatal("the instance was finalized over an unsettled batch")
	}
	// The peer's ambiguous mode also made its list unreadable; it answers
	// both again.
	f.peer.mu.Lock()
	f.peer.ambiguous, f.peer.unreadable = false, false
	f.peer.mu.Unlock()
	f.s.integrations.host.Wake()
	f.awaitGone(t)
	f.expectOwnedDeletedOnce(t, writes, 1)
}

// TestIntegrationDeleteReleaseDuringAHeldBatchLeavesItsIntentPending
// releases a terminating instance through the reviewed release command while
// its cleanup's delete batch is held at the provider. The release takes no
// per-instance lease, so it is not blocked by the held run, and it needs no
// provider. When the held batch then completes at the provider, the run that
// sent it is fenced: it records no receipt, sends nothing more, and writes
// no cleanup audit. The ledger row keeps its pending intent, as for any
// released incarnation.
func TestIntegrationDeleteReleaseDuringAHeldBatchLeavesItsIntentPending(t *testing.T) {
	f := newIntegrationDeleteFixture(t, nil)
	admin, router, _ := integrationDeleteAdmin(t, f)
	hold, entered := make(chan struct{}), make(chan struct{})
	var released atomic.Bool
	releaseHold := func() {
		if released.CompareAndSwap(false, true) {
			close(hold)
		}
	}
	defer releaseHold()
	_, writes, _ := f.traffic()
	f.peer.mu.Lock()
	f.peer.hold, f.peer.entered = hold, entered
	f.peer.mu.Unlock()
	f.requestDelete(t)
	integrationAwaitSignal(t, entered)
	if !f.ledgerPending(t) {
		t.Fatal("the batch reached the provider before its intent was durable")
	}
	held := integrationStored(t, f.s, f.d)
	if held == nil || held.DeletionTimestamp == nil {
		t.Fatal("the instance is not terminating while its batch is held")
	}
	summary := integrationSummaryOf(t, router, admin, "/integrations/default/publisher")
	if !summary.Terminating || summary.Revision != held.ResourceVersion {
		t.Fatalf("summary while the batch is held: %+v", summary)
	}
	// The release answers while the batch is still held: a release that
	// waited for the run would block here until the test's deadline.
	w := integrationAdminCall(t, router, admin, http.MethodPost, "/integrations/default/publisher/release", api.IntegrationReview{UID: summary.UID, Revision: summary.Revision}, admin.token)
	code, body := w.Code, w.Body.Bytes()
	var result api.IntegrationReleaseResult
	if code != http.StatusOK || json.Unmarshal(body, &result) != nil || !result.Released || result.LedgerRows != 1 {
		t.Fatalf("release during a held batch: %d %s", code, body)
	}
	if integrationStored(t, f.s, f.d) != nil {
		t.Fatal("the released object remains")
	}
	// The held batch now completes at the provider.
	releaseHold()
	awaitServingRuntime(t, f.s, func() bool {
		_, _, deleted := f.traffic()
		return len(deleted) == len(f.published)
	})
	f.s.integrations.host.Wake()
	awaitServingRuntime(t, f.s, func() bool { return f.phase() == "" })
	time.Sleep(time.Second)
	f.s.integrations.host.Wake()
	time.Sleep(300 * time.Millisecond)
	if _, after, _ := f.traffic(); after != writes+1 {
		t.Fatal("the released cleanup sent another batch", after-writes)
	}
	rows := integrationRows(t, f.s, kinds.KindIntegrationLedger)
	if len(rows) != 1 || rows[0].(*kinds.IntegrationLedger).Spec.Pending == nil {
		t.Fatal("the released incarnation's ledger row did not keep its pending intent", len(rows))
	}
	operation := false
	for _, row := range integrationRows(t, f.s, kinds.KindIntegrationOperation) {
		op := row.(*kinds.IntegrationOperation)
		if op.Spec.Action != "delete" {
			continue
		}
		operation = true
		if op.Spec.Receipt != nil {
			t.Fatal("the fenced run recorded a receipt after the release", op.Spec.Receipt)
		}
	}
	if !operation {
		t.Fatal("the held delete intent was not retained")
	}
	actions := map[string]int{}
	for _, entry := range integrationAudit(t, f.s, f.d) {
		actions[entry.Action]++
	}
	if actions["integration.release"] != 1 || actions["integration.cleanup"] != 0 {
		t.Fatalf("audit after a release during a held batch: %v", actions)
	}
}
