package ledger

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/store"
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
