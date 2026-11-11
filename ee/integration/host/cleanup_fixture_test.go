package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// cleanupWorld is the provider side of the terminating-job tests: one remote
// zone shared by every provider incarnation the factory builds, as a real
// provider account is. Every remote call goes through the host's request
// adapter, so the real budget, fence and lease checks stand between the job
// and the world; requests counts only calls that reached it.
type cleanupWorld struct {
	mu         sync.Mutex
	pubs       []Publication
	remote     map[kinds.Destination][]plan.RemoteRecord
	incomplete bool
	next       int
	events     []string
	provisions int
	requests   int
	creates    int
	deletes    int
	recoveries int
	// onDelete runs inside the provider's delete request, after the rows are
	// gone. A non-nil error is the provider's answer instead of a receipt.
	onDelete func(*kinds.IntegrationOperation) error
	// beforeDelete runs before the provider's delete request, outside the
	// world's lock, with the context of the run that asked for it.
	beforeDelete func(context.Context)
	// hangAfterDelete makes a delete request, once it reached the world,
	// wait for its context and answer like the Cloudflare adapter does when
	// the reread after an unacknowledged batch fails: a partial receipt
	// that marks the target ambiguous, with the context's error.
	hangAfterDelete bool
	// observer builds providers that only observe.
	observer bool
	now      func() time.Time
}

func (w *cleanupWorld) event(format string, args ...any) {
	w.events = append(w.events, fmt.Sprintf(format, args...))
}

func (w *cleanupWorld) snapshot(d kinds.Destination) []plan.RemoteRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]plan.RemoteRecord, 0, len(w.remote[d]))
	for _, row := range w.remote[d] {
		row.Endpoint = *row.Endpoint.DeepCopy()
		row.IDs = slices.Clone(row.IDs)
		out = append(out, row)
	}
	return out
}

func (w *cleanupWorld) counts() (requests, creates, deletes, provisions int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.requests, w.creates, w.deletes, w.provisions
}

func (w *cleanupWorld) seed(d kinds.Destination, name, typ, target, id string, owner plan.RemoteOwner) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.remote[d] = append(w.remote[d], plan.RemoteRecord{Endpoint: public.Endpoint{DNSName: name, RecordType: typ, RecordTTL: 300, Targets: []string{target}}, IDs: []string{id}, Version: "seed", Owner: owner})
}

func (w *cleanupWorld) drop(d kinds.Destination, id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.remote[d] = slices.DeleteFunc(w.remote[d], func(row plan.RemoteRecord) bool { return slices.Contains(row.IDs, id) })
}

// cleanupProvider is one provider incarnation. As a publisher it writes one
// remote row per target, stamped with its owner, like the Cloudflare adapter.
type cleanupProvider struct {
	w     *cleanupWorld
	host  RequestHost
	owner InstanceID
	index int
}

func (*cleanupProvider) Manifest() integration.Manifest { return integration.Manifest{Type: "webhook"} }
func (p *cleanupProvider) Provision(_ context.Context, h integration.Host, _ json.RawMessage, _ integration.SecretResolver) error {
	owned, ok := h.(OwnerHost)
	if !ok {
		return ErrUnsupported
	}
	p.host, p.owner = owned, owned.IntegrationOwner()
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	p.w.provisions++
	p.index = p.w.provisions
	p.w.event("provision#%d", p.index)
	return nil
}
func (*cleanupProvider) Validate(context.Context) error { return nil }
func (p *cleanupProvider) request(ctx context.Context, mutating bool, fn func() error) error {
	return p.host.Request(ctx, "test-provider", "account", mutating, func(context.Context) error {
		p.w.mu.Lock()
		defer p.w.mu.Unlock()
		p.w.requests++
		return fn()
	})
}
func (p *cleanupProvider) Check(ctx context.Context) (integration.Status, error) {
	if err := p.request(ctx, false, func() error { return nil }); err != nil {
		return integration.Status{}, err
	}
	return integration.Status{Reachable: true, Authenticated: true}, nil
}
func (*cleanupProvider) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (p *cleanupProvider) Stop(context.Context) error {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	p.w.event("stop#%d", p.index)
	return nil
}

// cleanupPublisher adds the publication seam. Observer-only providers do not
// have it, so the host must not treat them as owners of remote records.
type cleanupPublisher struct{ *cleanupProvider }

func (p cleanupPublisher) Destinations(context.Context) ([]Publication, error) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	return slices.Clone(p.w.pubs), nil
}
func (p cleanupPublisher) Records(ctx context.Context, d kinds.Destination) (RemoteSnapshot, error) {
	var out RemoteSnapshot
	err := p.request(ctx, false, func() error {
		out.Complete = !p.w.incomplete
		for _, row := range p.w.remote[d] {
			row.Endpoint = *row.Endpoint.DeepCopy()
			row.IDs = slices.Clone(row.IDs)
			out.Records = append(out.Records, row)
		}
		return nil
	})
	return out, err
}
func (cleanupPublisher) Adjust(_ context.Context, _ Publication, e []public.Endpoint) ([]public.Endpoint, error) {
	return e, nil
}
func (p cleanupPublisher) Apply(ctx context.Context, op *kinds.IntegrationOperation) (kinds.Receipt, error) {
	return p.effect(ctx, op)
}
func (p cleanupPublisher) Recover(ctx context.Context, op *kinds.IntegrationOperation, s RemoteSnapshot) (kinds.Receipt, error) {
	if !s.Complete {
		return kinds.Receipt{}, plan.ErrIncomplete
	}
	p.w.mu.Lock()
	p.w.recoveries++
	p.w.mu.Unlock()
	return p.effect(ctx, op)
}
func (p cleanupPublisher) effect(ctx context.Context, op *kinds.IntegrationOperation) (kinds.Receipt, error) {
	var receipt kinds.Receipt
	var hook error
	p.w.mu.Lock()
	before := p.w.beforeDelete
	p.w.mu.Unlock()
	if before != nil && op.Spec.After == nil {
		before(ctx)
	}
	err := p.request(ctx, true, func() error {
		d, key := op.Spec.Record.Destination, op.Spec.Record.Key
		p.w.remote[d] = slices.DeleteFunc(p.w.remote[d], func(row plan.RemoteRecord) bool {
			n, err := plan.Normalize(row.Endpoint)
			return err == nil && n.DNSName == key.Name && n.RecordType == key.Type && row.Owner.IntegrationUID == p.owner.UID
		})
		receipt = kinds.Receipt{Complete: true, ContentHash: op.Spec.AfterHash, ObservedAt: p.w.now()}
		if op.Spec.After == nil {
			p.w.deletes++
			if p.w.onDelete != nil {
				hook = p.w.onDelete(op)
			}
			return nil
		}
		p.w.creates++
		for _, target := range op.Spec.After.Targets {
			p.w.next++
			id := "id-" + strconv.Itoa(p.w.next)
			row := op.Spec.After.DeepCopy()
			row.Targets = []string{target}
			p.w.remote[d] = append(p.w.remote[d], plan.RemoteRecord{Endpoint: *row, IDs: []string{id}, Version: "v-" + id, Owner: plan.RemoteOwner{ClusterID: p.owner.ClusterID, IntegrationUID: p.owner.UID}})
			receipt.ProviderIDs = append(receipt.ProviderIDs, id)
			receipt.Targets = append(receipt.Targets, kinds.TargetResult{Target: target, ProviderID: id, Applied: true})
		}
		slices.Sort(receipt.ProviderIDs)
		return nil
	})
	if err != nil {
		return kinds.Receipt{}, err
	}
	if hook != nil {
		return kinds.Receipt{}, hook
	}
	p.w.mu.Lock()
	hang := p.w.hangAfterDelete && op.Spec.After == nil
	p.w.mu.Unlock()
	if hang {
		<-ctx.Done()
		return kinds.Receipt{ObservedAt: p.w.now(), Targets: []kinds.TargetResult{{Target: op.Spec.Record.Key.Name, ErrorClass: "ambiguous"}}}, ctx.Err()
	}
	return receipt, nil
}

// cleanupFixture runs the real Host loop over a memory ledger. Its callbacks
// stand in for root: Fence, Purge and Finalize check the guard against the
// object as it is stored at that moment, by exact UID, resource version and
// mode; a published status that differs from the last one is a new resource
// version of the object, as a real status write is; Finalize removes the
// object.
type cleanupFixture struct {
	t      *testing.T
	h      *Host
	st     *memory.Store
	w      *cleanupWorld
	d      kinds.Destination
	mu     sync.Mutex
	object *public.Integration
	extra  []*public.Integration
	// poke changes the captured sources, which wakes a waiting job.
	poke     int
	revision uint64
	clock    time.Time
	allowed  atomic.Bool
	// ended is root's serving lifetime being over.
	ended     atomic.Bool
	factories atomic.Int32
	purges    atomic.Int32
	finalized []CleanupSummary
	modes     []GuardMode
	refused   atomic.Int32
	published []InstanceStatus
	finalize  func(context.Context) error
	purge     func(context.Context) error
	elector   func(string) store.Elector
	// input edits each capture, under the fixture lock.
	input func(*Input)
	// slots is the host's concurrency bound; two unless set.
	slots int
	// timeout is the host's operation timeout; five seconds unless set.
	timeout time.Duration
	// records is how many record sets the captured sources publish.
	records int
	// patience bounds each await; ten seconds unless set.
	patience time.Duration
	// batch is the host's forget batch; the default unless set.
	batch int
	// refuse makes root refuse every guard, as a replaced cut does.
	refuse atomic.Bool
	// forgets is the number of deletes of each ledger forget, in commit order.
	forgetMu sync.Mutex
	forgets  []int
	// forgetCost is how long each delete of a ledger forget takes; a forget
	// that its context ends first commits nothing.
	forgetCost time.Duration
	// ledgerRefused makes the host's paged reads of ledger rows fail.
	ledgerRefused atomic.Bool
	// operationsStalled makes the host's paged reads of operations wait for
	// their context, as a store too slow for the run's bound does.
	operationsStalled atomic.Bool
	// writesStalled makes the ledger repository's writes of operations
	// (intents, receipts and confirmations) wait for their context.
	writesStalled atomic.Bool
}

// refusingReader is the host's reader with ledger reads that can be made to
// fail, as a store read that errors does.
type refusingReader struct{ f *cleanupFixture }

func (r refusingReader) List(ctx context.Context, kind store.Kind, o store.ListOptions) (store.ObjectList, error) {
	if kind == kinds.KindIntegrationLedger && r.f.ledgerRefused.Load() {
		return store.ObjectList{}, errCleanupTest
	}
	if kind == kinds.KindIntegrationOperation && r.f.operationsStalled.Load() {
		<-ctx.Done()
		return store.ObjectList{}, ctx.Err()
	}
	return r.f.st.List(ctx, kind, o)
}

const cleanupFinalizer = store.IntegrationCleanupFinalizer

func cleanupObject(name, policy string, finalizer bool) *public.Integration {
	d := webhook(name)
	d.ResourceVersion, d.Generation = "1", 1
	d.Spec.DeletionPolicy = policy
	d.Spec.Resync = metav1.Duration{Duration: time.Hour}
	if finalizer {
		d.Finalizers = []string{cleanupFinalizer}
	}
	return d
}

func newCleanupFixture(t *testing.T, object *public.Integration, configure func(*cleanupFixture)) *cleanupFixture {
	t.Helper()
	f := &cleanupFixture{t: t, st: memory.New(), object: object.DeepCopy(), revision: 1, clock: time.Unix(1700000000, 0).UTC(), d: kinds.Destination{Provider: "fake", Account: "account", Zone: "example.com"}}
	f.allowed.Store(true)
	f.w = &cleanupWorld{remote: map[kinds.Destination][]plan.RemoteRecord{}, now: f.now}
	f.w.pubs = []Publication{{Destination: f.d, Domains: []string{"example.com"}}}
	if configure != nil {
		configure(f)
	}
	repository, err := ledger.New(ledger.Options{Reader: f.st, Now: f.now, Write: func(ctx context.Context, g ledger.Guard, o store.Object, a store.ApplyOptions) (store.Object, error) {
		var out store.Object
		err := f.h.WithLease(ctx, g, func(ctx context.Context) error {
			if _, operation := o.(*kinds.IntegrationOperation); operation && f.writesStalled.Load() {
				<-ctx.Done()
				return ctx.Err()
			}
			var err error
			out, err = f.st.Apply(ctx, o, a)
			return err
		})
		return out, err
	}, Delete: func(ctx context.Context, g ledger.Guard, requests []store.DeleteRequest) error {
		return f.h.WithLease(ctx, g, func(ctx context.Context) error {
			if f.forgetCost > 0 {
				select {
				case <-time.After(f.forgetCost * time.Duration(len(requests))):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			for _, request := range requests {
				if err := f.st.Delete(ctx, request.Key, request.Options); err != nil {
					return err
				}
			}
			f.forgetMu.Lock()
			f.forgets = append(f.forgets, len(requests))
			f.forgetMu.Unlock()
			return nil
		})
	}})
	if err != nil {
		t.Fatal(err)
	}
	elector := f.elector
	if elector == nil {
		elector = f.st.Elector
	}
	if f.slots == 0 {
		f.slots = 2
	}
	if f.timeout == 0 {
		f.timeout = 5 * time.Second
	}
	if f.records == 0 {
		f.records = 1
	}
	f.h, err = New(Options{Capture: f.capture, Elector: elector, Ledger: repository, Reader: refusingReader{f}, Now: f.now,
		Factory: func(string) (integration.Integration, error) {
			f.factories.Add(1)
			base := &cleanupProvider{w: f.w}
			f.w.mu.Lock()
			observer := f.w.observer
			f.w.mu.Unlock()
			if observer {
				return base, nil
			}
			return cleanupPublisher{base}, nil
		},
		Allowed: f.allowed.Load,
		Alive:   func() bool { return !f.ended.Load() },
		Fence:   func(_ context.Context, g ExecutionGuard) error { return f.admits(g) },
		Status: func(_ context.Context, g ExecutionGuard, s InstanceStatus) error {
			if err := f.admits(g); err != nil {
				return err
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if n := len(f.published); n > 0 && f.published[n-1].Phase == s.Phase && f.published[n-1].Reason == s.Reason && f.published[n-1].Result == s.Result {
				return nil
			}
			f.published = append(f.published, s)
			f.revision++
			f.object.ResourceVersion = strconv.FormatUint(f.revision, 10)
			return nil
		},
		Purge: func(ctx context.Context, g ExecutionGuard) (PurgeResult, error) {
			if err := f.admits(g); err != nil || g.Mode != GuardCleanup {
				return PurgeResult{}, ErrFenced
			}
			f.purges.Add(1)
			if f.purge != nil {
				if err := f.purge(ctx); err != nil {
					return PurgeResult{}, err
				}
			}
			return PurgeResult{Objects: 2, SourceRows: 1}, nil
		},
		Finalize: func(ctx context.Context, g ExecutionGuard, summary CleanupSummary) error {
			if err := f.admits(g); err != nil {
				f.refused.Add(1)
				return err
			}
			if f.finalize != nil {
				if err := f.finalize(ctx); err != nil {
					return err
				}
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.finalized = append(f.finalized, summary)
			f.modes = append(f.modes, g.Mode)
			f.object = nil
			return nil
		},
		PollInterval: time.Millisecond, OperationTimeout: f.timeout, StopTimeout: time.Second, MaxConcurrent: f.slots})
	if err != nil {
		t.Fatal(err)
	}
	if f.batch > 0 {
		f.h.forgetBatch = f.batch
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.h.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("host did not quiesce")
		}
	})
	return f
}

// now is a strictly increasing clock: the ledger requires an observation to
// be later than the intent it settles.
func (f *cleanupFixture) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = f.clock.Add(time.Millisecond)
	return f.clock
}

func (f *cleanupFixture) advance(d time.Duration) {
	f.mu.Lock()
	f.clock = f.clock.Add(d)
	f.mu.Unlock()
}

func (f *cleanupFixture) capture(context.Context) (Input, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var objects []store.Object
	if f.object != nil {
		objects = append(objects, f.object.DeepCopy())
	}
	for _, d := range f.extra {
		objects = append(objects, d.DeepCopy())
	}
	snapshot, err := zonecache.Prepare(zonecache.PrepareOptions{Kinds: []store.Kind{"Integration"}, Generation: f.revision, Now: time.Now()}, objects, nil)
	if err != nil {
		return Input{}, err
	}
	in := Input{Objects: snapshot.Objects, Term: replication.Term{ClusterID: "cluster", Epoch: 1, PrimaryID: "primary", PromotionID: "promotion"}, BundleID: "bundle-1", Complete: true, Sources: sources()}
	if f.poke > 0 {
		in.Sources[0].Zone.Labels = map[string]string{"poke": strconv.Itoa(f.poke)}
	}
	for n := 1; n < f.records; n++ {
		in.Sources[0].Records.Spec.Endpoints = append(in.Sources[0].Records.Spec.Endpoints, public.Endpoint{DNSName: "extra-" + strconv.Itoa(n) + ".example.com", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2." + strconv.Itoa(100+n)}})
	}
	if f.input != nil {
		f.input(&in)
	}
	return in, nil
}

// admits is root's commit-boundary check, reduced to what the host must get
// right: the exact stored object and a mode that object allows.
func (f *cleanupFixture) admits(g ExecutionGuard) error {
	if f.refuse.Load() {
		return ErrFenced
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.object
	if d == nil || string(d.UID) != g.Ledger.Owner.UID || d.ResourceVersion != g.Ledger.IntegrationResourceVersion {
		return ErrFenced
	}
	mode, ok := InstanceMode(d)
	if !ok || mode != g.Mode || mode == GuardNormal && store.ReservedFinalizerUnarmed(d) {
		return ErrFenced
	}
	return nil
}

// change edits the stored object as one write: a new resource version.
func (f *cleanupFixture) change(edit func(*public.Integration)) {
	f.mu.Lock()
	edit(f.object)
	f.revision++
	f.object.ResourceVersion = strconv.FormatUint(f.revision, 10)
	f.mu.Unlock()
	f.h.Wake()
}

// requestDelete is what a store does with a delete of an object that has
// finalizers: it sets the deletion timestamp and keeps the object.
func (f *cleanupFixture) requestDelete() {
	f.change(func(d *public.Integration) {
		at := metav1.NewTime(time.Unix(1700000000, 0))
		d.DeletionTimestamp = &at
	})
}

func (f *cleanupFixture) wake() {
	f.mu.Lock()
	f.poke++
	f.mu.Unlock()
	f.h.Wake()
}

func (f *cleanupFixture) await(what string, fn func() bool) {
	f.t.Helper()
	patience := f.patience
	if patience == 0 {
		patience = 10 * time.Second
	}
	deadline := time.Now().Add(patience)
	for !fn() {
		if time.Now().After(deadline) {
			f.dump()
			f.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (f *cleanupFixture) dump() {
	f.t.Helper()
	for _, s := range f.h.Statuses() {
		f.t.Logf("status %s/%s leader=%v result=%+v", s.Phase, s.Reason, s.Leader, s.Result)
	}
	f.w.mu.Lock()
	f.t.Logf("world requests=%d creates=%d deletes=%d recoveries=%d events=%v", f.w.requests, f.w.creates, f.w.deletes, f.w.recoveries, f.w.events)
	f.w.mu.Unlock()
}

func (f *cleanupFixture) status() (string, string, Result) {
	for _, s := range f.h.Statuses() {
		if s.Owner.Name == "sink" {
			return s.Phase, s.Reason, s.Result
		}
	}
	return "", "", Result{}
}

func (f *cleanupFixture) awaitReason(phase, reason string) {
	f.t.Helper()
	f.await(phase+"/"+reason, func() bool { p, r, _ := f.status(); return p == phase && r == reason })
}

func (f *cleanupFixture) finalizations() ([]CleanupSummary, []GuardMode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.finalized), slices.Clone(f.modes)
}

func (f *cleanupFixture) awaitFinalized() CleanupSummary {
	f.t.Helper()
	f.await("finalization", func() bool { done, _ := f.finalizations(); return len(done) > 0 })
	// The object is gone: the host forgets it and nothing finalizes again.
	f.await("the finalized instance to leave the host", func() bool { return len(f.h.Statuses()) == 0 })
	time.Sleep(20 * time.Millisecond)
	done, _ := f.finalizations()
	if len(done) != 1 {
		f.t.Fatalf("finalized %d times", len(done))
	}
	return done[0]
}

func (f *cleanupFixture) rows(kind store.Kind) []store.Object {
	f.t.Helper()
	list, err := f.st.List(context.Background(), kind, store.ListOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	return list.Items
}

// awaitPublished waits until the ordinary job confirmed its one record set.
func (f *cleanupFixture) awaitPublished() *kinds.IntegrationLedger {
	f.t.Helper()
	var row *kinds.IntegrationLedger
	f.await("the confirmed publication", func() bool {
		for _, obj := range f.rows(kinds.KindIntegrationLedger) {
			l := obj.(*kinds.IntegrationLedger)
			if l.Spec.Pending == nil && len(l.Spec.ProviderIDs) == 2 {
				row = l
				return true
			}
		}
		return false
	})
	// Its status write is a new resource version; wait until it is out. Rows
	// of other owners in the zone are reported as conflicts, not as failure.
	f.await("the reported publication", func() bool {
		phase, _, result := f.status()
		return (phase == "Ready" || phase == "Degraded") && result.Applied == f.records && result.Pending == 0
	})
	return row
}

func (f *cleanupFixture) owner() InstanceID {
	return InstanceID{ClusterID: "cluster", Namespace: "dns", Name: "sink", UID: "uid-sink"}
}

func (f *cleanupFixture) owned() []plan.RemoteRecord {
	var out []plan.RemoteRecord
	for _, row := range f.w.snapshot(f.d) {
		if row.Owner.IntegrationUID == "uid-sink" {
			out = append(out, row)
		}
	}
	return out
}

var errCleanupTest = errors.New("test: provider answer lost")
