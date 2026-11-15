package ledger

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
)

// forgetFixture publishes the fixture record through the ordinary protocol
// and returns a repository whose Delete callback records every call. The
// carrier is sequential unless atomic is set, like a store without
// multi-object transactions: requests are applied in order and stop at the
// first failure.
type forgetFixture struct {
	t       testing.TB
	st      *memory.Store
	r       *Repository
	g       Guard
	create  *kinds.IntegrationOperation
	calls   [][]store.DeleteRequest
	failAt  int // 1-based request index to fail at; 0 never
	failure error
	now     time.Time
}

func newForgetFixture(t testing.TB) *forgetFixture {
	t.Helper()
	f := &forgetFixture{t: t, st: memory.New(), now: testTime.Add(10 * time.Second)}
	var err error
	f.r, err = New(Options{Reader: f.st, Now: func() time.Time { return f.now }, Write: func(ctx context.Context, _ Guard, o store.Object, opts store.ApplyOptions) (store.Object, error) {
		return f.st.Apply(ctx, o, opts)
	}, Delete: func(ctx context.Context, _ Guard, reqs []store.DeleteRequest) error {
		f.calls = append(f.calls, slices.Clone(reqs))
		for i, req := range reqs {
			if f.failAt == i+1 {
				return f.failure
			}
			if err := f.st.Delete(ctx, req.Key, req.Options); err != nil {
				return err
			}
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.g, f.create = fixture(t)
	res := begin(t, f.r, f.g, f.create, "")
	res.Operation, err = f.r.RecordReceipt(context.Background(), f.g, res.Operation, complete(f.create))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Confirm(context.Background(), f.g, res); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *forgetFixture) ledger() *kinds.IntegrationLedger {
	f.t.Helper()
	l, err := f.r.ReadLedger(context.Background(), f.create.Spec.Record)
	if err != nil {
		f.t.Fatal(err)
	}
	return l
}

func (f *forgetFixture) operation(id string) *kinds.IntegrationOperation {
	f.t.Helper()
	op, err := f.r.ReadOperation(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return op
}

// cleanupIntent is the delete the cleanup planner emits for the published
// record: both provider rows carry the owner's marker and the confirmed
// ledger baseline lists both IDs.
func (f *forgetFixture) cleanupIntent() *kinds.IntegrationOperation {
	f.t.Helper()
	row := func(target, id string) plan.RemoteRecord {
		return plan.RemoteRecord{Endpoint: public.Endpoint{DNSName: "a.example.com.", RecordType: "A", RecordTTL: 300, Targets: []string{target}}, IDs: []string{id}, Owner: plan.RemoteOwner{ClusterID: f.g.Owner.ClusterID, IntegrationUID: f.g.Owner.UID}}
	}
	p, err := plan.Build(plan.Input{Owner: f.g.Owner, Destination: f.create.Spec.Record.Destination, ConfigDigest: f.g.ConfigDigest, CredentialDigest: f.g.CredentialDigest, SourceDigest: kinds.Digest([]byte("source")), DesiredComplete: true, CurrentComplete: true, Cleanup: true,
		Current: []plan.RemoteRecord{row("192.0.2.1", "provider-1"), row("192.0.2.2", "provider-2")}, Ledger: []*kinds.IntegrationLedger{f.ledger()}})
	if err != nil {
		f.t.Fatal(err)
	}
	if len(p.Operations) != 1 || p.Operations[0].Action != "delete" || len(p.Retained) != 0 {
		f.t.Fatalf("cleanup plan: %+v", p)
	}
	return p.Intent(0, testTime.Add(2*time.Second))
}

// deleted runs the cleanup delete to its confirmed absent baseline.
func (f *forgetFixture) deleted() *kinds.IntegrationOperation {
	f.t.Helper()
	ctx := context.Background()
	intent := f.cleanupIntent()
	res := begin(f.t, f.r, f.g, intent, f.ledger().ResourceVersion)
	var err error
	res.Operation, err = f.r.RecordReceipt(ctx, f.g, res.Operation, kinds.Receipt{Complete: true, ObservedAt: testTime.Add(3 * time.Second), Targets: []kinds.TargetResult{{Target: "192.0.2.1", ProviderID: "provider-1", Applied: true}, {Target: "192.0.2.2", ProviderID: "provider-2", Applied: true}}})
	if err != nil {
		f.t.Fatal(err)
	}
	l, err := f.r.Confirm(ctx, f.g, res)
	if err != nil {
		f.t.Fatal(err)
	}
	if l.Spec.ContentHash != "" || len(l.Spec.ProviderIDs) != 0 || l.Spec.Pending != nil {
		f.t.Fatalf("confirmed delete left a baseline: %+v", l.Spec)
	}
	return res.Operation
}

func (f *forgetFixture) absent(kind store.Kind, name string) bool {
	f.t.Helper()
	var into store.Object = &kinds.IntegrationLedger{}
	if kind == kinds.KindIntegrationOperation {
		into = &kinds.IntegrationOperation{}
	}
	err := f.st.Get(context.Background(), store.Key{Kind: kind, Name: name}, into)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		f.t.Fatal(err)
	}
	return err != nil
}

func TestForgetRemovesConfirmedAbsentRecordByExactIdentity(t *testing.T) {
	ctx := context.Background()
	f := newForgetFixture(t)
	if _, err := f.r.ReserveBudget(ctx, f.g, BudgetCharge{Provider: "test", Account: "account", ID: kinds.Digest([]byte("charge")), Cost: 1, Capacity: 10, WindowSeconds: 300}); err != nil {
		t.Fatal(err)
	}
	removal := f.deleted()
	l, created := f.ledger(), f.operation(f.create.Spec.OperationID)
	// Order and duplicates in the caller's list do not matter; identities do.
	if err := f.r.Forget(ctx, f.g, l, []*kinds.IntegrationOperation{removal, created, removal}, Observation{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate operation: %v", err)
	}
	if err := f.r.Forget(ctx, f.g, l, []*kinds.IntegrationOperation{removal, created}, Observation{}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || len(f.calls[0]) != 3 {
		t.Fatalf("delete calls = %+v", f.calls)
	}
	reqs := f.calls[0]
	if reqs[0].Key != (store.Key{Kind: kinds.KindIntegrationLedger, Name: l.Name}) || reqs[0].Options.ExpectedUID != string(l.UID) || reqs[0].Options.ExpectedResourceVersion != l.ResourceVersion {
		t.Fatalf("the ledger row must be deleted first, by exact identity: %+v", reqs[0])
	}
	names := []string{created.Name, removal.Name}
	slices.Sort(names)
	var followed []string
	for i, req := range reqs {
		if req.Options.Force || req.Options.Manager != manager || req.Options.ExpectedUID == "" || req.Options.ExpectedResourceVersion == "" {
			t.Fatalf("request %d is not an exact owned delete: %+v", i, req)
		}
		if i > 0 && req.Key.Kind == kinds.KindIntegrationOperation {
			followed = append(followed, req.Key.Name)
		}
	}
	if !slices.Equal(followed, names) {
		t.Fatalf("operations must follow the ledger row in name order: %v", followed)
	}
	if !f.absent(kinds.KindIntegrationLedger, l.Name) || !f.absent(kinds.KindIntegrationOperation, created.Name) || !f.absent(kinds.KindIntegrationOperation, removal.Name) {
		t.Fatal("forgotten state remains")
	}
	// The shared request budget outlives every instance (D209).
	budgets, err := f.st.List(ctx, kinds.KindIntegrationBudget, store.ListOptions{})
	if err != nil || len(budgets.Items) != 1 || len(budgets.Items[0].(*kinds.IntegrationBudget).Spec.Reservations) != 1 {
		t.Fatalf("budget row touched: %v", err)
	}
	// Forgetting is not replayable against whatever exists later.
	if err := f.r.Forget(ctx, f.g, l, nil, Observation{}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("replayed forget: %v", err)
	}
}

func TestForgetRefusesPendingBaselineStaleAndForeignState(t *testing.T) {
	ctx := context.Background()
	fresh := func(f *forgetFixture) Observation { return Observation{ObservedAt: f.now.Add(-time.Second)} }

	t.Run("non-empty baseline", func(t *testing.T) {
		f := newForgetFixture(t)
		l, created := f.ledger(), f.operation(f.create.Spec.OperationID)
		ops := []*kinds.IntegrationOperation{created}
		for name, proof := range map[string]Observation{
			"no observation":           {},
			"observed content":         {BeforeHash: l.Spec.ContentHash, ObservedAt: f.now.Add(-time.Second)},
			"older than a minute":      {ObservedAt: f.now.Add(-61 * time.Second)},
			"from the future":          {ObservedAt: f.now.Add(time.Second)},
			"before the last receipt":  {ObservedAt: l.Spec.LastApplied.Add(-time.Nanosecond)},
			"other content than ours ": {BeforeHash: kinds.Digest([]byte("other")), ObservedAt: f.now.Add(-time.Second)},
		} {
			if err := f.r.Forget(ctx, f.g, l, ops, proof); !errors.Is(err, ErrBaseline) {
				t.Fatalf("%s: got %v, want ErrBaseline", name, err)
			}
		}
		if len(f.calls) != 0 {
			t.Fatal("a refused forget reached the carrier")
		}
		// The provider no longer holds the record (someone removed it there):
		// only the host's fresh complete observation of absence releases it.
		if err := f.r.Forget(ctx, f.g, l, ops, fresh(f)); err != nil {
			t.Fatal(err)
		}
		if !f.absent(kinds.KindIntegrationLedger, l.Name) || !f.absent(kinds.KindIntegrationOperation, created.Name) {
			t.Fatal("observed-absent record not forgotten")
		}
	})

	t.Run("pending row", func(t *testing.T) {
		f := newForgetFixture(t)
		intent := f.cleanupIntent()
		res := begin(t, f.r, f.g, intent, f.ledger().ResourceVersion)
		ops := []*kinds.IntegrationOperation{f.operation(f.create.Spec.OperationID), res.Operation}
		if err := f.r.Forget(ctx, f.g, res.Ledger, ops, fresh(f)); !errors.Is(err, ErrPending) {
			t.Fatalf("reserved, unanswered delete: got %v, want ErrPending", err)
		}
		// A partial receipt is an uncertain provider outcome, with or without
		// an absence claim.
		partial, err := f.r.RecordReceipt(ctx, f.g, res.Operation, kinds.Receipt{ObservedAt: testTime.Add(3 * time.Second), Targets: []kinds.TargetResult{{Target: "192.0.2.1", ProviderID: "provider-1", Applied: true}}})
		if err != nil {
			t.Fatal(err)
		}
		ops[1] = partial
		if err := f.r.Forget(ctx, f.g, f.ledger(), ops, fresh(f)); !errors.Is(err, ErrPending) {
			t.Fatalf("partially applied delete: got %v, want ErrPending", err)
		}
		if len(f.calls) != 0 {
			t.Fatal("a refused forget reached the carrier")
		}
	})

	t.Run("partial receipt without a reservation", func(t *testing.T) {
		// Not reachable through the protocol; a store in this state must still
		// not lose the only record of an applied provider effect.
		f := newForgetFixture(t)
		removal := f.deleted()
		stray := f.strayIntent(testTime.Add(4 * time.Second))
		stray.Spec.Receipt = &kinds.Receipt{ObservedAt: testTime.Add(5 * time.Second), Targets: []kinds.TargetResult{{Target: "192.0.2.1", ProviderID: "provider-1", Applied: true}}}
		saved, err := f.st.Apply(ctx, stray, store.ApplyOptions{Manager: manager, CreateOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		ops := []*kinds.IntegrationOperation{f.operation(f.create.Spec.OperationID), removal, saved.(*kinds.IntegrationOperation)}
		if err := f.r.Forget(ctx, f.g, f.ledger(), ops, Observation{}); !errors.Is(err, ErrPending) {
			t.Fatalf("got %v, want ErrPending", err)
		}
	})

	t.Run("stale and foreign identities", func(t *testing.T) {
		f := newForgetFixture(t)
		before := f.ledger()
		removal := f.deleted()
		l, created := f.ledger(), f.operation(f.create.Spec.OperationID)
		ops := []*kinds.IntegrationOperation{created, removal}
		staleOp := created.DeepCopy()
		staleOp.ResourceVersion = "0"
		otherUID := l.DeepCopy()
		otherUID.UID = "another-incarnation"
		edited := l.DeepCopy()
		edited.Spec.Sequence++
		otherOwner := f.g
		otherOwner.Owner.UID = "uid-2"
		noDelete := repository(t, f.st, nil)
		for name, tc := range map[string]struct {
			r    *Repository
			g    Guard
			l    *kinds.IntegrationLedger
			ops  []*kinds.IntegrationOperation
			want error
		}{
			"stale ledger resourceVersion":    {f.r, f.g, before, ops, store.ErrConflict},
			"ledger of another incarnation":   {f.r, f.g, otherUID, ops, store.ErrConflict},
			"ledger content not as stored":    {f.r, f.g, edited, ops, ErrInvalid},
			"stale operation resourceVersion": {f.r, f.g, l, []*kinds.IntegrationOperation{staleOp, removal}, store.ErrConflict},
			"guard of another incarnation":    {f.r, otherOwner, l, ops, ErrInvalid},
			"invalid guard":                   {f.r, Guard{}, l, ops, ErrInvalid},
			"row withheld":                    {f.r, f.g, nil, ops, store.ErrConflict},
			"nothing named":                   {f.r, f.g, nil, nil, ErrInvalid},
			"nil operation":                   {f.r, f.g, l, []*kinds.IntegrationOperation{nil}, ErrInvalid},
			"repository without a deleter":    {noDelete, f.g, l, ops, ErrInvalid},
		} {
			if err := tc.r.Forget(ctx, tc.g, tc.l, tc.ops, Observation{}); !errors.Is(err, tc.want) {
				t.Fatalf("%s: got %v, want %v", name, err, tc.want)
			}
		}
		// A settled operation of another record never rides along.
		other := f.unreserved("b.example.com.")
		if err := f.r.Forget(ctx, f.g, l, append(slices.Clone(ops), other), Observation{}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("operation of another record: %v", err)
		}
		if len(f.calls) != 0 {
			t.Fatal("a refused forget reached the carrier")
		}
		if err := f.r.Forget(ctx, f.g, l, ops, Observation{}); err != nil {
			t.Fatal(err)
		}
	})
}

// strayIntent is a second delete intent for the fixture record with its own
// operation ID (another plan over another remote row).
func (f *forgetFixture) strayIntent(at time.Time) *kinds.IntegrationOperation {
	f.t.Helper()
	row := plan.RemoteRecord{Endpoint: public.Endpoint{DNSName: "a.example.com.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.9"}}, IDs: []string{"provider-9"}, Owner: plan.RemoteOwner{ClusterID: f.g.Owner.ClusterID, IntegrationUID: f.g.Owner.UID}}
	p, err := plan.Build(plan.Input{Owner: f.g.Owner, Destination: f.create.Spec.Record.Destination, ConfigDigest: f.g.ConfigDigest, CredentialDigest: f.g.CredentialDigest, SourceDigest: kinds.Digest([]byte("source")), DesiredComplete: true, CurrentComplete: true, Current: []plan.RemoteRecord{row}})
	if err != nil || len(p.Operations) != 1 {
		f.t.Fatal("stray intent plan", err)
	}
	return p.Intent(0, at)
}

// unreserved stores a create intent for another record of the same owner
// without reserving a ledger row, as a failed reservation leaves it.
func (f *forgetFixture) unreserved(name string) *kinds.IntegrationOperation {
	f.t.Helper()
	p, err := plan.Build(plan.Input{Owner: f.g.Owner, Destination: f.create.Spec.Record.Destination, ConfigDigest: f.g.ConfigDigest, CredentialDigest: f.g.CredentialDigest, SourceDigest: kinds.Digest([]byte("source")), DesiredComplete: true, CurrentComplete: true, Desired: []public.Endpoint{{DNSName: name, RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.50"}}}})
	if err != nil || len(p.Operations) != 1 {
		f.t.Fatal("unreserved intent plan", err)
	}
	saved, err := f.st.Apply(context.Background(), p.Intent(0, testTime), store.ApplyOptions{Manager: manager, CreateOnly: true})
	if err != nil {
		f.t.Fatal(err)
	}
	return saved.(*kinds.IntegrationOperation)
}

func TestForgetOrderSurvivesACarrierWithoutTransactions(t *testing.T) {
	ctx := context.Background()
	f := newForgetFixture(t)
	removal := f.deleted()
	l, created := f.ledger(), f.operation(f.create.Spec.OperationID)
	ops := []*kinds.IntegrationOperation{created, removal}
	// The carrier stops after the ledger row: the row is gone, both
	// operations remain, and nothing was reported as forgotten.
	f.failAt, f.failure = 2, store.ErrClosed
	if err := f.r.Forget(ctx, f.g, l, ops, Observation{}); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("got %v", err)
	}
	if !f.absent(kinds.KindIntegrationLedger, l.Name) || f.absent(kinds.KindIntegrationOperation, created.Name) || f.absent(kinds.KindIntegrationOperation, removal.Name) {
		t.Fatal("sequential carrier did not stop after the ledger row")
	}
	f.failAt = 0
	// The same call cannot be replayed: its row no longer exists.
	if err := f.r.Forget(ctx, f.g, l, ops, Observation{}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("replay with the removed row: %v", err)
	}
	// With the row confirmed absent, the leftovers are forgotten on their own.
	if err := f.r.Forget(ctx, f.g, nil, ops, Observation{}); err != nil {
		t.Fatal(err)
	}
	if !f.absent(kinds.KindIntegrationOperation, created.Name) || !f.absent(kinds.KindIntegrationOperation, removal.Name) {
		t.Fatal("orphaned operations remain")
	}

	// An intent whose reservation failed has no ledger row and no receipt; it
	// conferred no provider permission and is forgotten the same way.
	orphan := f.unreserved("never-reserved.example.com.")
	if err := f.r.Forget(ctx, f.g, nil, []*kinds.IntegrationOperation{orphan}, Observation{}); err != nil {
		t.Fatal(err)
	}
	if !f.absent(kinds.KindIntegrationOperation, orphan.Name) {
		t.Fatal("orphaned intent remains")
	}
}
