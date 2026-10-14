package primaryruntime

import (
	"context"
	"reflect"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Use real expiring desired records, so a later source reconcile is a genuine
// no-op rather than a repair of manually changed aggregate expiry.
func expiryZone(t *testing.T, h *harness, name, apex string, deadline time.Time, parents ...*zk.ZoneLifecycle) *zk.ZoneLifecycle {
	t.Helper()
	z := h.zone(name, apex)
	if !deadline.IsZero() {
		rs := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: name + "-lease", Namespace: "apps"}, Spec: public.DNSRecordSetSpec{
			ZoneRef: public.ObjectRef{Name: z.Name}, Endpoints: []public.Endpoint{{DNSName: "lease." + apex, RecordType: "A", Targets: []string{"192.0.2.1"}, Kudurru: &public.EndpointExt{ExpiresAt: &metav1.Time{Time: deadline}}}},
		}}
		if _, err := h.st.Apply(t.Context(), rs, store.ApplyOptions{Manager: "api"}); err != nil {
			t.Fatal(err)
		}
	}
	l := h.begin(z, parents...)
	if _, err := h.r.Reconcile(t.Context(), l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	l, err := h.r.ReadLifecycle(t.Context(), l.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func expiryAssertCut(t *testing.T, c Cut, apex string, now time.Time, available bool) {
	t.Helper()
	model := c.Match("x." + apex)
	if model == nil || model.Name() != apex || model.AvailableAt(now) != available {
		t.Fatalf("cut availability for %s at %s: want %t, model %v", apex, now, available, model)
	}
	m, ok := c.Member(apex)
	if !ok || (!available && m.Published) {
		t.Fatal("unavailable member claims publication", m, ok)
	}
}

func expirySameMember(t *testing.T, a, b Member) {
	t.Helper()
	a.Published, b.Published = false, false
	if !reflect.DeepEqual(a, b) {
		t.Fatal("serving expiry changed durable membership", a, b)
	}
}

func expirySameImage(t *testing.T, a, b replication.Image) {
	t.Helper()
	// ReadGeneration stamps each export time; all durable payload, source/RV,
	// manifest, authority and generation evidence must otherwise be identical.
	a.CreatedAt, b.CreatedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("expiry preparation/lookup/no-op changed durable image")
	}
}

func TestCutEffectiveExpiryActualSQLiteMinimaAndAdmission(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hours [3]int
	}{
		{"parent-only", [3]int{1, 0, 0}},
		{"nearest-parent", [3]int{3, 2, 0}},
		{"own-earliest", [3]int{3, 2, 1}},
		{"remote-parent-earliest", [3]int{1, 2, 3}},
		{"unbounded", [3]int{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "sqlite")
			base := time.Now().UTC().Truncate(time.Second)
			var own [3]time.Time
			for i, hours := range tc.hours {
				if hours != 0 {
					own[i] = base.Add(time.Duration(hours) * time.Hour)
				}
			}
			root := expiryZone(t, h, "root", "example.", own[0])
			middle := expiryZone(t, h, "middle", "middle.example.", own[1], root)
			leaf := expiryZone(t, h, "leaf", "leaf.middle.example.", own[2], root, middle)
			z := h.zone("forward", "forward.middle.example.")
			z.Spec.Type = public.ZoneTypeForward
			saved, err := h.st.Apply(t.Context(), z, store.ApplyOptions{Manager: "api"})
			if err != nil {
				t.Fatal(err)
			}
			forward := h.begin(saved.(*public.DNSZone), root, middle)
			if _, err := h.r.FenceParents(t.Context(), forward.Spec.Zone); err != nil {
				t.Fatal(err)
			}
			if _, err := h.r.Activate(t.Context(), forward.Spec.Zone); err != nil {
				t.Fatal(err)
			}
			members := []*zk.ZoneLifecycle{root, middle, leaf}
			for _, l := range members {
				if err := h.r.Republish(t.Context(), l.Spec.Zone); err != nil {
					t.Fatal(err)
				}
			}
			before := readTestImage(t, h)
			p := prepareTestImage(t, before, imageTestOptions(t, h))
			var effective time.Time
			for i, l := range members {
				if !own[i].IsZero() && (effective.IsZero() || own[i].Before(effective)) {
					effective = own[i]
				}
				e := p.durable[l.Spec.Zone]
				o, _ := p.CommittedPrimaryStatus(l.Spec.Zone.ObjectName)
				if !e.expiry.Equal(own[i]) || !e.view.Aggregate.Expiry.Equal(own[i]) || !o.ExpiresAt.Equal(effective) {
					t.Fatal("effective expiry mutated raw evidence or diverged from status", own[i], effective, e.expiry, o)
				}
				at := effective
				if at.IsZero() {
					at = base.Add(100 * time.Hour)
				}
				for _, now := range []time.Time{at.Add(-time.Nanosecond), at, at.Add(time.Nanosecond)} {
					want := effective.IsZero() || now.Before(effective)
					if p.Admitted(l.Spec.Zone.Apex, now) != want {
						t.Fatal("image admission disagrees with absolute expiry", l.Spec.Zone, now, want)
					}
					expiryAssertCut(t, p.Cut(now), l.Spec.Zone.Apex, now, want)
					runtimeCut := h.r.Capture(now)
					expiryAssertCut(t, runtimeCut, l.Spec.Zone.Apex, now, want)
					m, _ := runtimeCut.Member(l.Spec.Zone.Apex)
					if m.Published != want {
						t.Fatal("runtime published diagnostic ignored inherited expiry", m)
					}
					durable, _ := p.DurableMember(l.Spec.Zone)
					expirySameMember(t, durable, m)
				}
				if !effective.IsZero() {
					opts := imageTestOptions(t, h)
					opts.Now = effective
					expired := prepareTestImage(t, before, opts)
					expiryAssertCut(t, expired.Cut(effective), l.Spec.Zone.Apex, effective, false)
					expiredStatus, _ := expired.CommittedPrimaryStatus(l.Spec.Zone.ObjectName)
					if expired.Admitted(l.Spec.Zone.Apex, effective) || !reflect.DeepEqual(o, expiredStatus) {
						t.Fatal("preparation after ancestor expiry served or changed raw status", expiredStatus)
					}
				}
			}
			parentDeadline := p.cut.deadlines[middle.Spec.Zone.Apex]
			if !p.cut.deadlines[forward.Spec.Zone.Apex].Equal(parentDeadline) || !p.Admitted(forward.Spec.Zone.Apex, base) || (!parentDeadline.IsZero() && p.Admitted(forward.Spec.Zone.Apex, parentDeadline)) {
				t.Fatal("nonprimary admission lost captured ancestor deadline")
			}
			expirySameImage(t, before, readTestImage(t, h))
		})
	}
}

func TestCutEffectiveExpiryHeldMapsAfterActualParentRefresh(t *testing.T) {
	h := newHarness(t, "sqlite")
	a := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	b := a.Add(time.Hour)
	parent := expiryZone(t, h, "parent", "example.", a)
	child := expiryZone(t, h, "child", "child.example.", time.Time{}, parent)
	p := statusImage(t, h)
	held := h.r.Capture(a)
	childBefore, err := h.repo.Read(t.Context(), child.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	var rs public.DNSRecordSet
	if err := h.st.Get(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "apps", Name: "parent-lease"}, &rs); err != nil {
		t.Fatal(err)
	}
	rs.Spec.Endpoints[0].Kudurru.ExpiresAt = &metav1.Time{Time: b}
	if _, err := h.st.Apply(t.Context(), &rs, store.ApplyOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.Reconcile(t.Context(), parent.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	fresh := statusImage(t, h)
	expiryAssertCut(t, held, child.Spec.Zone.Apex, a, false)
	expiryAssertCut(t, p.Cut(a), child.Spec.Zone.Apex, a, false)
	expiryAssertCut(t, fresh.Cut(a), child.Spec.Zone.Apex, a, true)
	expiryAssertCut(t, h.r.Capture(a), child.Spec.Zone.Apex, a, true)
	expiryAssertCut(t, fresh.Cut(b), child.Spec.Zone.Apex, b, false)
	expiryAssertCut(t, h.r.Capture(b), child.Spec.Zone.Apex, b, false)
	oldStatus, _ := p.CommittedPrimaryStatus("child")
	newStatus, _ := fresh.CommittedPrimaryStatus("child")
	if !held.deadlines[child.Spec.Zone.Apex].Equal(a) || !oldStatus.ExpiresAt.Equal(a) || !newStatus.ExpiresAt.Equal(b) || !fresh.durable[child.Spec.Zone].expiry.IsZero() {
		t.Fatal("held deadlines/status mutated or effective expiry replaced raw expiry")
	}
	childAfter, err := h.repo.Read(t.Context(), child.Spec.HeadZone())
	if err != nil || !reflect.DeepEqual(childBefore, childAfter) {
		t.Fatal("ancestor refresh changed child head", err)
	}
	expirySameMember(t, oldStatus.Member, newStatus.Member)
}

func TestCutEffectiveExpiryActualRestoreNoopAndRepublish(t *testing.T) {
	h := newHarness(t, "sqlite")
	ctx := t.Context()
	a := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	parent := expiryZone(t, h, "parent", "example.", a)
	middle := expiryZone(t, h, "middle", "middle.example.", a.Add(time.Hour), parent)
	child := expiryZone(t, h, "child", "child.middle.example.", time.Time{}, parent, middle)
	members := []*zk.ZoneLifecycle{parent, middle, child}
	st := h.st.(*sqlite.Store)
	image, token, err := st.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := prepareTestImage(t, *image, imageTestOptions(t, h))
	opts := h.r.o
	var acknowledged int
	// Explicit test-only exact acknowledgment. Production root owns the full
	// protected complete-lease/current-source publication adapter.
	opts.Publish = func(_ context.Context, cut Cut, m Member) error {
		expected, ok := p.DurableMember(m.Zone)
		if !ok {
			return ErrConflict
		}
		expirySameMember(t, expected, m)
		if !cut.deadlines[m.Zone.Apex].Equal(a) {
			return ErrConflict
		}
		acknowledged++
		return nil
	}
	restarted, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	restore, err := restarted.PrepareRestore(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PublishIfCurrent(ctx, token, sqlite.PublicationOptions{}, func() error { return restarted.InstallRestore(restore) }); err != nil {
		t.Fatal(err)
	}
	held := restarted.Capture(a)
	for _, l := range members {
		before := restarted.Capture(a.Add(-time.Nanosecond))
		expiryAssertCut(t, before, l.Spec.Zone.Apex, before.now, true)
		expiryAssertCut(t, held, l.Spec.Zone.Apex, a, false)
		if err := restarted.Republish(ctx, l.Spec.Zone); err != nil {
			t.Fatal("exact restored member failed publication", err)
		}
		if _, err := restarted.Reconcile(ctx, l.Spec.Zone); err != nil {
			t.Fatal("restored no-op source reconcile changed equality", err)
		}
		current := restarted.Capture(a.Add(-time.Nanosecond))
		expiryAssertCut(t, current, l.Spec.Zone.Apex, current.now, true)
		expiryAssertCut(t, restarted.Capture(a), l.Spec.Zone.Apex, a, false)
		m, _ := current.Member(l.Spec.Zone.Apex)
		if !m.Published {
			t.Fatal("restored exact member did not acknowledge", m)
		}
		heldMember, _ := held.Member(l.Spec.Zone.Apex)
		expirySameMember(t, heldMember, m)
		if heldMember.Published || !held.deadlines[l.Spec.Zone.Apex].Equal(a) {
			t.Fatal("post-restore rebuild mutated held cut")
		}
	}
	if acknowledged != 2*len(members) {
		t.Fatal("restore/no-op did not require exact acknowledgment", acknowledged)
	}
	expirySameImage(t, *image, readTestImage(t, h))
}

func TestCutEffectiveExpiryRestoreUsesDurableRosterBeforeReplacementOverlay(t *testing.T) {
	h := newHarness(t, "sqlite")
	ctx := t.Context()
	a := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	l := expiryZone(t, h, "primary", "example.", a)
	if err := h.st.Delete(ctx, store.Key{Kind: "DNSZone", Name: "primary"}, store.DeleteOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	removed := l.Spec.Source
	removed.Removed = true
	if _, err := h.r.RetireSource(ctx, l.Spec.Zone, removed); err != nil {
		t.Fatal(err)
	}
	h.zone("primary", "example.")
	p := statusImage(t, h)
	if !p.cut.deadlines[l.Spec.Zone.Apex].IsZero() || p.cut.entries[l.Spec.Zone.Apex].member.Incarnation != 0 || !p.durable[l.Spec.Zone].expiry.Equal(a) {
		t.Fatal("fixture did not retain raw retired expiry behind desired placeholder")
	}
	restarted, err := New(h.r.o)
	if err != nil {
		t.Fatal(err)
	}
	restore, err := restarted.PrepareRestore(p)
	if err != nil {
		t.Fatal(err)
	}
	if !restore.cut.deadlines[l.Spec.Zone.Apex].Equal(a) || restore.cut.ready[l.Spec.Zone.Apex] {
		t.Fatal("restore reused placeholder deadline or bypassed image readiness")
	}
	expected, _ := p.DurableMember(l.Spec.Zone)
	actual, _ := restore.cut.Member(l.Spec.Zone.Apex)
	expirySameMember(t, expected, actual)
}

func TestCutEffectiveExpiryDoesNotWaiveWitnessOrRawOwnExpiry(t *testing.T) {
	h := newHarness(t, "sqlite")
	a := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	parent := expiryZone(t, h, "parent", "example.", a)
	child := expiryZone(t, h, "child", "child.example.", time.Time{}, parent)
	image, opts := readTestImage(t, h), imageTestOptions(t, h)
	p := prepareTestImage(t, image, opts)
	cut := p.Cut(a)
	cut.deadlines = nil // Even an absent effective map cannot waive own expiry.
	expiryAssertCut(t, cut, parent.Spec.Zone.Apex, a, false)
	missing := rewriteTestImage(t, image, opts, func(obj store.Object) bool {
		if l, ok := obj.(*zk.ZoneLifecycle); ok && l.Spec.Zone == child.Spec.Zone {
			l.Spec.Parents = nil
		}
		return true
	})
	missingPart := prepareTestImage(t, missing, opts)
	expiryAssertCut(t, missingPart.Cut(opts.Now), child.Spec.Zone.Apex, opts.Now, false)
	if missingPart.Admitted(child.Spec.Zone.Apex, opts.Now) {
		t.Fatal("expiry map waived missing exact parent witness")
	}
	restore, err := h.r.PrepareRestore(missingPart)
	if err != nil {
		t.Fatal(err)
	}
	restore.cut.now = opts.Now
	expiryAssertCut(t, restore.cut, child.Spec.Zone.Apex, opts.Now, false)
}
