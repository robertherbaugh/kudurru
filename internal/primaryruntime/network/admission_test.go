package network_test

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	network "github.com/robertherbaugh/kudurru/internal/primaryruntime/network"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zone"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNotifyBindRequiresCompleteImmutableCutAndReleasesRejectedOwnership(t *testing.T) {
	f := newFixture(t, public.ZonePolicy{})
	f.change()
	cut := f.r.Capture(time.Now())
	m, _ := cut.Member(f.zone.Spec.ZoneName)
	z := cut.Match(m.Zone.Apex)
	for _, mode := range []string{"nil-keys", "empty-identity", "nil-current", "expired-current", "capture-error"} {
		t.Run(mode, func(t *testing.T) {
			var released atomic.Int64
			s, err := network.NewNotify(network.NotifyOptions{Capture: func(context.Context, pr.PublicationFence, pr.Member) (network.NotifyCut, error) {
				cut := network.NotifyCut{Identity: "test", Keys: f.keys.Capture().Keys(), Current: func() bool { return true }, Release: func() { released.Add(1) }}
				switch mode {
				case "nil-keys":
					cut.Keys = nil
				case "empty-identity":
					cut.Identity = ""
				case "nil-current":
					cut.Current = nil
				case "expired-current":
					cut.Current = func() bool { return false }
				case "capture-error":
					return cut, pr.ErrUnavailable
				}
				return cut, nil
			}})
			check(t, err)
			ctx, release, err := s.Bind(f.ctx(), m, z)
			if !errors.Is(err, pr.ErrUnavailable) || ctx != nil || release != nil || released.Load() != 1 {
				t.Fatal("bad cut admitted or leaked", err, released.Load())
			}
		})
	}
	s, err := network.NewNotify(network.NotifyOptions{})
	check(t, err)
	if _, _, err := s.Bind(f.ctx(), m, z); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("nil capture admitted", err)
	}
}

func TestActualNotifyAncestorExpiryInvalidatesBoundChildWithoutNewGeneration(t *testing.T) {
	p := peer(t, true)
	f := newFixture(t, public.ZonePolicy{})
	parent, err := f.r.ReadLifecycle(f.ctx(), f.l.Spec.Zone)
	check(t, err)
	object, err := f.s.Apply(f.ctx(), &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "child"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "child.example.", Nameservers: []string{"ns.child.example."}, Policy: notifyPolicy(p.target, true)}}, store.ApplyOptions{Manager: store.ManagerSystem})
	check(t, err)
	child, err := pr.NewLifecycle(f.fence.Term.ClusterID, object.(*public.DNSZone), nil)
	check(t, err)
	child.Spec.Parents = append(child.Spec.Parents, pr.ParentObligation(parent, child))
	child, err = f.r.Transition(f.ctx(), nil, child)
	check(t, err)
	child, err = f.r.AdmitAncestor(f.ctx(), parent.Spec.Zone, child.Spec.Zone)
	check(t, err)
	_, err = f.r.Reconcile(f.ctx(), child.Spec.Zone)
	check(t, err)
	child, err = f.r.ReadLifecycle(f.ctx(), child.Spec.Zone)
	check(t, err)
	deadline := time.Now().Truncate(time.Second).Add(3 * time.Second)
	expires := metav1.NewTime(deadline)
	_, err = f.s.Apply(f.ctx(), &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "parent-expiry", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: f.zone.Name}, Endpoints: []public.Endpoint{{DNSName: "expiring.example.", RecordType: "A", RecordTTL: 60, Targets: []string{"192.0.2.1"}, Kudurru: &public.EndpointExt{ExpiresAt: &expires}}}}}, store.ApplyOptions{Manager: store.ManagerSystem})
	check(t, err)
	_, err = f.r.Reconcile(f.ctx(), f.l.Spec.Zone)
	check(t, err)
	check(t, f.r.NotifyOnce(f.ctx()))
	_, err = f.s.Apply(f.ctx(), &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "child-change", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "child"}, Endpoints: []public.Endpoint{{DNSName: "host.child.example.", RecordType: "A", RecordTTL: 60, Targets: []string{"192.0.2.2"}}}}}, store.ApplyOptions{Manager: store.ManagerSystem})
	check(t, err)
	_, err = f.r.Reconcile(f.ctx(), child.Spec.Zone)
	check(t, err)
	pending, err := f.repo.Read(f.ctx(), child.Spec.HeadZone())
	check(t, err)
	if pending.Head.Spec.NotifySequence == 0 {
		t.Fatal("child has no durable intent")
	}
	p.setHook(func() {
		timer := time.NewTimer(time.Until(deadline.Add(time.Millisecond)))
		defer timer.Stop()
		<-timer.C
	})
	before := f.s.Authority().Installed
	if err := f.r.NotifyOnce(f.ctx()); err == nil {
		t.Fatal("expired ancestor permitted acknowledgment")
	}
	after, err := f.repo.Read(f.ctx(), child.Spec.HeadZone())
	check(t, err)
	if p.calls.Load() != 1 || p.bad.Load() || !reflect.DeepEqual(pending, after) || !before.Same(f.s.Authority().Installed) {
		t.Fatal("ancestor expiry lost intent or changed durable generation")
	}
}

func TestNotifyCapsRejectExcessConfigurationAndNeverTruncate(t *testing.T) {
	for _, o := range []network.NotifyOptions{{MaxTargets: 65}, {MaxNames: 65}, {MaxAddresses: 9}, {Parallelism: 5}, {MaxTargets: -1}} {
		if _, err := network.NewNotify(o); !errors.Is(err, pr.ErrCapacity) {
			t.Fatal("excess option accepted", o, err)
		}
	}
}

func TestActualNotifyBoundGenerationAndHeadCannotBeSubstituted(t *testing.T) {
	for _, mode := range []string{"rotation-before-admit", "head-before-admit", "cancel-before-admit"} {
		t.Run(mode, func(t *testing.T) {
			p := peer(t, true)
			f := newFixture(t, notifyPolicy(p.target, true))
			f.change()
			ctx, release, _, _ := f.bound()
			defer release()
			pending := f.view()
			switch mode {
			case "rotation-before-admit":
				check(t, f.keys.Replace(f.ctx(), ring(t, "rotated")))
			case "cancel-before-admit":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "head-before-admit":
				rr := &public.DNSRecordSet{}
				check(t, f.s.Get(f.ctx(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "change"}, rr))
				rr.Spec.Endpoints[0].Targets = []string{"192.0.2.2"}
				_, err := f.s.Apply(f.ctx(), rr, store.ApplyOptions{Manager: store.ManagerSystem})
				check(t, err)
				_, err = f.r.Reconcile(f.ctx(), f.l.Spec.Zone)
				check(t, err)
				pending = f.view()
			}
			if _, err := f.a.Admission(ctx, pr.Command{Lifecycle: f.l, Expected: pending.Expected, Reason: "notify-delivery"}); err == nil {
				t.Fatal("substituted capture admitted")
			}
			if p.calls.Load() != 0 || !reflect.DeepEqual(pending, f.view()) {
				t.Fatal("failed admission mutated or sent")
			}
		})
	}
}

func TestNotifySendRejectsChangedModelAndMember(t *testing.T) {
	p := peer(t, true)
	f := newFixture(t, notifyPolicy(p.target, true))
	f.change()
	ctx, release, m, z := f.bound()
	defer release()
	v := f.view()
	_, err := f.a.Admission(ctx, pr.Command{Lifecycle: f.l, Expected: v.Expected, Reason: "notify-delivery"})
	check(t, err)
	changed := m
	changed.Sequence++
	if err := f.sender.Send(ctx, changed, z); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("changed member sent", err)
	}
	if err := f.sender.Send(ctx, m, (*zone.Zone)(nil)); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("changed model sent", err)
	}
	if p.calls.Load() != 0 {
		t.Fatal("binding denial sent")
	}
}
