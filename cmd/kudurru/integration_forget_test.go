package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestIntegrationRuntimeActualLedgerForgetIsFencedAndAtomic drives the root
// adapter's ledger Delete callback (D367) from inside a real elected lease
// of the mounted SQLite runtime: the original observation context of a
// provider supplies the exact guard, lease and writer, and the repository is
// built from the same root callbacks the host's own repository uses.
func TestIntegrationRuntimeActualLedgerForgetIsFencedAndAtomic(t *testing.T) {
	var s *servingRuntime
	var once sync.Once
	done := make(chan struct{})
	var failures []string
	// The provider goroutine only records; the test goroutine reports.
	fail := func(what string) { failures = append(failures, what) }
	destination := kinds.Destination{Provider: "test", Account: "account", Zone: "example.com"}
	var kept *kinds.IntegrationLedger
	exercise := func(ctx context.Context) {
		defer close(done)
		observation, ok := ctx.Value(integrationObservationKey{}).(*integrationObservation)
		if !ok || observation == nil {
			fail("observation carries no original guard")
			return
		}
		g := observation.guard.Ledger
		repo, err := ledger.New(ledger.Options{Reader: s.opts.Store, Now: s.opts.Now, Write: s.integrations.write, Delete: s.integrations.forget})
		if err != nil {
			fail("repository")
			return
		}
		input := plan.Input{Owner: g.Owner, Destination: destination, ConfigDigest: g.ConfigDigest, CredentialDigest: g.CredentialDigest, SourceDigest: kinds.Digest([]byte("source")), DesiredComplete: true, CurrentComplete: true}
		run := func(in plan.Input, expectedRV string, ids []string) (*kinds.IntegrationLedger, *kinds.IntegrationOperation, error) {
			p, err := plan.Build(in)
			if err != nil {
				return nil, nil, err
			}
			if len(p.Operations) != 1 {
				return nil, nil, errors.New("plan has not exactly one operation")
			}
			res, err := repo.Begin(ctx, g, p.Intent(0, s.opts.Now()), expectedRV)
			if err != nil {
				return nil, nil, errors.Join(errors.New("begin"), err)
			}
			op, err := repo.RecordReceipt(ctx, g, res.Operation, kinds.Receipt{Complete: true, ContentHash: res.Operation.Spec.AfterHash, ProviderIDs: ids, ObservedAt: s.opts.Now().UTC()})
			if err != nil {
				return nil, nil, errors.Join(errors.New("receipt"), err)
			}
			res.Operation = op
			l, err := repo.Confirm(ctx, g, res)
			if err != nil {
				return nil, nil, errors.Join(errors.New("confirm"), err)
			}
			return l, op, nil
		}
		// Publish a record and delete it again with the cleanup plan, through
		// the root's fenced write path.
		settle := func(name string) (*kinds.IntegrationLedger, []*kinds.IntegrationOperation, error) {
			e := public.Endpoint{DNSName: name, RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.44"}}
			create := input
			create.Desired = []public.Endpoint{e}
			l, created, err := run(create, "", []string{"id-" + name})
			if err != nil {
				return nil, nil, err
			}
			cleanup := input
			cleanup.Cleanup = true
			cleanup.Current = []plan.RemoteRecord{{Endpoint: e, IDs: []string{"id-" + name}, Owner: plan.RemoteOwner{ClusterID: g.Owner.ClusterID, IntegrationUID: g.Owner.UID}}}
			cleanup.Ledger = []*kinds.IntegrationLedger{l}
			l, removed, err := run(cleanup, l.ResourceVersion, nil)
			return l, []*kinds.IntegrationOperation{created, removed}, err
		}
		first, firstOps, err := settle("first.example.com.")
		if err != nil {
			fail("first record did not settle through the fenced writer: " + err.Error())
			return
		}
		second, secondOps, err := settle("second.example.com.")
		if err != nil {
			fail("second record did not settle through the fenced writer: " + err.Error())
			return
		}
		kept = second
		if _, err := repo.ReserveBudget(ctx, g, ledger.BudgetCharge{Provider: "test", Account: "account", ID: kinds.Digest([]byte("charge")), Cost: 1, Capacity: 10, WindowSeconds: 300}); err != nil {
			fail("budget reservation")
			return
		}
		var budget kinds.IntegrationBudget
		if err := s.opts.Store.Get(ctx, store.Key{Kind: kinds.KindIntegrationBudget, Name: kinds.BudgetName(g.Owner.ClusterID, "test", "account")}, &budget); err != nil {
			fail("budget row")
			return
		}
		exact := func(kind store.Kind, obj metav1.Object) store.DeleteRequest {
			return store.DeleteRequest{Key: store.Key{Kind: kind, Name: obj.GetName()}, Options: store.DeleteOptions{Manager: "integration-ledger", ExpectedUID: string(obj.GetUID()), ExpectedResourceVersion: obj.GetResourceVersion()}}
		}
		present := func(l *kinds.IntegrationLedger) bool {
			return s.opts.Store.Get(ctx, store.Key{Kind: kinds.KindIntegrationLedger, Name: l.Name}, &kinds.IntegrationLedger{}) == nil
		}
		// The callback can name only private publication state, by exact
		// identity, never with Force.
		noUID := exact(kinds.KindIntegrationLedger, first)
		noUID.Options.ExpectedUID = ""
		forced := exact(kinds.KindIntegrationLedger, first)
		forced.Options.Force = true
		for name, requests := range map[string][]store.DeleteRequest{
			"budget row":        {exact(kinds.KindIntegrationLedger, first), exact(kinds.KindIntegrationBudget, &budget)},
			"desired object":    {{Key: store.Key{Kind: "Integration", Namespace: g.Owner.Namespace, Name: g.Owner.Name}, Options: store.DeleteOptions{ExpectedUID: g.Owner.UID, ExpectedResourceVersion: g.IntegrationResourceVersion}}},
			"no expected UID":   {noUID},
			"force":             {forced},
			"nothing requested": nil,
		} {
			if err := s.integrations.forget(ctx, g, requests); !errors.Is(err, ih.ErrInvalid) {
				fail("root deleter accepted: " + name)
			}
		}
		// One command: a later request that does not match rolls back the
		// ledger row deleted before it.
		stale := exact(kinds.KindIntegrationOperation, firstOps[1])
		stale.Options.ExpectedResourceVersion = "1"
		if err := s.integrations.forget(ctx, g, []store.DeleteRequest{exact(kinds.KindIntegrationLedger, first), stale}); !errors.Is(err, store.ErrConflict) || !present(first) {
			fail("stale operation identity did not roll back the ledger delete")
		}
		missing := exact(kinds.KindIntegrationOperation, firstOps[1])
		missing.Key.Name = kinds.OperationName(kinds.Digest([]byte("never stored")))
		if err := s.integrations.forget(ctx, g, []store.DeleteRequest{exact(kinds.KindIntegrationLedger, first), missing}); !errors.Is(err, store.ErrNotFound) || !present(first) {
			fail("missing operation did not roll back the ledger delete")
		}
		// A guard of another lease is not this elected job.
		other := g
		other.LeaseID = "another-lease"
		if err := repo.Forget(ctx, other, first, firstOps, ledger.Observation{}); !errors.Is(err, ih.ErrFenced) || !present(first) {
			fail("guard of another lease forgot a record")
		}
		if err := repo.Forget(ctx, g, first, firstOps, ledger.Observation{}); err != nil || present(first) {
			fail("exact forget through the root deleter failed")
			return
		}
		// The Integration changes: the original guard is fenced in the
		// command, and the second record stays.
		var current public.Integration
		if err := s.opts.Store.Get(ctx, store.Key{Kind: "Integration", Namespace: g.Owner.Namespace, Name: g.Owner.Name}, &current); err != nil {
			fail("integration read")
			return
		}
		current.Spec.Resync = metav1.Duration{Duration: 20 * time.Minute}
		if _, err := s.opts.Store.Apply(context.WithoutCancel(ctx), &current, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: current.ResourceVersion}); err != nil {
			fail("integration change")
			return
		}
		if err := repo.Forget(ctx, g, second, secondOps, ledger.Observation{}); !errors.Is(err, ih.ErrFenced) || !present(second) {
			fail("a fenced guard forgot a record")
		}
	}
	s, path := integrationFixture(t, nil, func(string) (integration.Integration, error) {
		return &integrationProbe{observe: func(ctx context.Context, _ ih.OwnerHost) (ih.SourceSnapshot, error) {
			once.Do(func() { exercise(ctx) })
			return ih.SourceSnapshot{Complete: true}, nil
		}}, nil
	})
	integrationApply(t, s, integrationDefinition("forget", path))
	integrationRun(t, s)
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("the elected observation never ran")
	}
	for _, failure := range failures {
		t.Error(failure)
	}
	if t.Failed() {
		return
	}
	ledgers, err := s.opts.Store.List(t.Context(), kinds.KindIntegrationLedger, store.ListOptions{})
	if err != nil || len(ledgers.Items) != 1 || ledgers.Items[0].GetName() != kept.Name {
		t.Fatal("exactly the fenced record's ledger row must remain", err != nil)
	}
	operations, err := s.opts.Store.List(t.Context(), kinds.KindIntegrationOperation, store.ListOptions{})
	if err != nil || len(operations.Items) != 2 {
		t.Fatal("exactly the fenced record's operations must remain", err != nil)
	}
	for _, row := range operations.Items {
		if row.(*kinds.IntegrationOperation).Spec.Record != kept.Spec.Record {
			t.Fatal("an operation of the forgotten record remains")
		}
	}
	budgets, err := s.opts.Store.List(t.Context(), kinds.KindIntegrationBudget, store.ListOptions{})
	if err != nil || len(budgets.Items) != 1 {
		t.Fatal("the shared request budget must survive forgetting", err != nil)
	}
	// Effect and audit commit together: one delete row per forgotten object.
	deleted := 0
	for _, kind := range []store.Kind{kinds.KindIntegrationLedger, kinds.KindIntegrationOperation} {
		entries, err := s.opts.Store.ReadAudit(t.Context(), sqlite.AuditQuery{Kind: kind})
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Action, ".delete") {
				deleted++
			}
		}
	}
	if deleted != 3 {
		t.Fatal("forgotten objects and their audit rows differ", deleted)
	}
}
