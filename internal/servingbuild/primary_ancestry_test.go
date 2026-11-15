package servingbuild

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/driver"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/sqliteadapter"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ancestryGate struct {
	entered chan struct{}
	release chan struct{}
	used    atomic.Bool
}

func newAncestryGate() *ancestryGate {
	return &ancestryGate{entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *ancestryGate) wait(ctx context.Context) error {
	if !g.used.CompareAndSwap(false, true) {
		return nil
	}
	close(g.entered)
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// All guards/adapters are real. Optional scheduling gates run before entering
// the actual LifecycleCAS, never while holding publication/provider/SQLite pins.
func ancestryRuntime(t *testing.T, h *primaryFeedbackFixture, witness, activate *ancestryGate) *pr.Runtime {
	t.Helper()
	guard, err := currentguard.New(currentguard.Options{Pin: h.f.keyrings.PrimaryPin(h.f.pub)})
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := sqliteadapter.New(sqliteadapter.Options{Store: h.f.store, Background: guard.Background(), Credentials: guard.Credentials()})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := h.f.store.ZoneStateBackend(zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	cas := func(ctx context.Context, current, next *zk.ZoneLifecycle) (*zk.ZoneLifecycle, error) {
		if current != nil && next.Spec.Zone.ObjectName == "uncommitted" {
			if witness != nil && current.Spec.Phase == zk.PhasePreparing && next.Spec.Phase == zk.PhasePreparing && len(current.Spec.Parents) == 0 && len(next.Spec.Parents) == 1 {
				if err := witness.wait(ctx); err != nil {
					return nil, err
				}
			}
			if activate != nil && next.Spec.Phase == zk.PhaseActive {
				if err := activate.wait(ctx); err != nil {
					return nil, err
				}
			}
		}
		return adapter.LifecycleCAS(ctx, current, next)
	}
	runtime, err := pr.New(pr.Options{Store: h.f.store, Repository: zonestate.NewRepository(backend, zonestate.Limits{}), Reader: primaryzone.NewReader(h.f.store, primaryzone.ReaderOptions{Policy: guard.CapturePolicy}), LifecycleCAS: cas, Admit: adapter.Admission, Publish: h.publish.Publish})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func ancestryDesiredParent(t *testing.T, h *primaryFeedbackFixture) {
	t.Helper()
	expiry := h.f.now.Truncate(time.Second).Add(time.Hour)
	h.f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := tx.Apply(ctx, &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "parent"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "example.", Nameservers: []string{"ns.example."}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		if err != nil {
			return err
		}
		_, err = tx.Apply(ctx, &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "parent-expiry", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "parent"}, Endpoints: []public.Endpoint{{DNSName: "lease.example.", RecordType: "A", Targets: []string{"192.0.2.1"}, Kudurru: &public.EndpointExt{ExpiresAt: &metav1.Time{Time: expiry}}}}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
}

func ancestryLease(t *testing.T, h *primaryFeedbackFixture) (*serving.Lease, *RuntimeOwner) {
	t.Helper()
	h.f.waitCurrent(t)
	lease, err := h.f.pub.CaptureHTTP(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := lease.Bundle().Component("runtime")
	return lease, value.(*RuntimeOwner)
}

func ancestrySource(t *testing.T, h *primaryFeedbackFixture, lease *serving.Lease, owner *RuntimeOwner, name string) pr.PrimaryStatusObservation {
	t.Helper()
	observation, ok := owner.Primary().CommittedPrimaryStatus(name)
	if !ok {
		t.Fatal("missing actual lifecycle", name)
	}
	value, _ := lease.Bundle().Component("objects")
	object, ok := value.(*zonecache.Snapshot).Objects.Get(store.Key{Kind: "DNSZone", Name: name})
	if !ok {
		t.Fatal("missing actual source", name)
	}
	desired := object.(*public.DNSZone)
	token, err := primaryzone.SourceToken(desired)
	if err != nil || observation.Source.UID != token.UID || observation.Source.ResourceVersion != token.ResourceVersion || observation.Source.IntentDigest != token.IntentDigest || observation.Source.Removed || observation.Member.Zone.UID != string(desired.UID) || observation.Member.Zone.Apex != desired.Spec.ZoneName {
		t.Fatal("lifecycle borrowed a different source token", observation, token, err)
	}
	if observation.HeadResourceVersion != "" && (!observation.ConfigMatchesDesired || observation.CommittedSource.UID != token.UID || observation.CommittedSource.IntentDigest != token.IntentDigest) {
		t.Fatal("head claimed a different frozen config/receipt", observation)
	}
	route := owner.Zones().Zones(h.f.now).Match(desired.Spec.ZoneName)
	admitted := owner.Primary().Admitted(desired.Spec.ZoneName, h.f.now)
	if route == nil || route.Name() != desired.Spec.ZoneName || route.RoutingAvailableAt(h.f.now) != admitted {
		t.Fatal("primary admission and actual route diverged", name, admitted)
	}
	return observation
}

func ancestryUnavailable(t *testing.T, h *primaryFeedbackFixture, lease *serving.Lease, owner *RuntimeOwner, apex string) {
	t.Helper()
	if owner.Primary().Admitted(apex, h.f.now) {
		t.Fatal("unfenced/pending primary admitted", apex)
	}
	response := query(t, h.f.chain, lease, apex)
	if response.Msg.Rcode != dns.RcodeServerFailure || len(response.Msg.Answer) != 0 {
		t.Fatal("DNS served a primary before its barriers", response.Msg)
	}
}

func ancestryRun(t *testing.T, h *primaryFeedbackFixture, runtime *pr.Runtime) *driver.Driver {
	t.Helper()
	runner, err := driver.New(driver.Options{Store: h.f.store, Runtime: runtime, Publication: h.publish, Image: pr.ImageOptions{ClusterID: h.f.store.Info().ClusterID, Scheme: h.f.builder.opts.Schema.scheme, Validation: h.f.builder.opts.Schema.validation(h.ctx)}, PollInterval: 5 * time.Millisecond, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx, h.writer) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("ancestry driver did not join")
		}
	})
	return runner
}

func ancestryGateReached(t *testing.T, h *primaryFeedbackFixture, g *ancestryGate, runner *driver.Driver) {
	t.Helper()
	select {
	case <-g.entered:
	case <-h.ctx.Done():
		t.Fatal("ancestry barrier not reached", runner.Status(), h.ctx.Err())
	}
}

func TestPrimaryAncestryPreparingChildAdmitsNewParentBeforeFencing(t *testing.T) {
	h := newPrimaryFeedbackFixture(t) // Actual controller holds only status callbacks.
	ancestryDesiredParent(t, h)
	witness, active := newAncestryGate(), newAncestryGate()
	runtime := ancestryRuntime(t, h, witness, active)
	runner := ancestryRun(t, h, runtime)

	ancestryGateReached(t, h, witness, runner)
	lease, owner := ancestryLease(t, h)
	parent := ancestrySource(t, h, lease, owner, "parent")
	child := ancestrySource(t, h, lease, owner, "uncommitted")
	if parent.Member.Phase != zk.PhaseFencing || parent.Member.Version.CommitSequence != 2 || child.Member.Phase != zk.PhasePreparing || len(child.Member.Parents) != 0 || child.HeadResourceVersion != "" {
		t.Fatal("unexpected checkpoint before actual witness CAS", parent, child)
	}
	ancestryUnavailable(t, h, lease, owner, "example.")
	ancestryUnavailable(t, h, lease, owner, "uncommitted.example.")
	lease.Release()
	close(witness.release)

	ancestryGateReached(t, h, active, runner)
	lease, owner = ancestryLease(t, h)
	parent = ancestrySource(t, h, lease, owner, "parent")
	child = ancestrySource(t, h, lease, owner, "uncommitted")
	if child.Member.Phase != zk.PhaseFencing || child.Member.Version.CommitSequence != 1 || len(child.Member.Parents) != 1 {
		t.Fatal("child skipped head/witness preparation", child)
	}
	ancestryWitness(t, parent, child)
	ancestryUnavailable(t, h, lease, owner, "uncommitted.example.")
	lease.Release()
	close(active.release)
	ancestryWait(t, h, runner)
}

func ancestryWitness(t *testing.T, parent, child pr.PrimaryStatusObservation) {
	t.Helper()
	if len(child.Member.Parents) != 1 {
		t.Fatal("missing exact parent witness", child)
	}
	witness := child.Member.Parents[0]
	if witness.Zone != parent.HeadZone || witness.Incarnation != parent.Member.Incarnation || witness.CommitSequence != parent.Member.Version.CommitSequence || witness.RootDigest != parent.Member.Version.RootDigest || witness.BoundaryID == "" || witness.Released {
		t.Fatal("parent/head witness was substituted", parent, witness)
	}
}

func ancestryWait(t *testing.T, h *primaryFeedbackFixture, runner *driver.Driver) {
	t.Helper()
	for {
		changed := h.f.pub.Subscribe()
		lease, err := h.f.pub.CaptureHTTP(h.ctx)
		if err == nil {
			value, _ := lease.Bundle().Component("runtime")
			owner := value.(*RuntimeOwner)
			if owner.Primary().Admitted("example.", h.f.now) && owner.Primary().Admitted("uncommitted.example.", h.f.now) {
				parent := ancestrySource(t, h, lease, owner, "parent")
				child := ancestrySource(t, h, lease, owner, "uncommitted")
				ancestryWitness(t, parent, child)
				if parent.Member.Phase != zk.PhaseActive || child.Member.Phase != zk.PhaseActive || parent.Member.Version.CommitSequence != 2 || child.Member.Version.CommitSequence != 1 {
					t.Fatal("unexpected committed activation", parent, child)
				}
				response := query(t, h.f.chain, lease, "uncommitted.example.")
				if response.Msg.Rcode != dns.RcodeSuccess || !response.Msg.Authoritative {
					t.Fatal("published child is not authoritative", response.Msg)
				}
				parentMember, childMember := parent.Member, child.Member
				lease.Release()
				for _, member := range []pr.Member{parentMember, childMember} {
					if err := h.publish.Wait(h.ctx, h.writer, member); err != nil {
						t.Fatal("complete exact publication not acknowledged", err)
					}
				}
				return
			}
			lease.Release()
		}
		select {
		case <-changed:
		case <-h.ctx.Done():
			t.Fatal("primary ancestry failed to converge", runner.Status(), h.f.pub.Status(), h.ctx.Err())
		}
	}
}

func TestPrimaryAncestryRestartFromParentHeadOneChildPreparingCheckpoint(t *testing.T) {
	h := newPrimaryFeedbackFixture(t)
	ancestryDesiredParent(t, h)
	before := ancestryRuntime(t, h, nil, nil)
	parent, err := pr.NewLifecycle(h.writer.Term.ClusterID, h.read("parent"), nil)
	if err != nil {
		t.Fatal(err)
	}
	parent, err = before.Transition(h.ctx, nil, parent)
	if err != nil {
		t.Fatal(err)
	}
	next := parent.DeepCopy()
	next.Spec.Sequence++
	next.Spec.Phase = zk.PhaseFencing
	parent, err = before.Transition(h.ctx, parent, next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := before.Prepare(h.ctx, parent.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	lease, owner := ancestryLease(t, h)
	p := ancestrySource(t, h, lease, owner, "parent")
	c := ancestrySource(t, h, lease, owner, "uncommitted")
	if p.Member.Phase != zk.PhaseFencing || p.Member.Sequence != 2 || p.Member.Version.CommitSequence != 1 || c.Member.Phase != zk.PhasePreparing || c.Member.Sequence != 1 || len(c.Member.Parents) != 0 || c.HeadResourceVersion != "" {
		t.Fatal("not the exact pre-fix durable checkpoint", p, c)
	}
	ancestryUnavailable(t, h, lease, owner, "example.")
	ancestryUnavailable(t, h, lease, owner, "uncommitted.example.")
	lease.Release()

	// A new runtime has no independently installed members. Driver.Run restores
	// one actual complete image under PublishIfCurrent, then repairs ancestry.
	restarted := ancestryRuntime(t, h, nil, nil)
	runner := ancestryRun(t, h, restarted)
	ancestryWait(t, h, runner)
}
