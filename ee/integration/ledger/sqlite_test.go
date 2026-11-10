package ledger

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestSQLiteReceiptRecoveryAfterReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	var private []store.Kind
	for _, kind := range runtimekinds.Private() {
		private = append(private, store.Kind(kind))
	}
	options := sqlite.Options{Scheme: scheme, InternalKinds: private}
	st, err := sqlite.Open(path, options)
	if err != nil {
		t.Fatal(err)
	}
	g, op := fixture(t)
	g.Term = st.Authority().Term
	g.Owner.ClusterID = g.Term.ClusterID
	op.Spec.Record.Owner = g.Owner
	// Owner changed to the durable database cluster; rename its ledger identity
	// is automatic in Begin. Operation ID remains immutable for this fixture.
	makeRepo := func() *Repository {
		r, err := New(Options{Reader: st, Write: func(ctx context.Context, guard Guard, obj store.Object, opts store.ApplyOptions) (store.Object, error) {
			var saved store.Object
			err := st.Command(ctx, guard.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				var err error
				saved, err = tx.Apply(ctx, obj, opts)
				return err
			})
			return saved, err
		}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := makeRepo()
	res := begin(t, r, g, op, "")
	res.Operation, err = r.RecordReceipt(ctx, g, res.Operation, complete(op))
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = sqlite.Open(path, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	r = makeRepo()
	l, err := r.ReadLedger(ctx, op.Spec.Record)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := r.ReadOperation(ctx, op.Spec.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if l.Spec.Pending == nil || l.Spec.ContentHash != "" || saved.Spec.Receipt == nil || !saved.Spec.Receipt.Complete {
		t.Fatal("restart lost intent/receipt boundary")
	}
	confirmed, err := r.Confirm(ctx, g, &Reservation{Ledger: l, Operation: saved})
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Spec.ContentHash != op.Spec.AfterHash || confirmed.Spec.Pending != nil {
		t.Fatal("restart confirmation failed")
	}
}

// TestSQLiteForgetIsFencedExactAndDurable runs the cleanup protocol on a real
// state.db: publish, cleanup-mode delete, then Forget through a Delete
// callback that commits in the same fenced command shape as Write.
func TestSQLiteForgetIsFencedExactAndDurable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	var private []store.Kind
	for _, kind := range runtimekinds.Private() {
		private = append(private, store.Kind(kind))
	}
	options := sqlite.Options{Scheme: scheme, InternalKinds: private}
	st, err := sqlite.Open(path, options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Now().UTC()
	makeRepo := func() *Repository {
		r, err := New(Options{Reader: st, Now: func() time.Time { return now }, Write: func(ctx context.Context, guard Guard, obj store.Object, opts store.ApplyOptions) (store.Object, error) {
			var saved store.Object
			err := st.Command(ctx, guard.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				var err error
				saved, err = tx.Apply(ctx, obj, opts)
				return err
			})
			return saved, err
		}, Delete: func(ctx context.Context, guard Guard, reqs []store.DeleteRequest) error {
			return st.Command(ctx, guard.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				for _, req := range reqs {
					if err := tx.Delete(ctx, req.Key, req.Options); err != nil {
						return err
					}
				}
				return nil
			})
		}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := makeRepo()
	g, _ := fixture(t)
	g.Term = st.Authority().Term
	g.Owner.ClusterID = g.Term.ClusterID
	destination := kinds.Destination{Provider: "test", Account: "account", Zone: "example.com"}
	published := public.Endpoint{DNSName: "a.example.com.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.1"}}
	input := plan.Input{Owner: g.Owner, Destination: destination, ConfigDigest: g.ConfigDigest, CredentialDigest: g.CredentialDigest, SourceDigest: kinds.Digest([]byte("source")), DesiredComplete: true, CurrentComplete: true}
	run := func(in plan.Input, expectedRV string, receipt kinds.Receipt) (*kinds.IntegrationLedger, *kinds.IntegrationOperation) {
		t.Helper()
		p, err := plan.Build(in)
		if err != nil || len(p.Operations) != 1 {
			t.Fatal("plan", err)
		}
		res := begin(t, r, g, p.Intent(0, now.Add(-30*time.Second)), expectedRV)
		receipt.Complete, receipt.ContentHash = true, res.Operation.Spec.AfterHash
		if res.Operation, err = r.RecordReceipt(ctx, g, res.Operation, receipt); err != nil {
			t.Fatal(err)
		}
		l, err := r.Confirm(ctx, g, res)
		if err != nil {
			t.Fatal(err)
		}
		return l, res.Operation
	}
	create := input
	create.Desired = []public.Endpoint{published}
	l, created := run(create, "", kinds.Receipt{ProviderIDs: []string{"provider-1"}, ObservedAt: now.Add(-20 * time.Second)})
	if _, err := r.ReserveBudget(ctx, g, BudgetCharge{Provider: "test", Account: "account", ID: kinds.Digest([]byte("charge")), Cost: 1, Capacity: 10, WindowSeconds: 300}); err != nil {
		t.Fatal(err)
	}
	// Published and not yet deleted: the baseline lists provider content.
	if err := r.Forget(ctx, g, l, []*kinds.IntegrationOperation{created}, Observation{}); !errors.Is(err, ErrBaseline) {
		t.Fatalf("forget of a published record: %v", err)
	}
	cleanup := input
	cleanup.Cleanup = true
	cleanup.Current = []plan.RemoteRecord{{Endpoint: published, IDs: []string{"provider-1"}, Owner: plan.RemoteOwner{ClusterID: g.Owner.ClusterID, IntegrationUID: g.Owner.UID}}}
	cleanup.Ledger = []*kinds.IntegrationLedger{l}
	l, removed := run(cleanup, l.ResourceVersion, kinds.Receipt{ObservedAt: now.Add(-10 * time.Second)})
	ops := []*kinds.IntegrationOperation{created, removed}

	count := func(kind store.Kind) int {
		t.Helper()
		rows, err := st.List(ctx, kind, store.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return len(rows.Items)
	}
	// A guard of another term is refused at the commit boundary, and nothing
	// of the record is removed.
	stale := g
	stale.Term.Epoch++
	if err := r.Forget(ctx, stale, l, ops, Observation{}); !errors.Is(err, replication.ErrCAS) {
		t.Fatalf("stale guard: %v", err)
	}
	// An operation that changed after the caller listed it is a conflict, and
	// the ledger row named in the same call stays too.
	touched := removed.DeepCopy()
	if err := st.Command(ctx, g.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		touched.Labels = map[string]string{"touched": "after-read"}
		_, err := tx.Apply(ctx, touched, store.ApplyOptions{Manager: manager, ExpectedResourceVersion: removed.ResourceVersion})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Forget(ctx, g, l, ops, Observation{}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("operation changed since it was listed: %v", err)
	}
	if count(kinds.KindIntegrationLedger) != 1 || count(kinds.KindIntegrationOperation) != 2 {
		t.Fatal("a refused forget removed private state")
	}
	removed, err = r.ReadOperation(ctx, removed.Spec.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Forget(ctx, g, l, []*kinds.IntegrationOperation{created, removed}, Observation{}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = sqlite.Open(path, options)
	if err != nil {
		t.Fatal(err)
	}
	r = makeRepo()
	if count(kinds.KindIntegrationLedger) != 0 || count(kinds.KindIntegrationOperation) != 0 {
		t.Fatal("forgotten state returned after reopen")
	}
	if count(kinds.KindIntegrationBudget) != 1 {
		t.Fatal("the shared request budget must survive cleanup")
	}
	if _, err := r.ReadLedger(ctx, l.Spec.Record); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("forgotten ledger row: %v", err)
	}
}
