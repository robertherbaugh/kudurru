package ledger

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

var testTime = time.Unix(100, 0).UTC()

func fixture(t testing.TB) (Guard, *kinds.IntegrationOperation) {
	t.Helper()
	g := Guard{Owner: kinds.Owner{ClusterID: "cluster", Namespace: "dns", Name: "public", UID: "uid-1"}, ConfigDigest: kinds.Digest([]byte("config")), CredentialDigest: kinds.Digest([]byte("credential")), IntegrationResourceVersion: "rv-1", LeaseID: "lease-1", Term: replication.Term{ClusterID: "cluster", Epoch: 1, PrimaryID: "primary", PromotionID: "promotion"}}
	p, err := plan.Build(plan.Input{Owner: g.Owner, Destination: kinds.Destination{Provider: "test", Account: "account", Zone: "example.com"}, ConfigDigest: g.ConfigDigest, CredentialDigest: g.CredentialDigest, SourceDigest: kinds.Digest([]byte("source")), DesiredComplete: true, CurrentComplete: true, Desired: []public.Endpoint{{DNSName: "a.example.com.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.1", "192.0.2.2"}}}})
	if err != nil {
		t.Fatal(err)
	}
	return g, p.Intent(0, testTime)
}
func repository(t testing.TB, st *memory.Store, write Write) *Repository {
	t.Helper()
	if write == nil {
		write = func(ctx context.Context, _ Guard, o store.Object, opts store.ApplyOptions) (store.Object, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !opts.CreateOnly && opts.ExpectedResourceVersion == "" || opts.Force {
				t.Fatal("unconditional mutation")
			}
			return st.Apply(ctx, o, opts)
		}
	}
	r, err := New(Options{Reader: st, Write: write, Now: func() time.Time { return testTime.Add(10 * time.Second) }})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func begin(t testing.TB, r *Repository, g Guard, op *kinds.IntegrationOperation, rv string) *Reservation {
	t.Helper()
	res, err := r.Begin(context.Background(), g, op, rv)
	if err != nil {
		t.Fatal(err)
	}
	return res
}
func complete(op *kinds.IntegrationOperation) kinds.Receipt {
	return kinds.Receipt{Complete: true, ContentHash: op.Spec.AfterHash, ProviderIDs: []string{"provider-1", "provider-2"}, ObservedAt: testTime.Add(time.Second), Targets: []kinds.TargetResult{{Target: "192.0.2.1", ProviderID: "provider-1", Applied: true}, {Target: "192.0.2.2", ProviderID: "provider-2", Applied: true}}}
}
func TestDurabilityBoundariesAndRestart(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	g, op := fixture(t)
	fail := true
	r := repository(t, st, func(ctx context.Context, _ Guard, o store.Object, opts store.ApplyOptions) (store.Object, error) {
		if fail && store.KindOf(o) == kinds.KindIntegrationLedger {
			return nil, store.ErrClosed
		}
		return st.Apply(ctx, o, opts)
	})
	if _, err := r.Begin(ctx, g, op, ""); !errors.Is(err, store.ErrClosed) {
		t.Fatal(err)
	}
	if _, err := r.ReadOperation(ctx, op.Spec.OperationID); err != nil {
		t.Fatal("intent not durable before reservation", err)
	}
	if _, err := r.ReadLedger(ctx, op.Spec.Record); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("failed reservation published", err)
	}
	fail = false
	r = repository(t, st, nil)
	res := begin(t, r, g, op, "")
	if res.Ledger.Spec.ContentHash != "" || res.Ledger.Spec.Pending == nil {
		t.Fatal("reservation advanced baseline")
	}
	// A new repository recovers a crash after provider success but before receipt.
	r = repository(t, st, nil)
	replay := begin(t, r, g, op, res.Ledger.ResourceVersion)
	if replay.Operation.Spec.Receipt != nil {
		t.Fatal("unobserved remote result claimed")
	}
	if _, err := r.Confirm(ctx, g, replay); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}
	partial := kinds.Receipt{ObservedAt: testTime.Add(time.Second), Targets: []kinds.TargetResult{{Target: "192.0.2.1", ProviderID: "provider-1", Applied: true}}}
	saved, err := r.RecordReceipt(ctx, g, replay.Operation, partial)
	if err != nil {
		t.Fatal(err)
	}
	replay.Operation = saved
	if _, err := r.Confirm(ctx, g, replay); !errors.Is(err, ErrPending) {
		t.Fatal("partial baseline confirmed", err)
	}
	receipt := complete(op)
	receipt.ObservedAt = receipt.ObservedAt.Add(time.Second)
	saved, err = r.RecordReceipt(ctx, g, saved, receipt)
	if err != nil {
		t.Fatal(err)
	}
	replay.Operation = saved
	// Durable complete receipt survives restart; ledger is not yet confirmed.
	r = repository(t, st, nil)
	l, err := r.ReadLedger(ctx, op.Spec.Record)
	if err != nil || l.Spec.ContentHash != "" {
		t.Fatal("receipt changed baseline", err)
	}
	saved, err = r.ReadOperation(ctx, op.Spec.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := r.Confirm(ctx, g, &Reservation{Ledger: l, Operation: saved})
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Spec.Pending != nil || confirmed.Spec.ContentHash != op.Spec.AfterHash || confirmed.Spec.LastOperationID != op.Spec.OperationID || confirmed.Spec.Sequence != 2 {
		t.Fatalf("baseline=%+v", confirmed.Spec)
	}
	if _, err := r.Begin(ctx, g, op, confirmed.ResourceVersion); !errors.Is(err, ErrPending) {
		t.Fatal("replayed completed remote operation", err)
	}
}
func TestExactCASAndIntentIdentity(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	r := repository(t, st, nil)
	g, op := fixture(t)
	res := begin(t, r, g, op, "")
	if _, err := r.Begin(ctx, g, op, ""); !errors.Is(err, store.ErrConflict) {
		t.Fatal("empty token overwrote ledger", err)
	}
	changed := op.DeepCopy()
	changed.Spec.After.Targets[0] = "192.0.2.9"
	changed.Spec.AfterHash = kinds.EndpointDigest(changed.Spec.After)
	if _, err := r.Begin(ctx, g, changed, res.Ledger.ResourceVersion); !errors.Is(err, ErrInvalid) {
		t.Fatal("same operation ID changed intent", err)
	}
	changed = op.DeepCopy()
	changed.Spec.OperationID = kinds.Digest([]byte("other"))
	changed.Name = kinds.OperationName(changed.Spec.OperationID)
	if _, err := r.Begin(ctx, g, changed, res.Ledger.ResourceVersion); !errors.Is(err, ErrPending) {
		t.Fatal("replaced pending operation", err)
	}
	saved, err := r.RecordReceipt(ctx, g, res.Operation, complete(op))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RecordReceipt(ctx, g, res.Operation, complete(op)); !errors.Is(err, store.ErrConflict) {
		t.Fatal("substituted fresh operation RV", err)
	}
	if _, err := r.Confirm(ctx, g, res); !errors.Is(err, store.ErrConflict) {
		t.Fatal("accepted stale receipt token", err)
	}
	res.Operation = saved
	l, err := r.Confirm(ctx, g, res)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Confirm(ctx, g, res); !errors.Is(err, store.ErrConflict) {
		t.Fatal("substituted fresh ledger RV", err)
	}
	// Updating input/returned copies cannot alter persisted identity/content.
	l.Spec.ProviderIDs[0] = "mutated"
	res.Operation.Spec.After.Targets[0] = "mutated"
	got, err := r.ReadLedger(ctx, op.Spec.Record)
	if err != nil || got.Spec.ProviderIDs[0] == "mutated" {
		t.Fatal("store alias")
	}
}
func TestMonotoneReceipts(t *testing.T) {
	ctx := context.Background()
	r := repository(t, memory.New(), nil)
	g, op := fixture(t)
	res := begin(t, r, g, op, "")
	partial := kinds.Receipt{ObservedAt: testTime.Add(time.Second), Targets: []kinds.TargetResult{{Target: "192.0.2.1", ProviderID: "id", Applied: true}}}
	saved, err := r.RecordReceipt(ctx, g, res.Operation, partial)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		receipt kinds.Receipt
	}{{"lost progress", kinds.Receipt{ObservedAt: testTime.Add(2 * time.Second)}}, {"changed ID", kinds.Receipt{ObservedAt: testTime.Add(2 * time.Second), Targets: []kinds.TargetResult{{Target: "192.0.2.1", ProviderID: "other", Applied: true}}}}, {"older", kinds.Receipt{ObservedAt: testTime, Targets: partial.Targets}}} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.RecordReceipt(ctx, g, saved, tc.receipt); !errors.Is(err, ErrReceipt) {
				t.Fatal(err)
			}
		})
	}
	wrong := complete(op)
	wrong.ContentHash = kinds.Digest([]byte("wrong"))
	if _, err := r.RecordReceipt(ctx, g, saved, wrong); !errors.Is(err, ErrInvalid) {
		t.Fatal("mismatched complete result", err)
	}
	wrong = complete(op)
	wrong.ProviderIDs = nil
	if _, err := r.RecordReceipt(ctx, g, saved, wrong); !errors.Is(err, ErrInvalid) {
		t.Fatal("complete missing provider IDs", err)
	}
}
func TestAbandonTerminalizesBeforeRelease(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	r := repository(t, st, nil)
	g, op := fixture(t)
	res := begin(t, r, g, op, "")
	fail := true
	r = repository(t, st, func(ctx context.Context, _ Guard, o store.Object, opts store.ApplyOptions) (store.Object, error) {
		if fail && store.KindOf(o) == kinds.KindIntegrationLedger {
			return nil, store.ErrClosed
		}
		return st.Apply(ctx, o, opts)
	})
	if _, err := r.Abandon(ctx, g, res, Observation{BeforeHash: "", ObservedAt: testTime.Add(time.Second)}); !errors.Is(err, store.ErrClosed) {
		t.Fatal(err)
	}
	saved, err := r.ReadOperation(ctx, op.Spec.OperationID)
	if err != nil || saved.Spec.AbandonedAt.IsZero() {
		t.Fatal("not terminal before ledger release", err)
	}
	if _, err := r.RecordReceipt(ctx, g, res.Operation, complete(op)); !errors.Is(err, store.ErrConflict) {
		t.Fatal("late receipt escaped terminal CAS", err)
	}
	if _, err := r.RecordReceipt(ctx, g, saved, complete(op)); !errors.Is(err, ErrPending) {
		t.Fatal("terminal operation resurrected", err)
	}
	fail = false
	res.Operation = saved
	released, err := r.Abandon(ctx, g, res, Observation{ObservedAt: testTime.Add(2 * time.Second)})
	if err != nil || released.Spec.Pending != nil || released.Spec.ContentHash != "" {
		t.Fatal("restart release failed", released, err)
	}
	if _, err := r.Begin(ctx, g, op, released.ResourceVersion); !errors.Is(err, ErrPending) {
		t.Fatal("abandoned ID reused", err)
	}
}
func TestAbandonRejectsAmbiguousOrStaleProof(t *testing.T) {
	ctx := context.Background()
	r := repository(t, memory.New(), nil)
	g, op := fixture(t)
	res := begin(t, r, g, op, "")
	for _, proof := range []Observation{{}, {BeforeHash: kinds.Digest([]byte("changed")), ObservedAt: testTime.Add(time.Second)}, {ObservedAt: testTime.Add(time.Hour)}} {
		if _, err := r.Abandon(ctx, g, res, proof); !errors.Is(err, ErrPending) {
			t.Fatal(err)
		}
	}
	saved, err := r.RecordReceipt(ctx, g, res.Operation, kinds.Receipt{ObservedAt: testTime.Add(time.Second), Targets: []kinds.TargetResult{{Target: "192.0.2.1", ProviderID: "id", Applied: true}}})
	if err != nil {
		t.Fatal(err)
	}
	res.Operation = saved
	if _, err := r.Abandon(ctx, g, res, Observation{ObservedAt: testTime.Add(2 * time.Second)}); !errors.Is(err, ErrPending) {
		t.Fatal("discarded applied partial work", err)
	}
}
func TestGuardFencedAtEachWriteAndHistoricalRecovery(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	g, op := fixture(t)
	active := g
	count := 0
	r := repository(t, st, func(ctx context.Context, guard Guard, o store.Object, opts store.ApplyOptions) (store.Object, error) {
		if guard != active {
			return nil, replication.ErrFenced
		}
		count++
		return st.Apply(ctx, o, opts)
	})
	res := begin(t, r, g, op, "")
	if count != 2 {
		t.Fatal("missing durable intent/reservation writes")
	}
	active.LeaseID = "lease-2"
	if _, err := r.RecordReceipt(ctx, g, res.Operation, complete(op)); !errors.Is(err, replication.ErrFenced) {
		t.Fatal("canceled lease wrote receipt", err)
	}
	active.ConfigDigest = kinds.Digest([]byte("new config"))
	active.CredentialDigest = kinds.Digest([]byte("rotated credential"))
	active.IntegrationResourceVersion = "rv-2"
	saved, err := r.RecordReceipt(ctx, active, res.Operation, complete(op))
	if err != nil {
		t.Fatal("current same UID cannot record historical observation", err)
	}
	res.Operation = saved
	if _, err := r.Confirm(ctx, g, res); !errors.Is(err, replication.ErrFenced) {
		t.Fatal("stale lease confirmed", err)
	}
	if _, err := r.Confirm(ctx, active, res); err != nil {
		t.Fatal(err)
	}
	active.Owner.UID = "new UID"
	if _, err := r.RecordReceipt(ctx, active, saved, complete(op)); !errors.Is(err, ErrInvalid) {
		t.Fatal("recreated instance wrote old ledger", err)
	}
}
func TestConcurrentReservations(t *testing.T) {
	st := memory.New()
	r := repository(t, st, nil)
	g, op := fixture(t)
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			if _, err := r.Begin(context.Background(), g, op, ""); err == nil {
				successes.Add(1)
			} else if !errors.Is(err, store.ErrConflict) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("winners=%d", successes.Load())
	}
}
func TestSequenceOverflowAndCancellation(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	r := repository(t, st, nil)
	g, op := fixture(t)
	res := begin(t, r, g, op, "")
	l := res.Ledger.DeepCopy()
	l.Spec.Sequence = math.MaxUint64
	out, err := st.Apply(ctx, l, store.ApplyOptions{Manager: manager, ExpectedResourceVersion: l.ResourceVersion})
	if err != nil {
		t.Fatal(err)
	}
	res.Ledger = out.(*kinds.IntegrationLedger)
	res.Operation, err = r.RecordReceipt(ctx, g, res.Operation, complete(op))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Confirm(ctx, g, res); !errors.Is(err, ErrOverflow) {
		t.Fatal(err)
	}
	st2 := memory.New()
	r2 := repository(t, st2, nil)
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r2.Begin(cancelCtx, g, op, ""); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestBaselineAndRemoteBeforeAreIndependent(t *testing.T) {
	ctx := context.Background()
	g, op := fixture(t)
	op.Spec.Action = "adopt"
	op.Spec.Before = op.Spec.After.DeepCopy()
	op.Spec.Before.Targets = []string{"192.0.2.99"}
	op.Spec.BeforeHash = kinds.EndpointDigest(op.Spec.Before)
	r := repository(t, memory.New(), nil)
	res := begin(t, r, g, op, "")
	if res.Ledger.Spec.Pending.BeforeHash != "" || op.Spec.BeforeHash == "" {
		t.Fatal("conflated ledger baseline and remote before")
	}
	var err error
	res.Operation, err = r.RecordReceipt(ctx, g, res.Operation, complete(op))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Confirm(ctx, g, res); err != nil {
		t.Fatal(err)
	}
}

func TestConfirmPersistsPolicyForRemovedSources(t *testing.T) {
	ctx := context.Background()
	g, op := fixture(t)
	op.Spec.Policy = "upsert-only"
	r := repository(t, memory.New(), nil)
	res := begin(t, r, g, op, "")
	var err error
	res.Operation, err = r.RecordReceipt(ctx, g, res.Operation, complete(op))
	if err != nil {
		t.Fatal(err)
	}
	l, err := r.Confirm(ctx, g, res)
	if err != nil || l.Spec.Policy != "upsert-only" {
		t.Fatal("lost deletion restriction", l, err)
	}
}
