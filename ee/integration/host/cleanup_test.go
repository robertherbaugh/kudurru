package host

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
	"github.com/robertherbaugh/kudurru/internal/credentials"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A terminating Integration with the reserved finalizer and policy Delete is
// ended by one elected job after the ordinary job is joined: it deletes only
// what the ledger and the marker both prove, forgets the ledger, purges and
// finalizes exactly once (D367).
func TestTerminatingDeleteElectsDeletesForgetsAndFinalizesOnce(t *testing.T) {
	f := newCleanupFixture(t, cleanupObject("sink", "Delete", true), nil)
	foreign := plan.RemoteOwner{ClusterID: "cluster", IntegrationUID: "uid-previous"}
	f.w.seed(f.d, "kept.example.com.", "A", "198.51.100.1", "foreign-1", foreign)
	f.w.seed(f.d, "manual.example.com.", "A", "198.51.100.2", "manual-1", plan.RemoteOwner{})
	f.w.seed(f.d, "legacy.example.com.", "TXT", "heritage=external-dns", "heritage-1", plan.RemoteOwner{ExternalDNSOwner: "legacy"})
	f.awaitPublished()
	if len(f.owned()) != 2 {
		t.Fatalf("ordinary job published %d rows", len(f.owned()))
	}
	untouched := func() []plan.RemoteRecord {
		var out []plan.RemoteRecord
		for _, row := range f.w.snapshot(f.d) {
			if row.Owner.IntegrationUID != "uid-sink" {
				out = append(out, row)
			}
		}
		return out
	}
	before := untouched()

	f.requestDelete()
	summary := f.awaitFinalized()

	_, modes := f.finalizations()
	if modes[0] != GuardCleanup || summary.Released {
		t.Fatalf("finalized under mode %q released=%v", modes[0], summary.Released)
	}
	if summary.RecordSetsDeleted != 1 || summary.RecordsForgotten != 1 || summary.OperationsForgotten != 2 || summary.Purged.Objects != 2 {
		t.Fatalf("summary: %+v", summary)
	}
	if f.purges.Load() != 1 || f.refused.Load() != 0 {
		t.Fatalf("purges=%d refused finalizations=%d", f.purges.Load(), f.refused.Load())
	}
	if rows := f.owned(); len(rows) != 0 {
		t.Fatalf("owned rows remain: %+v", rows)
	}
	if after := untouched(); !reflect.DeepEqual(before, after) || len(after) != 3 {
		t.Fatalf("rows of other owners changed:\n%+v\n%+v", before, after)
	}
	_, creates, deletes, provisions := f.w.counts()
	if creates != 1 || deletes != 1 || provisions != 2 {
		t.Fatalf("creates=%d deletes=%d provisions=%d", creates, deletes, provisions)
	}
	if rows := append(f.rows(kinds.KindIntegrationLedger), f.rows(kinds.KindIntegrationOperation)...); len(rows) != 0 {
		t.Fatalf("%d ledger and operation rows remain", len(rows))
	}
	// The shared request budget outlives the instance.
	if rows := f.rows(kinds.KindIntegrationBudget); len(rows) != 1 {
		t.Fatalf("budget rows: %d", len(rows))
	}
	// The ordinary provider was stopped before the cleanup provider existed.
	lifecycle := func() []string {
		f.w.mu.Lock()
		defer f.w.mu.Unlock()
		return slices.Clone(f.w.events)
	}
	f.await("the cleanup provider to stop", func() bool { return len(lifecycle()) == 4 })
	if events := lifecycle(); !slices.Equal(events, []string{"provision#1", "stop#1", "provision#2", "stop#2"}) {
		t.Fatalf("provider lifecycle: %v", events)
	}
}

// A terminating object that carries the finalizer under any policy other than
// Delete is released: the finalizer goes with no provider, purge, license or
// credential.
func TestTerminatingRetainIsReleasedWithoutProviderOrLicense(t *testing.T) {
	object := cleanupObject("sink", "Retain", true)
	at := metav1.NewTime(time.Unix(1700000000, 0))
	object.DeletionTimestamp = &at
	object.Spec.Suspend = true
	object.Spec.Credentials = map[string]public.CredentialReference{"token": {Env: "MISSING"}}
	f := newCleanupFixture(t, object, func(f *cleanupFixture) { f.allowed.Store(false) })
	summary := f.awaitFinalized()
	_, modes := f.finalizations()
	if modes[0] != GuardRelease || !summary.Released || summary.RecordSetsDeleted != 0 {
		t.Fatalf("mode %q summary %+v", modes[0], summary)
	}
	requests, creates, deletes, provisions := f.w.counts()
	if requests+creates+deletes+provisions != 0 || f.factories.Load() != 0 || f.purges.Load() != 0 {
		t.Fatalf("release reached a provider or purged: requests=%d provisions=%d factories=%d purges=%d", requests, provisions, f.factories.Load(), f.purges.Load())
	}
	if rows := f.rows(kinds.KindIntegrationBudget); len(rows) != 0 {
		t.Fatal("release charged the request budget")
	}
}

// Cleanup pauses with a distinct reason and without any provider call while
// the license does not allow integrations, the instance is suspended, or a
// publisher's credential does not resolve. It finishes once the cause is gone.
func TestTerminatingPausedReasonsMakeNoProviderCall(t *testing.T) {
	terminating := func(edit func(*public.Integration)) *public.Integration {
		object := cleanupObject("sink", "Delete", true)
		at := metav1.NewTime(time.Unix(1700000000, 0))
		object.DeletionTimestamp = &at
		if edit != nil {
			edit(object)
		}
		return object
	}
	quiet := func(t *testing.T, f *cleanupFixture) {
		t.Helper()
		time.Sleep(30 * time.Millisecond)
		requests, _, _, provisions := f.w.counts()
		done, _ := f.finalizations()
		if requests != 0 || provisions != 0 || len(done) != 0 || f.purges.Load() != 0 {
			t.Fatalf("paused cleanup acted: requests=%d provisions=%d finalized=%d purges=%d", requests, provisions, len(done), f.purges.Load())
		}
	}
	t.Run("license", func(t *testing.T) {
		f := newCleanupFixture(t, terminating(nil), func(f *cleanupFixture) { f.allowed.Store(false) })
		f.awaitReason(PhaseTerminating, "LicenseUnavailable")
		quiet(t, f)
		if f.factories.Load() != 0 {
			t.Fatal("unlicensed cleanup built a provider")
		}
		f.allowed.Store(true)
		f.h.Wake()
		if summary := f.awaitFinalized(); summary.Released {
			t.Fatal("licensed cleanup reported a release")
		}
	})
	t.Run("suspended", func(t *testing.T) {
		f := newCleanupFixture(t, terminating(func(d *public.Integration) { d.Spec.Suspend = true }), nil)
		f.awaitReason(PhaseTerminating, "Suspended")
		quiet(t, f)
		f.change(func(d *public.Integration) { d.Spec.Suspend = false })
		f.awaitFinalized()
	})
	t.Run("publisher credential", func(t *testing.T) {
		token := ""
		var mu sync.Mutex
		resolver, err := credentials.New(credentials.Options{Environment: []string{"TEST_TOKEN"}, FingerprintKey: []byte(strings.Repeat("k", 32)), Getenv: func(name string) (string, bool) {
			mu.Lock()
			defer mu.Unlock()
			return token, name == "TEST_TOKEN" && token != ""
		}})
		if err != nil {
			t.Fatal(err)
		}
		object := terminating(func(d *public.Integration) {
			d.Spec.Credentials = map[string]public.CredentialReference{"token": {Env: "TEST_TOKEN"}}
		})
		f := newCleanupFixture(t, object, func(f *cleanupFixture) { f.input = func(in *Input) { in.Credentials = resolver } })
		f.awaitReason(PhaseTerminating, ReasonCredentials)
		quiet(t, f)
		// The durable status says so too, under the terminating guard.
		f.await("the published credentials reason", func() bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return len(f.published) == 1 && f.published[0].Phase == PhaseTerminating && f.published[0].Reason == ReasonCredentials
		})
		mu.Lock()
		token = "restored-private-token"
		mu.Unlock()
		f.h.Wake()
		f.awaitFinalized()
		if _, _, _, provisions := f.w.counts(); provisions != 1 {
			t.Fatalf("provisions after the credential returned: %d", provisions)
		}
	})
	t.Run("observer needs no credential", func(t *testing.T) {
		object := terminating(func(d *public.Integration) {
			d.Spec.Credentials = map[string]public.CredentialReference{"token": {Env: "MISSING"}}
		})
		f := newCleanupFixture(t, object, func(f *cleanupFixture) { f.w.observer = true })
		f.awaitFinalized()
		requests, _, _, provisions := f.w.counts()
		if requests != 0 || provisions != 0 || f.purges.Load() != 1 {
			t.Fatalf("local-only cleanup: requests=%d provisions=%d purges=%d", requests, provisions, f.purges.Load())
		}
	})
	t.Run("live object without the finalizer", func(t *testing.T) {
		f := newCleanupFixture(t, cleanupObject("sink", "Delete", false), nil)
		f.awaitReason("Paused", ReasonFinalizerMissing)
		quiet(t, f)
		if f.factories.Load() != 0 {
			t.Fatal("an unarmed Delete instance built a provider")
		}
		// Arming is one write of the object; the instance then runs.
		f.change(func(d *public.Integration) { d.Finalizers = []string{cleanupFinalizer} })
		f.awaitPublished()
	})
	t.Run("another finalizer only", func(t *testing.T) {
		object := terminating(func(d *public.Integration) { d.Finalizers = []string{"example.com/other"} })
		f := newCleanupFixture(t, object, nil)
		f.awaitReason("Paused", "Deleting")
		quiet(t, f)
	})
}

// A lease lost between the provider's delete and its receipt leaves a pending
// intent. The fenced lease cannot record anything; the next lease resolves
// the intent from a complete reread and makes no second mutation.
func TestTerminatingLeaseLossBetweenApplyAndReceiptResolvesByReread(t *testing.T) {
	var mu sync.Mutex
	var lose context.CancelFunc
	lost := false
	terms := 0
	var between []string
	var f *cleanupFixture
	elect := func(string) store.Elector {
		return fakeElector{run: func(ctx context.Context, start func(context.Context), _ func()) error {
			for ctx.Err() == nil {
				term, cancel := context.WithCancel(ctx)
				mu.Lock()
				lose, lost = cancel, false
				mu.Unlock()
				start(term)
				cancel()
				mu.Lock()
				again := lost
				if again {
					terms++
					// What the fenced lease left durable.
					for _, obj := range f.rows(kinds.KindIntegrationLedger) {
						if obj.(*kinds.IntegrationLedger).Spec.Pending == nil {
							between = append(between, "ledger not pending")
						}
					}
					for _, obj := range f.rows(kinds.KindIntegrationOperation) {
						if op := obj.(*kinds.IntegrationOperation); op.Spec.Action == "delete" && op.Spec.Receipt != nil {
							between = append(between, "fenced lease recorded a receipt")
						}
					}
				}
				mu.Unlock()
				if !again {
					<-ctx.Done()
				}
			}
			return nil
		}}
	}
	// The fixture pointer is published before the host starts any job.
	newCleanupFixture(t, cleanupObject("sink", "Delete", true), func(created *cleanupFixture) { f = created; created.elector = elect })
	f.awaitPublished()
	f.w.mu.Lock()
	f.w.onDelete = func(*kinds.IntegrationOperation) error {
		// The provider applied the delete; the lease ends before the answer.
		f.w.onDelete = nil
		mu.Lock()
		lost = true
		lose()
		mu.Unlock()
		return nil
	}
	f.w.mu.Unlock()
	f.requestDelete()
	summary := f.awaitFinalized()
	mu.Lock()
	defer mu.Unlock()
	if terms != 1 || len(between) != 0 {
		t.Fatalf("lost terms=%d, durable state between the leases: %v", terms, between)
	}
	f.w.mu.Lock()
	deletes, recoveries := f.w.deletes, f.w.recoveries
	f.w.mu.Unlock()
	if deletes != 1 || recoveries != 0 {
		t.Fatalf("provider mutations after the lost lease: deletes=%d recoveries=%d", deletes, recoveries)
	}
	// The finalizing lease deleted nothing itself; it confirmed the absence.
	if summary.RecordSetsDeleted != 0 || summary.RecordsForgotten != 1 || summary.OperationsForgotten != 2 {
		t.Fatalf("summary of the second lease: %+v", summary)
	}
	if rows := append(f.rows(kinds.KindIntegrationLedger), f.rows(kinds.KindIntegrationOperation)...); len(rows) != 0 {
		t.Fatalf("%d ledger and operation rows remain", len(rows))
	}
}

// Blocking conditions never finalize, report a specific reason and counts,
// and leave what cannot be proven in place. Cleanup finishes when the cause
// is gone.
func TestTerminatingBlockingConditionsNeverFinalize(t *testing.T) {
	blocked := func(t *testing.T, f *cleanupFixture, reason string) Result {
		t.Helper()
		f.awaitReason(PhaseTerminating, reason)
		time.Sleep(30 * time.Millisecond)
		if done, _ := f.finalizations(); len(done) != 0 || f.purges.Load() != 0 {
			t.Fatalf("blocked cleanup purged or finalized: %d %d", len(done), f.purges.Load())
		}
		_, _, result := f.status()
		// The durable status carries the same reason and counts.
		f.await("the published blocked status", func() bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			n := len(f.published)
			return n > 0 && f.published[n-1].Phase == PhaseTerminating && f.published[n-1].Reason == reason && f.published[n-1].Result == result
		})
		// That write was a new resource version of the object. The report
		// follows it, so a reader matching by revision still sees the reason
		// while the job waits for its next run.
		f.await("the report to follow the object's revision", func() bool {
			f.mu.Lock()
			revision := f.object.ResourceVersion
			f.mu.Unlock()
			for _, s := range f.h.Statuses() {
				if s.Owner.Name == "sink" {
					return s.Revision == revision && s.Reason == reason
				}
			}
			return false
		})
		return result
	}
	t.Run("conflict at a name with owned rows", func(t *testing.T) {
		f := newCleanupFixture(t, cleanupObject("sink", "Delete", true), nil)
		f.awaitPublished()
		f.w.seed(f.d, "a.example.com.", "TXT", "operator note", "manual-txt", plan.RemoteOwner{})
		f.requestDelete()
		result := blocked(t, f, ReasonRecordsConflict)
		if result.Conflicts != 1 || result.Remaining != 1 || len(f.owned()) != 2 {
			t.Fatalf("result %+v owned rows %d", result, len(f.owned()))
		}
		if _, _, deletes, _ := f.w.counts(); deletes != 0 {
			t.Fatal("cleanup deleted at a conflicted name")
		}
		f.w.drop(f.d, "manual-txt")
		f.wake()
		f.awaitFinalized()
		if len(f.owned()) != 0 {
			t.Fatal("owned rows remain after the conflict was resolved")
		}
	})
	t.Run("incomplete snapshot", func(t *testing.T) {
		f := newCleanupFixture(t, cleanupObject("sink", "Delete", true), nil)
		f.awaitPublished()
		f.w.mu.Lock()
		f.w.incomplete = true
		f.w.mu.Unlock()
		f.requestDelete()
		blocked(t, f, ReasonSnapshotIncomplete)
		if _, _, deletes, _ := f.w.counts(); deletes != 0 || len(f.owned()) != 2 {
			t.Fatal("cleanup acted on an incomplete snapshot")
		}
		f.w.mu.Lock()
		f.w.incomplete = false
		f.w.mu.Unlock()
		f.wake()
		f.awaitFinalized()
	})
	t.Run("marker without ledger evidence", func(t *testing.T) {
		f := newCleanupFixture(t, cleanupObject("sink", "Delete", true), nil)
		f.awaitPublished()
		// A row that only claims to be this instance's, as a copied record is.
		f.w.seed(f.d, "copied.example.com.", "A", "198.51.100.9", "copied-1", plan.RemoteOwner{ClusterID: "cluster", IntegrationUID: "uid-sink"})
		f.requestDelete()
		result := blocked(t, f, ReasonOwnershipUnproven)
		// What is proven is deleted and forgotten; the copied row stays.
		rows := f.owned()
		if result.Conflicts != 1 || result.Remaining != 0 || len(rows) != 1 || rows[0].IDs[0] != "copied-1" {
			t.Fatalf("result %+v owned rows %+v", result, rows)
		}
		f.w.drop(f.d, "copied-1")
		f.wake()
		f.awaitFinalized()
	})
	t.Run("ledger evidence without marker", func(t *testing.T) {
		f := newCleanupFixture(t, cleanupObject("sink", "Delete", true), nil)
		row := f.awaitPublished()
		// Someone took the records over at the provider: same IDs, no marker.
		f.w.mu.Lock()
		for i := range f.w.remote[f.d] {
			f.w.remote[f.d][i].Owner = plan.RemoteOwner{}
		}
		f.w.mu.Unlock()
		f.requestDelete()
		result := blocked(t, f, ReasonOwnershipUnproven)
		if _, _, deletes, _ := f.w.counts(); deletes != 0 || result.Remaining != 1 || len(f.w.snapshot(f.d)) != 2 {
			t.Fatalf("cleanup deleted rows it does not own: %+v", result)
		}
		if rows := f.rows(kinds.KindIntegrationLedger); len(rows) != 1 || rows[0].GetName() != row.Name {
			t.Fatal("the ledger row of a record that still exists was forgotten")
		}
	})
	t.Run("record removed at the provider by someone else", func(t *testing.T) {
		f := newCleanupFixture(t, cleanupObject("sink", "Delete", true), nil)
		f.awaitPublished()
		f.w.mu.Lock()
		f.w.remote[f.d] = nil
		f.w.mu.Unlock()
		f.requestDelete()
		// No delete receipt will ever confirm this record; the fresh complete
		// observation of its absence lets the row go.
		summary := f.awaitFinalized()
		if _, _, deletes, _ := f.w.counts(); deletes != 0 || summary.RecordSetsDeleted != 0 || summary.RecordsForgotten != 1 {
			t.Fatalf("summary %+v", summary)
		}
	})
	t.Run("destination removed from the spec", func(t *testing.T) {
		f := newCleanupFixture(t, cleanupObject("sink", "Delete", true), nil)
		f.awaitPublished()
		other := f.d
		other.Zone = "example.org"
		f.w.mu.Lock()
		f.w.pubs = []Publication{{Destination: other, Domains: []string{"example.org"}}}
		f.w.mu.Unlock()
		f.requestDelete()
		result := blocked(t, f, ReasonDestinationRemoved)
		if _, _, deletes, _ := f.w.counts(); deletes != 0 || result.Remaining != 1 || len(f.owned()) != 2 {
			t.Fatalf("cleanup acted for a destination it cannot read: %+v", result)
		}
		// Naming the destination again lets the cleanup reach its records.
		f.w.mu.Lock()
		f.w.pubs = []Publication{{Destination: f.d, Domains: []string{"example.com"}}}
		f.w.mu.Unlock()
		f.wake()
		f.awaitFinalized()
	})
	t.Run("provider answer lost without a lease change", func(t *testing.T) {
		f := newCleanupFixture(t, cleanupObject("sink", "Delete", true), nil)
		f.awaitPublished()
		f.w.mu.Lock()
		f.w.onDelete = func(*kinds.IntegrationOperation) error { f.w.onDelete = nil; return errCleanupTest }
		f.w.mu.Unlock()
		f.requestDelete()
		// An unclassified provider error is not retried on a timer; the next
		// run of the same lease confirms the absence by rereading.
		f.awaitReason(PhaseTerminating, ReasonProviderFailed)
		if _, _, result := f.status(); result.Pending != 1 {
			t.Fatalf("uncertain delete not reported pending: %+v", result)
		}
		f.wake()
		summary := f.awaitFinalized()
		f.w.mu.Lock()
		deletes, recoveries := f.w.deletes, f.w.recoveries
		f.w.mu.Unlock()
		if deletes != 1 || recoveries != 0 || summary.RecordsForgotten != 1 {
			t.Fatalf("deletes=%d recoveries=%d summary=%+v", deletes, recoveries, summary)
		}
	})
}

// An exhausted request budget stops cleanup before any provider request. The
// budget row is never removed, and cleanup resumes in the next window.
func TestTerminatingBudgetExhaustionMakesNoRequestAndResumes(t *testing.T) {
	f := newCleanupFixture(t, cleanupObject("sink", "Delete", true), nil)
	f.awaitPublished()
	name := kinds.BudgetName("cluster", "test-provider", "account")
	var budget kinds.IntegrationBudget
	if err := f.st.Get(context.Background(), store.Key{Kind: kinds.KindIntegrationBudget, Name: name}, &budget); err != nil {
		t.Fatal(err)
	}
	// Another instance of the cluster used up the shared window.
	budget.Spec.Sequence++
	budget.Spec.WindowStart = f.now()
	budget.Spec.Reservations = []kinds.BudgetReservation{{ID: kinds.Digest([]byte("other instance")), Cost: budget.Spec.Capacity}}
	if _, err := f.st.Apply(context.Background(), &budget, store.ApplyOptions{Manager: "integration-ledger", ExpectedResourceVersion: budget.ResourceVersion}); err != nil {
		t.Fatal(err)
	}
	requests, _, _, _ := f.w.counts()
	f.requestDelete()
	f.awaitReason(PhaseTerminating, ReasonBudgetExhausted)
	time.Sleep(30 * time.Millisecond)
	if after, _, deletes, _ := f.w.counts(); after != requests || deletes != 0 {
		t.Fatalf("requests reached the provider without budget: %d -> %d", requests, after)
	}
	if done, _ := f.finalizations(); len(done) != 0 {
		t.Fatal("finalized without budget")
	}
	f.advance(6 * time.Minute)
	f.wake()
	f.awaitFinalized()
	rows := f.rows(kinds.KindIntegrationBudget)
	if len(rows) != 1 || rows[0].GetName() != name || rows[0].GetUID() != budget.UID {
		t.Fatal("the shared budget row was removed or replaced")
	}
	if after := rows[0].(*kinds.IntegrationBudget); !after.Spec.WindowStart.After(budget.Spec.WindowStart) || len(after.Spec.Reservations) == 0 {
		t.Fatal("cleanup did not charge the next window")
	}
}

// A recreated object of the same name cannot start while the old
// incarnation's terminating job is still inside a root callback.
func TestTerminatingReplacementWaitsForCleanupJoin(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	object := cleanupObject("sink", "Delete", true)
	at := metav1.NewTime(time.Unix(1700000000, 0))
	object.DeletionTimestamp = &at
	f := newCleanupFixture(t, object, func(f *cleanupFixture) {
		f.w.observer = true
		f.purge = func(context.Context) error {
			once.Do(func() { close(entered) })
			<-release // a commit in flight does not end with its context
			return ErrFenced
		}
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the terminating job never purged")
	}
	factories := f.factories.Load()
	// The store now shows another incarnation under the same name.
	f.mu.Lock()
	f.object = cleanupObject("sink", "Retain", false)
	f.object.UID = "uid-recreated"
	f.revision++
	f.mu.Unlock()
	f.h.Wake()
	time.Sleep(50 * time.Millisecond)
	if f.factories.Load() != factories {
		t.Fatal("the replacement started before the terminating job was joined")
	}
	statuses := f.h.Statuses()
	if len(statuses) != 1 || statuses[0].Owner.UID != "uid-sink" {
		t.Fatalf("statuses while the old job is in flight: %+v", statuses)
	}
	close(release)
	f.await("the replacement to start", func() bool {
		statuses := f.h.Statuses()
		return f.factories.Load() > factories && len(statuses) == 1 && statuses[0].Owner.UID == "uid-recreated"
	})
	if done, _ := f.finalizations(); len(done) != 0 {
		t.Fatal("the fenced job finalized")
	}
}

// A cleanup larger than one run continues in the next: a run that reached
// its bound after confirming a delete is followed at once by another, the
// intent it left behind is settled from a reread, and every record set is
// deleted exactly once.
func TestTerminatingCleanupContinuesAcrossBoundedRuns(t *testing.T) {
	var mu sync.Mutex
	asked := 0
	f := newCleanupFixture(t, cleanupObject("sink", "Delete", true), func(f *cleanupFixture) {
		f.records = 4
		f.timeout = 750 * time.Millisecond
	})
	f.awaitPublished()
	if len(f.owned()) != 5 {
		t.Fatalf("published rows: %d", len(f.owned()))
	}
	f.w.mu.Lock()
	// Every second delete of the cleanup is slower than what is left of its
	// run: the provider is not reached before the bound.
	f.w.beforeDelete = func(ctx context.Context) {
		mu.Lock()
		asked++
		slow := asked%2 == 0
		mu.Unlock()
		if slow {
			<-ctx.Done()
		}
	}
	f.w.mu.Unlock()
	f.requestDelete()
	f.patience = 30 * time.Second
	summary := f.awaitFinalized()
	_, _, deletes, _ := f.w.counts()
	if deletes != 4 || len(f.owned()) != 0 {
		t.Fatalf("deletes that reached the provider: %d, owned rows left: %d", deletes, len(f.owned()))
	}
	// One lease, several runs, and every record set confirmed and forgotten.
	if summary.Runs < 3 || summary.RecordSetsDeleted != 4 || summary.RecordsForgotten != 4 {
		t.Fatalf("summary: %+v", summary)
	}
	if _, _, _, provisions := f.w.counts(); provisions != 2 {
		t.Fatalf("the cleanup lease provisioned %d providers", provisions-1)
	}
	if rows := append(f.rows(kinds.KindIntegrationLedger), f.rows(kinds.KindIntegrationOperation)...); len(rows) != 0 {
		t.Fatalf("%d ledger and operation rows remain", len(rows))
	}
}

// A blocked cleanup waits without a concurrency slot: with a single slot,
// another instance still gets its lease.
func TestTerminatingBlockedJobHoldsNoConcurrencySlot(t *testing.T) {
	object := cleanupObject("sink", "Delete", true)
	at := metav1.NewTime(time.Unix(1700000000, 0))
	object.DeletionTimestamp = &at
	f := newCleanupFixture(t, object, func(f *cleanupFixture) {
		f.slots = 1
		f.w.observer = true
		// A ledger row of an instance whose provider does not publish has no
		// destination its cleanup could read.
		record := kinds.RecordIdentity{Owner: f.owner(), Destination: f.d, Key: kinds.RecordKey{Name: "a.example.com.", Type: "A"}}
		if _, err := f.st.Apply(context.Background(), &kinds.IntegrationLedger{ObjectMeta: metav1.ObjectMeta{Name: kinds.LedgerName(record)}, Spec: kinds.IntegrationLedgerSpec{FormatVersion: kinds.FormatVersion, Record: record, Sequence: 1}}, store.ApplyOptions{Manager: "integration-ledger", CreateOnly: true}); err != nil {
			t.Fatal(err)
		}
	})
	f.awaitReason(PhaseTerminating, ReasonDestinationRemoved)
	if _, _, result := f.status(); result.Remaining != 1 {
		t.Fatalf("blocked result: %+v", result)
	}
	other := cleanupObject("other", "Retain", false)
	f.mu.Lock()
	f.extra = append(f.extra, other)
	f.revision++
	f.mu.Unlock()
	f.h.Wake()
	f.await("the other instance to get the only slot", func() bool {
		for _, s := range f.h.Statuses() {
			if s.Owner.Name == "other" && s.Leader && s.Phase != "Waiting" {
				return true
			}
		}
		return false
	})
	if done, _ := f.finalizations(); len(done) != 0 || f.purges.Load() != 0 {
		t.Fatal("the blocked cleanup purged or finalized")
	}
}

// A terminating job takes no operator command, and its guard is not an
// ordinary one.
func TestTerminatingJobRefusesCommands(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	object := cleanupObject("sink", "Delete", true)
	at := metav1.NewTime(time.Unix(1700000000, 0))
	object.DeletionTimestamp = &at
	f := newCleanupFixture(t, object, func(f *cleanupFixture) {
		f.purge = func(context.Context) error {
			once.Do(func() { close(entered) })
			<-release
			return nil
		}
	})
	defer close(release)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the terminating job never purged")
	}
	owner := f.owner()
	if _, err := f.h.CaptureCommand(context.Background(), owner, "1"); !errors.Is(err, ErrFenced) {
		t.Fatalf("command capture on a terminating job: %v", err)
	}
	if _, err := f.h.Sync(context.Background(), owner, ""); !errors.Is(err, ErrFenced) {
		t.Fatalf("sync on a terminating job: %v", err)
	}
	if _, err := f.h.Check(context.Background(), owner); !errors.Is(err, ErrFenced) {
		t.Fatalf("check on a terminating job: %v", err)
	}
	f.h.mu.RLock()
	j := f.h.jobs["dns/sink"]
	normal := j.guard
	f.h.mu.RUnlock()
	// The lease is held under the terminating mode only.
	if _, g, err := f.h.captured(j); err != nil || g.Mode != GuardCleanup || g.Ledger != normal {
		t.Fatalf("captured guard: %+v %v", g, err)
	}
}

func TestInstanceModeFollowsTheStoredObject(t *testing.T) {
	at := metav1.NewTime(time.Unix(1700000000, 0))
	for _, tc := range []struct {
		name               string
		policy             string
		finalizers         []string
		terminating        bool
		mode               GuardMode
		ok                 bool
		unarmedLiveRefused bool
	}{
		{"live retain", "Retain", nil, false, GuardNormal, true, false},
		{"live default", "", nil, false, GuardNormal, true, false},
		{"live delete armed", "Delete", []string{cleanupFinalizer}, false, GuardNormal, true, false},
		{"live delete unarmed", "Delete", nil, false, GuardNormal, true, true},
		{"terminating delete", "Delete", []string{"example.com/other", cleanupFinalizer}, true, GuardCleanup, true, false},
		{"terminating retain with finalizer", "Retain", []string{cleanupFinalizer}, true, GuardRelease, true, false},
		{"terminating default with finalizer", "", []string{cleanupFinalizer}, true, GuardRelease, true, false},
		{"terminating delete without finalizer", "Delete", []string{"example.com/other"}, true, GuardNormal, false, false},
		{"terminating retain without finalizer", "Retain", []string{"example.com/other"}, true, GuardNormal, false, false},
	} {
		d := cleanupObject("sink", tc.policy, false)
		d.Finalizers = tc.finalizers
		if tc.terminating {
			d.DeletionTimestamp = &at
		}
		mode, ok := InstanceMode(d)
		if mode != tc.mode || ok != tc.ok || store.ReservedFinalizerUnarmed(d) != tc.unarmedLiveRefused {
			t.Errorf("%s: mode=%q ok=%v unarmed=%v", tc.name, mode, ok, store.ReservedFinalizerUnarmed(d))
		}
	}
	if _, ok := InstanceMode(nil); ok {
		t.Error("nil object admitted a mode")
	}
}

// cleanupEngine is the serial engine fixture with a ledger that can forget
// and a job held under the cleanup mode.
func cleanupEngine(t *testing.T) (*Host, *job, *fakeRecovery, *time.Time) {
	t.Helper()
	h, j, p, st, now := engineFixture(t)
	repository, err := ledger.New(ledger.Options{Reader: st, Now: func() time.Time { return *now }, Write: func(ctx context.Context, _ ledger.Guard, o store.Object, a store.ApplyOptions) (store.Object, error) {
		return st.Apply(ctx, o, a)
	}, Delete: func(ctx context.Context, _ ledger.Guard, requests []store.DeleteRequest) error {
		for _, request := range requests {
			if err := st.Delete(ctx, request.Key, request.Options); err != nil {
				return err
			}
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	h.opts.Ledger = repository
	recovery := &fakeRecovery{fakePublisher: p}
	j.provider = recovery
	return h, j, recovery, now
}

// Cleanup resumes a partially applied delete although the credential and the
// configuration changed since it was recorded; an ordinary sync refuses the
// same intent. No other partial intent is resumed: nothing is desired any
// more, so its remaining effects are not authorized.
func TestCleanupRecoveryResumesOnlyDeletesAcrossChangedAuthority(t *testing.T) {
	own := func(g ExecutionGuard) plan.RemoteOwner {
		return plan.RemoteOwner{ClusterID: g.Ledger.Owner.ClusterID, IntegrationUID: g.Ledger.Owner.UID}
	}
	rotate := func(h *Host, j *job) {
		h.mu.Lock()
		j.guard.ConfigDigest = kinds.Digest([]byte("edited zone policy"))
		j.guard.CredentialDigest = kinds.Digest([]byte("rotated token"))
		h.mu.Unlock()
	}
	t.Run("partial delete", func(t *testing.T) {
		h, j, p, now := cleanupEngine(t)
		ctx := context.Background()
		f, g, _ := h.captured(j)
		d := p.pubs[0].Destination
		if res, err := h.sync(ctx, j, f, g, ""); err != nil || res.Applied != 1 {
			t.Fatal(res, err)
		}
		published := *sources()[0].Records.Spec.Endpoints[0].DeepCopy()
		published.DNSName = "a.example.com."
		row := func(target, id string) plan.RemoteRecord {
			e := *published.DeepCopy()
			e.Targets = []string{target}
			return plan.RemoteRecord{Endpoint: e, IDs: []string{id}, Owner: own(g)}
		}
		p.remote[d] = RemoteSnapshot{Complete: true, Records: []plan.RemoteRecord{row("192.0.2.1", "a"), row("192.0.2.2", "b")}}
		// A delete removes one row and its answer is lost.
		*now = now.Add(time.Second)
		h.mu.Lock()
		j.mode = GuardCleanup
		h.mu.Unlock()
		f, g, _ = h.captured(j)
		p.apply = func(_ context.Context, o *kinds.IntegrationOperation) (kinds.Receipt, error) {
			if o.Spec.Action != "delete" || o.Spec.Policy != string(plan.PolicySync) {
				t.Fatalf("cleanup intent: %s under %q", o.Spec.Action, o.Spec.Policy)
			}
			p.remote[d] = RemoteSnapshot{Complete: true, Records: []plan.RemoteRecord{row("192.0.2.2", "b")}}
			return kinds.Receipt{ObservedAt: *now, Targets: []kinds.TargetResult{{Target: "192.0.2.1", ErrorClass: "ambiguous"}}}, errCleanupTest
		}
		st := &terminatingLease{}
		if clean, out := h.cleanupRecords(ctx, j, f, g, st); clean || out.result.Pending != 1 || !errors.Is(out.err, ErrProvider) {
			t.Fatalf("first run: clean=%v %+v", clean, out)
		}
		*now = now.Add(time.Second)
		rotate(h, j)
		p.recover = func(_ context.Context, o *kinds.IntegrationOperation, s RemoteSnapshot) (kinds.Receipt, error) {
			if !s.Complete || len(s.Records) != 1 {
				t.Fatal("resumed without the fresh complete snapshot")
			}
			p.remote[d] = RemoteSnapshot{Complete: true}
			return kinds.Receipt{Complete: true, ContentHash: o.Spec.AfterHash, ObservedAt: *now, Targets: slices.Clone(o.Spec.Receipt.Targets)}, nil
		}
		// An ordinary job with the changed authority leaves the intent pending.
		h.mu.Lock()
		j.mode = GuardNormal
		h.mu.Unlock()
		normalFrame, normal, _ := h.captured(j)
		if _, err := h.sync(ctx, j, normalFrame, normal, ""); !errors.Is(err, ledger.ErrPending) || p.recoveries != 0 {
			t.Fatalf("ordinary sync resumed across changed authority: %v", err)
		}
		// So does a cleanup plan under a guard that is not the cleanup mode.
		pretender := &terminatingLease{}
		if clean, out := h.cleanupRecords(ctx, j, normalFrame, normal, pretender); clean || out.reason != ReasonPendingRecovery || p.recoveries != 0 {
			t.Fatalf("cleanup input under an ordinary guard resumed: %+v", out)
		}
		h.mu.Lock()
		j.mode = GuardCleanup
		h.mu.Unlock()
		f, g, _ = h.captured(j)
		clean, out := h.cleanupRecords(ctx, j, f, g, st)
		if !clean || p.recoveries != 1 || out.result.Remaining != 0 || st.summary.RecordsForgotten != 1 {
			t.Fatalf("cleanup did not resume the partial delete: clean=%v %+v recoveries=%d", clean, out, p.recoveries)
		}
		if rows, _ := h.ownerLedgers(ctx, g.Ledger.Owner); len(rows) != 0 {
			t.Fatal("ledger row remains")
		}
	})
	t.Run("partial create stays pending", func(t *testing.T) {
		h, j, p, now := cleanupEngine(t)
		ctx := context.Background()
		f, g, _ := h.captured(j)
		d := p.pubs[0].Destination
		var intent *kinds.IntegrationOperation
		p.apply = func(_ context.Context, o *kinds.IntegrationOperation) (kinds.Receipt, error) {
			intent = o
			e := *o.Spec.After.DeepCopy()
			e.Targets = e.Targets[:1]
			p.remote[d] = RemoteSnapshot{Complete: true, Records: []plan.RemoteRecord{{Endpoint: e, IDs: []string{"a"}, Owner: own(g)}}}
			return kinds.Receipt{ObservedAt: *now, Targets: []kinds.TargetResult{{Target: e.Targets[0], ProviderID: "a", Applied: true}}}, nil
		}
		if _, err := h.sync(ctx, j, f, g, ""); !errors.Is(err, ledger.ErrPending) {
			t.Fatal(err)
		}
		*now = now.Add(time.Second)
		h.mu.Lock()
		j.mode = GuardCleanup
		h.mu.Unlock()
		f, g, _ = h.captured(j)
		p.recover = func(context.Context, *kinds.IntegrationOperation, RemoteSnapshot) (kinds.Receipt, error) {
			t.Fatal("cleanup resumed a create")
			return kinds.Receipt{}, nil
		}
		st := &terminatingLease{}
		clean, out := h.cleanupRecords(ctx, j, f, g, st)
		if clean || out.reason != ReasonPendingRecovery || out.result.Pending != 1 || p.calls != 1 {
			t.Fatalf("partial create in cleanup: clean=%v %+v", clean, out)
		}
		l, err := h.opts.Ledger.ReadLedger(ctx, intent.Spec.Record)
		if err != nil || l.Spec.Pending == nil {
			t.Fatal("the uncertain create was settled without evidence", err)
		}
	})
}
