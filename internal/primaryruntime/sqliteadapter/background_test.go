package sqliteadapter

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/sqlite"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

func TestBackgroundRequiresGuardAndExactCapturedWriterEvenNoop(t *testing.T) {
	f := setup(t)
	ctx := f.ctx()
	l := f.active("primary", "example.")
	v, err := f.repo.Read(ctx, l.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	c := pr.Command{Lifecycle: l, Expected: v.Expected, Reason: "notify-delivery"}
	before := snapshot(t, f.s)
	for _, bad := range []context.Context{context.Background(), pr.WithPublicationFence(ctx, pr.PublicationFence{Term: f.fence.Term, Incarnation: f.fence.Incarnation + 1})} {
		if _, err := f.a.Admission(bad, c); !errors.Is(err, pr.ErrUnavailable) {
			t.Fatalf("no-op fence: %v", err)
		}
		next := l.DeepCopy()
		next.Spec.Sequence++
		if _, err := f.a.LifecycleCAS(bad, l, next); !errors.Is(err, pr.ErrUnavailable) {
			t.Fatalf("lifecycle fence: %v", err)
		}
	}
	f.a.o.Background = nil
	if _, err := f.a.Admission(ctx, c); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatalf("nil no-op guard: %v", err)
	}
	next := l.DeepCopy()
	next.Spec.Sequence++
	if _, err := f.a.LifecycleCAS(ctx, l, next); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatalf("nil lifecycle guard: %v", err)
	}
	if !reflect.DeepEqual(before, snapshot(t, f.s)) {
		t.Fatal("denial changed durable image")
	}
}

func TestActualParentRemovalSurvivingChildReincarnatesWithoutHistoricalMutation(t *testing.T) {
	f := setup(t)
	ctx := f.ctx()
	parent := f.active("parent", "example.")
	childSource := f.zone("child", "child.example.")
	child := f.begin(childSource, parent)
	if _, err := f.r.Reconcile(ctx, child.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Delete(ctx, store.Key{Kind: "DNSZone", Name: "parent"}, store.DeleteOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	removed := parent.Spec.Source
	removed.Removed = true
	parent, err := f.r.RetireSource(ctx, parent.Spec.Zone, removed)
	if err != nil {
		t.Fatal(err)
	}
	historical, err := f.repo.Read(ctx, parent.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	if historical.Head.Spec.Lifecycle != zk.LifecycleTombstoned || !fence(historical, child.Spec.Zone.Apex) {
		t.Fatal("missing terminal head/reservation")
	}
	if _, err := f.r.Retire(ctx, child.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	retired, err := f.r.ReleaseParents(ctx, child.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	witness := retired.Spec.Parents[0]
	if !witness.Released || witness.CommitSequence != historical.Head.Spec.CommitSequence || witness.RootDigest != historical.Head.Spec.RootDigest {
		t.Fatal(witness)
	}
	after, err := f.repo.Read(ctx, parent.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(historical, after) {
		t.Fatal("terminal release mutated historical head or reservations")
	}
	replacement := f.intent(childSource)
	replacement.Spec.Incarnation = retired.Spec.Incarnation + 1
	replacement.Spec.Sequence = retired.Spec.Sequence + 1
	fresh, err := f.r.Transition(ctx, retired, replacement)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Reconcile(ctx, fresh.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	if m, ok := f.r.Capture(time.Now()).Member(childSource.Spec.ZoneName); !ok || !m.Published || m.Incarnation != 2 || len(m.Parents) != 0 {
		t.Fatal(m)
	}
	after, err = f.repo.Read(ctx, parent.Spec.HeadZone())
	if err != nil || !reflect.DeepEqual(historical, after) {
		t.Fatal("replacement modified terminal parent", err)
	}
}

func TestNonprimaryActivateHasMembershipWithoutRRHead(t *testing.T) {
	f := setup(t)
	ctx := f.ctx()
	z := f.zone("secondary", "example.")
	z.Spec.Type = public.ZoneTypeForward
	z.Spec.Forward = &public.ForwardSpec{UpstreamGroup: "default"}
	saved, err := f.s.Apply(ctx, z, store.ApplyOptions{Manager: "api", ExpectedResourceVersion: z.ResourceVersion})
	if err != nil {
		t.Fatal(err)
	}
	l := f.begin(saved.(*public.DNSZone))
	if _, err := f.r.FenceParents(ctx, l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	active, err := f.r.Activate(ctx, l.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	if active.Spec.Phase != zk.PhaseActive {
		t.Fatal(active.Spec.Phase)
	}
	v, err := f.repo.Read(ctx, active.Spec.HeadZone())
	if !errors.Is(err, store.ErrNotFound) || v.Head != nil {
		t.Fatal("nonprimary acquired head", err)
	}
	m, ok := f.r.Capture(time.Now()).Member("example.")
	if !ok || !m.Published || m.Version != (zonestate.Version{}) {
		t.Fatal(m)
	}
}

// Explicit test-only provider publication. The RW lock covers Command's final
// commit/no-op fence, rather than merely its transaction callback.
type pinnedBackground struct {
	mu      sync.RWMutex
	revoked bool
	entered chan struct{}
}

func (g *pinnedBackground) WithCurrent(ctx context.Context, _ pr.PublicationFence, fn func(BackgroundPolicy) error) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.revoked {
		return pr.ErrUnavailable
	}
	if g.entered != nil {
		close(g.entered)
	}
	return fn(func(context.Context, *sqlite.CommandTx, BackgroundCheck) error { return ctx.Err() })
}

func TestBackgroundPinsProviderThroughActualBlockedWriterAndFinalCommit(t *testing.T) {
	f := setup(t)
	ctx := f.ctx()
	l := f.active("primary", "example.")
	guard := &pinnedBackground{entered: make(chan struct{})}
	f.a.o.Background = guard
	locked, release := make(chan struct{}), make(chan struct{})
	writer := make(chan error, 1)
	go func() {
		writer <- f.s.Command(ctx, f.fence.Term, func(context.Context, *sqlite.CommandTx) error { close(locked); <-release; return nil })
	}()
	<-locked
	next := l.DeepCopy()
	next.Spec.Sequence++
	result := make(chan error, 1)
	go func() { _, err := f.a.LifecycleCAS(ctx, l, next); result <- err }()
	<-guard.entered
	rotated := make(chan struct{})
	go func() { guard.mu.Lock(); guard.revoked = true; guard.mu.Unlock(); close(rotated) }()
	select {
	case <-rotated:
		close(release)
		t.Fatal("provider rotated before final Store.Command fence")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	<-rotated
	actual, err := f.r.ReadLifecycle(ctx, l.Spec.Zone)
	if err != nil || actual.Spec.Sequence != next.Spec.Sequence {
		t.Fatal(actual, err)
	}
	v, err := f.repo.Read(ctx, l.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, f.s)
	if _, err := f.a.Admission(ctx, pr.Command{Lifecycle: actual, Expected: v.Expected, Reason: "notify-delivery"}); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("revoked no-op admitted", err)
	}
	if !reflect.DeepEqual(before, snapshot(t, f.s)) {
		t.Fatal("revoked no-op mutated head")
	}
}

func TestBackgroundDeadlineWhileBlockedInActualWriterCannotMutateLifecycle(t *testing.T) {
	f := setup(t)
	ctx := f.ctx()
	l := f.active("primary", "example.")
	before := snapshot(t, f.s)
	guard := &pinnedBackground{entered: make(chan struct{})}
	f.a.o.Background = guard
	locked, release := make(chan struct{}), make(chan struct{})
	writer := make(chan error, 1)
	go func() {
		writer <- f.s.Command(ctx, f.fence.Term, func(context.Context, *sqlite.CommandTx) error { close(locked); <-release; return nil })
	}()
	<-locked
	deadline, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	next := l.DeepCopy()
	next.Spec.Sequence++
	result := make(chan error, 1)
	go func() { _, err := f.a.LifecycleCAS(deadline, l, next); result <- err }()
	<-guard.entered
	<-deadline.Done()
	close(release)
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, snapshot(t, f.s)) {
		t.Fatal("expired background command mutated lifecycle/head/audit")
	}
}

func TestReleasedWitnessRequiresExactTerminalProofAndLeavesActiveReservation(t *testing.T) {
	f := setup(t)
	ctx := f.ctx()
	parent := f.active("parent", "example.")
	child := f.begin(f.zone("child", "child.example."), parent)
	if _, err := f.r.Reconcile(ctx, child.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	retired, err := f.r.Retire(ctx, child.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	head, err := f.repo.Read(ctx, parent.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	forged := retired.DeepCopy()
	forged.Spec.Sequence++
	forged.Spec.Parents[0].Released = true
	forged.Spec.Parents[0].CommitSequence = head.Head.Spec.CommitSequence
	forged.Spec.Parents[0].RootDigest = head.Head.Spec.RootDigest
	if _, err := f.a.LifecycleCAS(ctx, retired, forged); !errors.Is(err, pr.ErrConflict) {
		t.Fatal("released active reserved boundary", err)
	}
	retiring := parent.DeepCopy()
	retiring.Spec.Sequence++
	retiring.Spec.Phase = zk.PhaseRetiring
	parent, err = f.r.Transition(ctx, parent, retiring)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := zonestate.Prepare(head.Aggregate, head.Head, zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	prepared.Head.Spec.Lifecycle = zk.LifecycleTombstoned
	terminal, err := f.a.Admission(ctx, pr.Command{Lifecycle: parent, Expected: head.Expected, Prepared: prepared, Reason: "tombstone"})
	if err != nil {
		t.Fatal(err)
	}
	forged.Spec.Parents[0].CommitSequence = terminal.Head.Spec.CommitSequence
	forged.Spec.Parents[0].RootDigest = terminal.Head.Spec.RootDigest
	if _, err := f.a.LifecycleCAS(ctx, retired, forged); !errors.Is(err, pr.ErrConflict) {
		t.Fatal("tombstone without retired parent used as terminal proof", err)
	}
	after, err := f.repo.Read(ctx, parent.Spec.HeadZone())
	if err != nil || !reflect.DeepEqual(terminal, after) {
		t.Fatal("denied release mutated historical reservation", err)
	}
}

func TestUnwitnessedReleasedZeroCannotBypassParentProof(t *testing.T) {
	f := setup(t)
	ctx := f.ctx()
	parent := f.active("parent", "example.")
	child := f.begin(f.zone("child", "child.example."), parent)
	retired, err := f.r.Retire(ctx, child.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	next := retired.DeepCopy()
	next.Spec.Sequence++
	next.Spec.Parents[0].Released = true
	if next.Spec.Parents[0].CommitSequence != 0 {
		t.Fatal("fixture already witnessed")
	}
	if _, err := f.a.LifecycleCAS(ctx, retired, next); !errors.Is(err, pr.ErrConflict) {
		t.Fatal("zero released witness bypassed proof", err)
	}
}
