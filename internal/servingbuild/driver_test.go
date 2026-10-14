package servingbuild

import (
	"context"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/driver"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDriverPublishesRealWholeBundleAcrossReplacementAndLateParent(t *testing.T) {
	f := newFixture(t)
	r, publication, ctx := primaryFixture(t, f)
	var desired public.DNSZone
	if err := f.store.Get(ctx, store.Key{Kind: "DNSZone", Name: "uncommitted"}, &desired); err != nil {
		t.Fatal(err)
	}
	desired.Spec.Nameservers = []string{"ns." + desired.Spec.ZoneName}
	object, err := f.store.Apply(ctx, &desired, store.ApplyOptions{Manager: store.ManagerSystem})
	if err != nil {
		t.Fatal(err)
	}
	z := object.(*public.DNSZone)
	d, err := driver.New(driver.Options{Store: f.store, Runtime: r, Publication: publication, Image: pr.ImageOptions{ClusterID: f.store.Info().ClusterID, Scheme: f.builder.opts.Schema.scheme, Validation: f.builder.opts.Schema.validation(ctx)}, PollInterval: 10 * time.Millisecond, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	writer, _ := pr.PublicationFenceFrom(ctx)
	runctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- d.Run(runctx, writer) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("driver failed to join")
		}
	})
	wait := func(check func(*RuntimeOwner) bool) {
		t.Helper()
		deadline := time.NewTimer(15 * time.Second)
		defer deadline.Stop()
		for {
			changed := f.pub.Subscribe()
			lease, err := f.pub.CaptureHTTP(ctx)
			if err == nil {
				value, _ := lease.Bundle().Component("runtime")
				ok := check(value.(*RuntimeOwner))
				lease.Release()
				if ok {
					return
				}
			}
			select {
			case <-changed:
			case <-deadline.C:
				t.Fatalf("driver failed to publish: %+v publisher: %+v", d.Status(), f.pub.Status())
			}
		}
	}
	active := func(z *public.DNSZone) func(*RuntimeOwner) bool {
		return func(owner *RuntimeOwner) bool {
			m, ok := owner.Primary().DurableMember(zk.ZoneID{ClusterID: writer.Term.ClusterID, ObjectName: z.Name, UID: string(z.UID), Apex: z.Spec.ZoneName})
			return ok && m.Phase == zk.PhaseActive && owner.Primary().Admitted(z.Spec.ZoneName, f.now) && owner.Zones().Zones(f.now).Match(z.Spec.ZoneName).RoutingAvailableAt(f.now)
		}
	}
	wait(active(z))
	parentObject, err := f.store.Apply(ctx, &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "late-parent"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "example.", Nameservers: []string{"ns.example."}}}, store.ApplyOptions{Manager: store.ManagerSystem})
	if err != nil {
		t.Fatal(err)
	}
	parent := parentObject.(*public.DNSZone)
	wait(func(owner *RuntimeOwner) bool { return active(z)(owner) && active(parent)(owner) })
	// Removing a parent must recover a surviving child through retirement and
	// a fresh incarnation, including exact release against its historical head.
	if err := f.store.Delete(ctx, store.KeyOf(parent), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	wait(func(owner *RuntimeOwner) bool {
		m, ok := owner.Primary().Cut(f.now).Member(z.Spec.ZoneName)
		return active(z)(owner) && ok && m.Incarnation > 1 && len(m.Parents) == 0
	})
	old := z
	if err := f.store.Delete(ctx, store.KeyOf(z), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	object, err = f.store.Apply(ctx, &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: z.Name}, Spec: z.Spec}, store.ApplyOptions{Manager: store.ManagerSystem})
	if err != nil {
		t.Fatal(err)
	}
	z = object.(*public.DNSZone)
	if z.UID == old.UID {
		t.Fatal("replacement did not change UID")
	}
	wait(active(z))
	if err := publication.Wait(ctx, writer, pr.Member{}); err == nil {
		t.Fatal("empty publication target accepted")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && err != context.Canceled && err != serving.ErrSuperseded {
			t.Fatal(err)
		}
		// Cleanup also joins; retain one terminal result for it.
		done <- err
	case <-time.After(5 * time.Second):
		t.Fatal("driver did not stop")
	}
}
