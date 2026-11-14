package host

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type fakePublisher struct {
	pubs      []Publication
	remote    map[kinds.Destination]RemoteSnapshot
	apply     func(context.Context, *kinds.IntegrationOperation) (kinds.Receipt, error)
	adjust    func([]public.Endpoint) []public.Endpoint
	calls     int
	check     func(context.Context) (integration.Status, error)
	start     func(context.Context) error
	stop      func(context.Context) error
	provision func(context.Context) error
}

func (*fakePublisher) Manifest() integration.Manifest { return integration.Manifest{Type: "webhook"} }
func (p *fakePublisher) Provision(ctx context.Context, _ integration.Host, _ json.RawMessage, _ integration.SecretResolver) error {
	if p.provision != nil {
		return p.provision(ctx)
	}
	return nil
}
func (*fakePublisher) Validate(context.Context) error { return nil }
func (p *fakePublisher) Check(ctx context.Context) (integration.Status, error) {
	if p.check != nil {
		return p.check(ctx)
	}
	return integration.Status{Reachable: true, Authenticated: true}, nil
}
func (p *fakePublisher) Start(ctx context.Context) error {
	if p.start != nil {
		return p.start(ctx)
	}
	<-ctx.Done()
	return nil
}
func (p *fakePublisher) Stop(ctx context.Context) error {
	if p.stop != nil {
		return p.stop(ctx)
	}
	return nil
}
func (p *fakePublisher) Destinations(context.Context) ([]Publication, error) { return p.pubs, nil }
func (p *fakePublisher) Records(_ context.Context, d kinds.Destination) (RemoteSnapshot, error) {
	return p.remote[d], nil
}
func (p *fakePublisher) Adjust(_ context.Context, _ Publication, e []public.Endpoint) ([]public.Endpoint, error) {
	if p.adjust != nil {
		return p.adjust(e), nil
	}
	return e, nil
}
func (p *fakePublisher) Apply(ctx context.Context, o *kinds.IntegrationOperation) (kinds.Receipt, error) {
	p.calls++
	return p.apply(ctx, o)
}
func webhook(name string) *public.Integration {
	return &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "dns", UID: types.UID("uid-" + name)}, Spec: public.IntegrationSpec{Type: "webhook", Webhook: &public.WebhookIntegration{Endpoint: "https://provider.example"}}}
}
func sources() []plan.Source {
	return []plan.Source{{Zone: &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "zone", Namespace: "dns", UID: "zone-uid", ResourceVersion: "1"}, Spec: public.DNSZoneSpec{ZoneName: "example.com"}}, Records: &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "record", Namespace: "dns", UID: "record-uid", ResourceVersion: "1"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "zone"}, Endpoints: []public.Endpoint{{DNSName: "a.example.com", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.1", "192.0.2.2"}}}}}}}
}
func engineFixture(t *testing.T) (*Host, *job, *fakePublisher, *memory.Store, *time.Time) {
	t.Helper()
	st := memory.New()
	in := captureInput(t, webhook("sink"))
	in.Sources = sources()
	instances, err := CaptureInstances(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0).UTC()
	var h *Host
	r, err := ledger.New(ledger.Options{Reader: st, Now: func() time.Time { return now }, Write: func(ctx context.Context, g ledger.Guard, o store.Object, a store.ApplyOptions) (store.Object, error) {
		var result store.Object
		err := h.WithLease(ctx, g, func(ctx context.Context) error { var err error; result, err = st.Apply(ctx, o, a); return err })
		return result, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	h, err = New(Options{Capture: func(context.Context) (Input, error) { return in, nil }, Elector: st.Elector, Fence: func(context.Context, ExecutionGuard) error { return nil }, Ledger: r, Reader: st, Allowed: func() bool { return true }, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	g, err := instances[0].Guard("lease")
	if err != nil {
		t.Fatal(err)
	}
	d := kinds.Destination{Provider: "fake", Account: "account", Zone: "example.com"}
	p := &fakePublisher{pubs: []Publication{{Destination: d, Domains: []string{"example.com"}}}, remote: map[kinds.Destination]RemoteSnapshot{d: {Complete: true}}}
	j := &job{key: instanceKey(g.Owner), ctx: ctx, cancel: cancel, done: make(chan struct{}), frame: frame{instances[0], in}, guard: g, active: true, leaseCtx: ctx, provider: p}
	h.jobs[j.key] = j
	p.apply = func(_ context.Context, o *kinds.IntegrationOperation) (kinds.Receipt, error) {
		return confirmedReceipt(o, now), nil
	}
	return h, j, p, st, &now
}
func confirmedReceipt(o *kinds.IntegrationOperation, now time.Time) kinds.Receipt {
	r := kinds.Receipt{Complete: true, ContentHash: o.Spec.AfterHash, ObservedAt: now}
	if o.Spec.After != nil {
		for i, target := range o.Spec.After.Targets {
			id := string(rune('a' + i))
			r.ProviderIDs = append(r.ProviderIDs, id)
			r.Targets = append(r.Targets, kinds.TargetResult{Target: target, ProviderID: id, Applied: true})
		}
	}
	return r
}
func TestPreviewDeterminismCompletenessAdjustmentAndStale(t *testing.T) {
	h, j, p, _, _ := engineFixture(t)
	ctx := context.Background()
	f, g, _ := h.captured(j)
	d := p.pubs[0].Destination
	other := d
	other.Account = "second"
	p.pubs = append(p.pubs, Publication{Destination: other})
	p.remote[other] = RemoteSnapshot{Complete: true}
	a, err := h.preview(ctx, j, f, g)
	if err != nil {
		t.Fatal(err)
	}
	p.pubs[0], p.pubs[1] = p.pubs[1], p.pubs[0]
	b, err := h.preview(ctx, j, f, g)
	if err != nil || a.Digest != b.Digest || len(a.Plans) != 2 {
		t.Fatal("nondeterministic complete preview", err)
	}
	if _, err := h.sync(ctx, j, f, g, "obsolete"); !errors.Is(err, ErrStalePreview) || p.calls != 0 {
		t.Fatal("stale preview applied", err)
	}
	p.remote[d] = RemoteSnapshot{Complete: false}
	if _, err := h.sync(ctx, j, f, g, ""); !errors.Is(err, plan.ErrIncomplete) || p.calls != 0 {
		t.Fatal("partial pages authorized changes", err)
	}
	p.remote[d] = RemoteSnapshot{Complete: true}
	f.input.Complete = false
	if _, err := h.sync(ctx, j, f, g, ""); !errors.Is(err, plan.ErrIncomplete) {
		t.Fatal(err)
	}
	f.input.Complete = true
	p.adjust = func(e []public.Endpoint) []public.Endpoint { e[0].DNSName = "escaped.other.test"; return e }
	if _, err := h.sync(ctx, j, f, g, ""); !errors.Is(err, ErrProvider) || p.calls != 0 {
		t.Fatal("adjust escaped selected owners", err)
	}
}
func TestIntentReceiptConfirmationOrderingAndFailures(t *testing.T) {
	for _, failKind := range []string{"", kinds.KindIntegrationOperation, kinds.KindIntegrationLedger} {
		t.Run("receipt_failure_"+failKind, func(t *testing.T) {
			h, j, p, st, now := engineFixture(t)
			ctx := context.Background()
			f, g, _ := h.captured(j)
			applied := false
			r, err := ledger.New(ledger.Options{Reader: st, Now: func() time.Time { return *now }, Write: func(ctx context.Context, _ ledger.Guard, o store.Object, a store.ApplyOptions) (store.Object, error) {
				if applied && string(store.KindOf(o)) == failKind {
					return nil, store.ErrClosed
				}
				return st.Apply(ctx, o, a)
			}})
			if err != nil {
				t.Fatal(err)
			}
			h.opts.Ledger = r
			var operation *kinds.IntegrationOperation
			p.apply = func(_ context.Context, o *kinds.IntegrationOperation) (kinds.Receipt, error) {
				operation = o
				durable, err := r.ReadOperation(ctx, o.Spec.OperationID)
				if err != nil || durable.Spec.Receipt != nil {
					t.Fatal("provider before durable intent", err)
				}
				l, err := r.ReadLedger(ctx, o.Spec.Record)
				if err != nil || l.Spec.Pending == nil || l.Spec.ContentHash != "" {
					t.Fatal("provider before pending reservation", err)
				}
				applied = true
				return confirmedReceipt(o, *now), nil
			}
			result, err := h.sync(ctx, j, f, g, "")
			if failKind == "" {
				if err != nil || result.Applied != 1 {
					t.Fatal(result, err)
				}
			} else if !errors.Is(err, store.ErrClosed) || result.Pending != 1 {
				t.Fatal(result, err)
			}
			l, err := r.ReadLedger(ctx, operation.Spec.Record)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := r.ReadOperation(ctx, operation.Spec.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if failKind == kinds.KindIntegrationOperation && saved.Spec.Receipt != nil {
				t.Fatal("failed receipt claimed durable")
			}
			if failKind == kinds.KindIntegrationLedger && saved.Spec.Receipt == nil {
				t.Fatal("confirmation before receipt")
			}
			if failKind != "" && (l.Spec.ContentHash != "" || l.Spec.Pending == nil) {
				t.Fatal("failed write advanced baseline")
			}
		})
	}
}
func TestRestartRecoveryPartialAndForeignOwnership(t *testing.T) {
	h, j, p, _, now := engineFixture(t)
	ctx := context.Background()
	f, g, _ := h.captured(j)
	d := p.pubs[0].Destination
	var intent *kinds.IntegrationOperation
	p.apply = func(_ context.Context, o *kinds.IntegrationOperation) (kinds.Receipt, error) {
		intent = o
		return kinds.Receipt{ObservedAt: *now, Targets: []kinds.TargetResult{{Target: o.Spec.After.Targets[0], ProviderID: "a", Applied: true}}}, errors.New("sensitive transport details")
	}
	result, err := h.sync(ctx, j, f, g, "")
	if !errors.Is(err, ErrProvider) || result.Pending != 1 {
		t.Fatal(result, err)
	}
	l, err := h.opts.Ledger.ReadLedger(ctx, intent.Spec.Record)
	if err != nil || l.Spec.ContentHash != "" {
		t.Fatal("partial confirmed", err)
	}
	*now = now.Add(time.Second)
	// Same durable repository, new worker lease: complete fresh remote facts recover
	// a crash without a duplicate Apply. Foreign rows cannot prove completion.
	j.guard.LeaseID = "restart"
	f, g, _ = h.captured(j)
	p.remote[d] = RemoteSnapshot{Complete: true, Records: []plan.RemoteRecord{{Endpoint: *intent.Spec.After.DeepCopy(), IDs: []string{"a", "b"}, Owner: plan.RemoteOwner{ClusterID: "cluster", IntegrationUID: "foreign"}}}}
	if _, err := h.sync(ctx, j, f, g, ""); !errors.Is(err, ledger.ErrPending) || p.calls != 1 {
		t.Fatal("foreign completion accepted", err)
	}
	row := p.remote[d]
	row.Records[0].Owner.IntegrationUID = g.Ledger.Owner.UID
	p.remote[d] = row
	result, err = h.sync(ctx, j, f, g, "")
	if err != nil || result.Pending != 0 || p.calls != 1 {
		t.Fatal("recovery replayed provider write", result, err)
	}
	l, err = h.opts.Ledger.ReadLedger(ctx, intent.Spec.Record)
	if err != nil || l.Spec.Pending != nil || l.Spec.ContentHash != intent.Spec.AfterHash {
		t.Fatal("fresh facts not confirmed", l, err)
	}
}
func TestObsoleteBundleDoesNotPublishSynced(t *testing.T) {
	h, j, p, _, now := engineFixture(t)
	p.apply = func(_ context.Context, o *kinds.IntegrationOperation) (kinds.Receipt, error) {
		h.mu.Lock()
		j.frame.input.BundleID = "next"
		h.mu.Unlock()
		return confirmedReceipt(o, *now), nil
	}
	a := h.execute(context.Background(), j, command{kind: "sync"})
	if !errors.Is(a.err, ErrFenced) {
		t.Fatal(a.err)
	}
	if s := h.Statuses(); len(s) > 0 && (!s[0].LastSync.IsZero() || s[0].Phase == "Ready") {
		t.Fatal("historical completion claimed current sync", s)
	}
}
func TestWithLeasePinsExactGuardAndPropagatesLoss(t *testing.T) {
	h, j, _, _, _ := engineFixture(t)
	called := false
	old := j.guard
	old.LeaseID = "obsolete"
	if err := h.WithLease(context.Background(), old, func(context.Context) error { called = true; return nil }); !errors.Is(err, ErrFenced) || called {
		t.Fatal(err)
	}
	if err := h.WithLease(context.Background(), j.guard, func(ctx context.Context) error {
		j.cancel()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
			t.Fatal("lease loss not propagated")
			return nil
		}
	}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type fakeRecovery struct {
	*fakePublisher
	recover    func(context.Context, *kinds.IntegrationOperation, RemoteSnapshot) (kinds.Receipt, error)
	recoveries int
}

func (p *fakeRecovery) Recover(ctx context.Context, o *kinds.IntegrationOperation, s RemoteSnapshot) (kinds.Receipt, error) {
	p.recoveries++
	return p.recover(ctx, o, s)
}
func TestPartialRecoveryRequiresCurrentDesiredConfigAndValidCompleteIDs(t *testing.T) {
	h, j, p, _, now := engineFixture(t)
	ctx := context.Background()
	f, g, _ := h.captured(j)
	d := p.pubs[0].Destination
	var intent *kinds.IntegrationOperation
	p.apply = func(_ context.Context, o *kinds.IntegrationOperation) (kinds.Receipt, error) {
		intent = o
		return kinds.Receipt{ObservedAt: *now, Targets: []kinds.TargetResult{{Target: o.Spec.After.Targets[0], ProviderID: "a", Applied: true}}}, nil
	}
	if _, err := h.sync(ctx, j, f, g, ""); !errors.Is(err, ledger.ErrPending) {
		t.Fatal(err)
	}
	*now = now.Add(time.Second)
	partial := *intent.Spec.After.DeepCopy()
	partial.Targets = partial.Targets[:1]
	p.remote[d] = RemoteSnapshot{Complete: true, Records: []plan.RemoteRecord{{Endpoint: partial, IDs: []string{"a"}, Owner: plan.RemoteOwner{ClusterID: g.Ledger.Owner.ClusterID, IntegrationUID: g.Ledger.Owner.UID}}}}
	recovery := &fakeRecovery{fakePublisher: p, recover: func(_ context.Context, o *kinds.IntegrationOperation, s RemoteSnapshot) (kinds.Receipt, error) {
		if !s.Complete || len(s.Records) != 1 || s.Records[0].IDs[0] != "a" {
			t.Fatal("resume without fresh observed IDs")
		}
		p.remote[d] = RemoteSnapshot{Complete: true, Records: []plan.RemoteRecord{{Endpoint: *o.Spec.After.DeepCopy(), IDs: []string{"a", "b"}, Owner: plan.RemoteOwner{ClusterID: g.Ledger.Owner.ClusterID, IntegrationUID: g.Ledger.Owner.UID}}}}
		return confirmedReceipt(o, *now), nil
	}}
	j.provider = recovery
	altered := f
	altered.input.Sources = sources()
	altered.input.Sources[0].Records.Spec.Endpoints[0].Targets = []string{"192.0.2.99"}
	if _, err := h.sync(ctx, j, altered, g, ""); !errors.Is(err, ledger.ErrPending) || recovery.recoveries != 0 {
		t.Fatal("changed desired resumed historical intent", err)
	}
	j.guard.ConfigDigest = kinds.Digest([]byte("next"))
	_, changed, _ := h.captured(j)
	if _, err := h.sync(ctx, j, f, changed, ""); !errors.Is(err, ledger.ErrPending) || recovery.recoveries != 0 {
		t.Fatal("changed config resumed historical intent", err)
	}
	j.guard = g.Ledger
	row := p.remote[d]
	row.Records[0].IDs = []string{"a", "a"}
	p.remote[d] = row
	if _, err := h.sync(ctx, j, f, g, ""); !errors.Is(err, plan.ErrInvalid) || recovery.recoveries != 0 {
		t.Fatal("malformed observation used for recovery", err)
	}
	row.Records[0].IDs = []string{"a"}
	p.remote[d] = row
	result, err := h.sync(ctx, j, f, g, "")
	if err != nil || result.Pending != 0 || recovery.recoveries != 1 || p.calls != 1 {
		t.Fatal("owned partial recovery did not complete safely", result, err)
	}
}
