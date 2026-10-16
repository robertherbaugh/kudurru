package servingbuild

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/controller"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/driver"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type primaryFeedbackResult struct {
	wrote bool
	retry time.Duration
	err   error
}

type primaryFeedbackCall struct {
	zone   *public.DNSZone
	allow  chan bool
	result chan primaryFeedbackResult
}

// Gate only callback scheduling. The controller, status capability, driver,
// complete publisher, current guard, and SQLite transactions are real.
type primaryFeedbackGate struct {
	status *PrimaryStatus
	calls  chan primaryFeedbackCall
}

func (g *primaryFeedbackGate) Subscribe() <-chan struct{} { return g.status.Subscribe() }
func (g *primaryFeedbackGate) Reconcile(ctx context.Context, z *public.DNSZone) (bool, time.Duration, error) {
	call := primaryFeedbackCall{zone: z.DeepCopy(), allow: make(chan bool, 1), result: make(chan primaryFeedbackResult, 1)}
	select {
	case g.calls <- call:
	case <-ctx.Done():
		return false, 0, ctx.Err()
	}
	select {
	case allow := <-call.allow:
		if !allow {
			return false, time.Millisecond, nil
		}
	case <-ctx.Done():
		return false, 0, ctx.Err()
	}
	wrote, retry, err := g.status.Reconcile(ctx, z)
	call.result <- primaryFeedbackResult{wrote, retry, err}
	return wrote, retry, err
}

type primaryFeedbackFixture struct {
	t       *testing.T
	f       *fixture
	runtime *pr.Runtime
	publish *PrimaryPublication
	writer  pr.PublicationFence
	ctx     context.Context
	gate    *primaryFeedbackGate
	clock   atomic.Int64
}

func newPrimaryFeedbackFixture(t *testing.T) *primaryFeedbackFixture {
	t.Helper()
	keys, err := dnsserver.NewStaticKeyring()
	if err != nil {
		t.Fatal(err)
	}
	keyrings, err := NewKeyringOwner(keys)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixtureWithKeyrings(t, keyrings)
	runtime, publication, ctx := primaryFixture(t, f)
	original, err := f.store.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	writer, _ := pr.PublicationFenceFrom(ctx)
	ctx, cancel := context.WithTimeout(sqlite.WithWriterToken(ctx, original), 45*time.Second)
	_, _ = beginPrimary(ctx, t, f, runtime)
	h := &primaryFeedbackFixture{t: t, f: f, runtime: runtime, publish: publication, writer: writer, ctx: ctx}
	h.clock.Store(f.now.UnixNano())
	status, err := NewPrimaryStatus(PrimaryStatusOptions{Store: f.store, Publisher: f.pub, Pin: keyrings.PrimaryPin(f.pub), Now: h.now})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	h.gate = &primaryFeedbackGate{status: status, calls: make(chan primaryFeedbackCall, 1)}
	ctrl, err := controller.New(f.store, controller.Options{Workers: 1, ZoneCoalesce: -1, CommittedPrimaryStatus: h.gate, Now: func() time.Time { return f.now }})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- ctrl.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("controller did not join")
		}
	})
	return h
}

func (h *primaryFeedbackFixture) now() time.Time { return time.Unix(0, h.clock.Load()).UTC() }

func (h *primaryFeedbackFixture) read(name string) *public.DNSZone {
	h.t.Helper()
	var z public.DNSZone
	if err := h.f.store.Get(h.ctx, store.Key{Kind: "DNSZone", Name: name}, &z); err != nil {
		h.t.Fatal(err)
	}
	return &z
}

func (h *primaryFeedbackFixture) observe(name string, wantAvailable bool) (pr.PrimaryStatusObservation, *public.DNSZone) {
	h.t.Helper()
	h.f.waitCurrent(h.t)
	lease, err := h.f.pub.CaptureHTTP(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	defer lease.Release()
	value, _ := lease.Bundle().Component("runtime")
	owner := value.(*RuntimeOwner)
	o, ok := owner.Primary().CommittedPrimaryStatus(name)
	if !ok {
		h.t.Fatal("missing lifecycle", name)
	}
	value, _ = lease.Bundle().Component("objects")
	object, ok := value.(*zonecache.Snapshot).Objects.Get(store.Key{Kind: "DNSZone", Name: name})
	if !ok {
		h.t.Fatal("missing desired primary", name)
	}
	z := object.(*public.DNSZone)
	admitted := owner.Primary().Admitted(z.Spec.ZoneName, h.now())
	route := owner.Zones().Zones(h.now()).Match(z.Spec.ZoneName)
	if admitted != wantAvailable || route == nil || route.Name() != z.Spec.ZoneName || route.RoutingAvailableAt(h.now()) != wantAvailable {
		h.t.Fatalf("unexpected admission/route for %s: admitted=%t want=%t", name, admitted, wantAvailable)
	}
	h.t.Logf("%s publicRV=%s sourceRV=%s lifecycle=%d head=%d config=%t admitted=%t", name, z.ResourceVersion, o.Source.ResourceVersion, o.Member.Sequence, o.Member.Version.CommitSequence, o.ConfigMatchesDesired, admitted)
	return o, z
}

func (h *primaryFeedbackFixture) project(name string) primaryFeedbackResult {
	h.t.Helper()
	for {
		var call primaryFeedbackCall
		select {
		case call = <-h.gate.calls:
		case <-h.ctx.Done():
			h.t.Fatal("no current controller callback", h.ctx.Err())
		}
		if call.zone.Name != name || call.zone.ResourceVersion != h.read(name).ResourceVersion {
			call.allow <- false
			continue
		}
		h.f.waitCurrent(h.t)
		call.allow <- true
		select {
		case result := <-call.result:
			if result.err != nil {
				h.t.Fatal("projection failed", result.err)
			}
			return result
		case <-h.ctx.Done():
			h.t.Fatal("projection did not return", h.ctx.Err())
		}
	}
}

func (h *primaryFeedbackFixture) writeCondition(name, want, reason string) *public.DNSZone {
	h.t.Helper()
	for !h.project(name).wrote {
		if h.ctx.Err() != nil {
			h.t.Fatal("status write never completed", h.ctx.Err())
		}
	}
	z := h.read(name)
	ready := meta.FindStatusCondition(z.Status.Conditions, public.ConditionReady)
	if ready == nil || string(ready.Status) != want || ready.Reason != reason {
		h.t.Fatalf("unexpected Ready condition: %+v", ready)
	}
	return z
}

// Stop and join each actual driver pulse before releasing the next controller
// callback. This makes source/publication interleaving deterministic, without
// manually changing lifecycle receipts or replacing any admission adapter.
func (h *primaryFeedbackFixture) align(name string, accept ...func(pr.PrimaryStatusObservation) bool) {
	h.t.Helper()
	d, err := driver.New(driver.Options{Store: h.f.store, Runtime: h.runtime, Publication: h.publish, Image: pr.ImageOptions{ClusterID: h.f.store.Info().ClusterID, Scheme: h.f.builder.opts.Schema.scheme, Validation: h.f.builder.opts.Schema.validation(h.ctx)}, PollInterval: 5 * time.Millisecond, Timeout: 2 * time.Second})
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, h.writer) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				h.t.Error("driver stopped", err)
			}
		case <-time.After(5 * time.Second):
			h.t.Error("driver did not join")
		}
	}()
	for {
		changed := h.f.pub.Subscribe()
		lease, err := h.f.pub.CaptureHTTP(h.ctx)
		if err == nil {
			value, _ := lease.Bundle().Component("runtime")
			owner := value.(*RuntimeOwner)
			available := owner.Primary().Admitted(h.read(name).Spec.ZoneName, h.f.now)
			if available && len(accept) != 0 {
				o, found := owner.Primary().CommittedPrimaryStatus(name)
				available = found && accept[0](o)
			}
			lease.Release()
			if available {
				return
			}
		}
		select {
		case <-changed:
		case <-h.ctx.Done():
			h.t.Fatalf("driver failed to align: %+v publisher=%+v", d.Status(), h.f.pub.Status())
		}
	}
}

func TestPrimaryStatusSourceRVActualControllerDriverWaitAndQuiescence(t *testing.T) {
	h := newPrimaryFeedbackFixture(t)
	h.align("uncommitted")
	before, _ := h.observe("uncommitted", true)
	written := h.writeCondition("uncommitted", "True", public.ReasonServing)
	gap, _ := h.observe("uncommitted", false)
	if !gap.ConfigMatchesDesired || gap.Source.ResourceVersion == written.ResourceVersion {
		t.Fatal("fixture did not expose the status-only source gap")
	}
	for range 3 {
		result := h.project("uncommitted")
		if result.wrote || result.retry != primaryStatusRetry || !equality.Semantic.DeepEqual(written, h.read("uncommitted")) {
			t.Fatal("source-lag projection produced another status RV", result)
		}
	}
	h.align("uncommitted")
	after, current := h.observe("uncommitted", true)
	if after.Source.ResourceVersion != current.ResourceVersion || after.Member.Version != before.Member.Version || !equality.Semantic.DeepEqual(written, current) {
		t.Fatal("source alignment changed public status or durable head")
	}
	for range 3 {
		result := h.project("uncommitted")
		if result.wrote || result.retry != 0 || !equality.Semantic.DeepEqual(written, h.read("uncommitted")) {
			t.Fatal("aligned status did not become quiescent", result)
		}
	}
	final, _ := h.observe("uncommitted", true)
	if !reflect.DeepEqual(after, final) {
		t.Fatal("quiescent controller changed exact lifecycle/head evidence")
	}
}

func TestPrimaryStatusSourceRVConfigGapAndUIDReplacementRemainUnavailable(t *testing.T) {
	h := newPrimaryFeedbackFixture(t)
	h.align("uncommitted")
	h.writeCondition("uncommitted", "True", public.ReasonServing)
	h.align("uncommitted")
	old, _ := h.observe("uncommitted", true)
	z := h.read("uncommitted")
	z.Spec.Nameservers = []string{"changed." + z.Spec.ZoneName}
	if _, err := h.f.store.Apply(h.ctx, z, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	gap, _ := h.observe(z.Name, false)
	if gap.ConfigMatchesDesired {
		t.Fatal("configuration gap attested to the old head")
	}
	// Source CAS precedes head compilation. Alignment alone cannot permit
	// serving or defer the genuine frozen-config mismatch back to historical
	// Ready=true status.
	lifecycle, err := h.runtime.ReadLifecycle(h.ctx, old.Member.Zone)
	if err != nil {
		t.Fatal(err)
	}
	token, err := primaryzone.SourceToken(h.read(z.Name))
	if err != nil {
		t.Fatal(err)
	}
	next := lifecycle.DeepCopy()
	next.Spec.Sequence++
	next.Spec.Source = zk.LifecycleSource{UID: token.UID, ResourceVersion: token.ResourceVersion, IntentDigest: token.IntentDigest}
	if _, err := h.runtime.Transition(h.ctx, lifecycle, next); err != nil {
		t.Fatal(err)
	}
	alignedGap, desired := h.observe(z.Name, false)
	if alignedGap.ConfigMatchesDesired || alignedGap.Source.ResourceVersion != desired.ResourceVersion {
		t.Fatal("fixture did not retain an aligned source with an old head")
	}
	unavailable := h.writeCondition(z.Name, "False", "AwaitingCommittedPrimary")
	if unavailable.Status.Serial != 0 || unavailable.Status.ContentHash != "" || unavailable.Status.RecordCount != 0 {
		t.Fatal("configuration gap claimed committed data", unavailable.Status)
	}
	h.align(z.Name)
	repaired, _ := h.observe(z.Name, true)
	if !repaired.ConfigMatchesDesired || repaired.Member.Version == old.Member.Version {
		t.Fatal("actual driver did not commit the changed configuration")
	}
	h.writeCondition(z.Name, "True", public.ReasonServing)
	h.align(z.Name)
	z = h.read(z.Name)
	if err := h.f.store.Command(h.ctx, h.writer.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := tx.Delete(ctx, store.KeyOf(z), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		next := z.DeepCopy()
		next.UID, next.ResourceVersion, next.Generation = "", "", 0
		_, err := tx.Apply(ctx, next, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	replacement, current := h.observe(z.Name, false)
	if current.UID == z.UID || replacement.ConfigMatchesDesired || replacement.Member.Zone.UID == string(current.UID) {
		t.Fatal("replacement borrowed the former UID/head")
	}
	h.writeCondition(z.Name, "False", "AwaitingCommittedPrimary")
	h.align(z.Name)
	restored, desired := h.observe(z.Name, true)
	if restored.Member.Zone.UID != string(desired.UID) || restored.Member.Incarnation <= old.Member.Incarnation || !restored.ConfigMatchesDesired {
		t.Fatal("actual replacement lifecycle did not become ready")
	}
}

func TestPrimaryStatusSourceRVAlignedParentExpiryRemainsUnavailable(t *testing.T) {
	h := newPrimaryFeedbackFixture(t)
	// Begin with a published child, then admit a late parent. This isolates
	// status/expiry from simultaneous initial parent/child fencing.
	h.align("uncommitted")
	expiry := h.f.now.Truncate(time.Second).Add(time.Hour)
	if _, err := h.f.store.Apply(h.ctx, &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "parent"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "example.", Nameservers: []string{"ns.example."}}}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	h.align("uncommitted")
	h.f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := tx.Apply(ctx, &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "parent-expiry", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "parent"}, Endpoints: []public.Endpoint{{DNSName: "lease.example.", RecordType: "A", Targets: []string{"192.0.2.1"}, Kudurru: &public.EndpointExt{ExpiresAt: &metav1.Time{Time: expiry}}}}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	h.align("parent", func(o pr.PrimaryStatusObservation) bool { return o.ExpiresAt.Equal(expiry) })
	h.writeCondition("uncommitted", "True", public.ReasonServing)
	h.align("uncommitted")
	before, _ := h.observe("uncommitted", true)
	if !before.ConfigMatchesDesired || !before.ExpiresAt.Equal(expiry) || len(before.Member.Parents) != 1 || before.Member.Phase != zk.PhaseActive {
		t.Fatal("missing inherited parent expiry/witness", before)
	}
	h.clock.Store(expiry.UnixNano())
	h.observe("uncommitted", false)
	z := h.writeCondition("uncommitted", "False", "CommittedPrimaryUnavailable")
	if z.Status.Serial != before.Serial || z.Status.ContentHash != before.ContentHash || z.Status.RecordCount != before.RecordCount {
		t.Fatal("expiry erased truthful durable diagnostics")
	}
	// The false condition itself needs source alignment too. This cannot make
	// expired DNS data available, even when the old head/config remains exact.
	h.align("uncommitted")
	after, desired := h.observe("uncommitted", false)
	if after.Source.ResourceVersion != desired.ResourceVersion || after.Member.Version != before.Member.Version {
		t.Fatal("expiry status alignment changed the durable head")
	}
	if result := h.project("uncommitted"); result.wrote || result.retry != primaryStatusRetry {
		t.Fatal("aligned expired parent churned or skipped its bounded recheck", result)
	}
}
