package servingbuild

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/controller"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/driver"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A primary zone example. and a primary zone child.example. on one node,
// created in both orders, each written (a name-server change rewrites its
// apex), the child deleted and created again, three rounds: with the real
// driver, the real committed-status controller (whose writes put lifecycle
// receipts behind their DNSZones) and the real complete publisher, every
// pair converges, and every zone that has been served keeps being served by
// the DNS publication (Publisher.Capture) until the test deletes it (D404).
//
// Before D404 a round stopped converging for good, typically within four
// rounds (CORE-CHILD-ZONE repro01, measure06): the parent's receipt was
// behind its DNSZone, the child's fence required the exact receipt, and the
// parent's realignment required the fence. And the parent was out of
// service from the child's creation until its fence was published (up to
// 0.8 s, measure05/measure06), the child from its late parent's fence until
// that parent activated (0.5 s, measure05).
func TestPrimaryParentAndChildServeAcrossEachOther(t *testing.T) {
	for _, parentFirst := range []bool{true, false} {
		t.Run(fmt.Sprint("parentFirst=", parentFirst), func(t *testing.T) { primaryParentAndChild(t, parentFirst) })
	}
}

func primaryParentAndChild(t *testing.T, parentFirst bool) {
	keys, err := dnsserver.NewStaticKeyring()
	if err != nil {
		t.Fatal(err)
	}
	keyrings, err := NewKeyringOwner(keys)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixtureWithKeyrings(t, keyrings)
	r, publication, ctx := primaryFixture(t, f)
	if err := f.store.Delete(ctx, store.Key{Kind: "DNSZone", Name: "uncommitted"}, store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	// Production intervals: the driver's default poll and operation bounds.
	d, err := driver.New(driver.Options{Store: f.store, Runtime: r, Publication: publication, Image: pr.ImageOptions{ClusterID: f.store.Info().ClusterID, Scheme: f.builder.opts.Schema.scheme, Validation: f.builder.opts.Schema.validation(ctx)}})
	if err != nil {
		t.Fatal(err)
	}
	writer, _ := pr.PublicationFenceFrom(ctx)
	original, err := f.store.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	runctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 2)
	go func() { done <- d.Run(runctx, writer) }()
	status, err := NewPrimaryStatus(PrimaryStatusOptions{Store: f.store, Publisher: f.pub, Pin: keyrings.PrimaryPin(f.pub), Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	ctrl, err := controller.New(f.store, controller.Options{Workers: 1, CommittedPrimaryStatus: status, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	go func() { done <- ctrl.Run(sqlite.WithWriterToken(runctx, original)) }()
	t.Cleanup(func() {
		cancel()
		for range 2 {
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Log(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("driver or controller failed to join")
			}
		}
	})
	// served reads the publication DNS answers from, as a query does.
	served := func(apex string) (bool, string) {
		lease, err := f.pub.Capture()
		if err != nil {
			return false, "capture: " + err.Error()
		}
		defer lease.Release()
		value, _ := lease.Bundle().Component("runtime")
		owner, ok := value.(*RuntimeOwner)
		if !ok || owner == nil || owner.Primary() == nil || owner.Zones() == nil {
			return false, "no runtime"
		}
		now := time.Now()
		route := owner.Zones().Zones(now).Match(apex)
		if owner.Primary().Serves(apex, now) && route != nil && route.Name() == apex && route.RoutingAvailableAt(now) {
			return true, ""
		}
		m, _ := owner.Primary().Cut(now).Member(apex)
		why := fmt.Sprintf("serves=%v phase=%s incarnation=%d commit=%d", owner.Primary().Serves(apex, now), m.Phase, m.Incarnation, m.Version.CommitSequence)
		for _, p := range m.Parents {
			pm, _ := owner.Primary().Cut(now).Member(p.Zone.Apex)
			why += fmt.Sprintf(" [obligation commit=%d released=%v parent phase=%s commit=%d]", p.CommitSequence, p.Released, pm.Phase, pm.Version.CommitSequence)
		}
		if route != nil {
			why += fmt.Sprintf(" route=%s available=%v", route.Name(), route.RoutingAvailableAt(now))
		}
		return false, why
	}
	var (
		mu      sync.Mutex
		action  string
		watched = map[string]bool{}
		gaps    []string
	)
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		down := map[string]bool{}
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
			mu.Lock()
			names := make([]string, 0, len(watched))
			for apex := range watched {
				names = append(names, apex)
			}
			mu.Unlock()
			for _, apex := range names {
				ok, why := served(apex)
				mu.Lock()
				if watched[apex] && !ok && !down[apex] {
					gaps = append(gaps, fmt.Sprintf("%s unserved during %q: %s", apex, action, why))
				}
				down[apex] = watched[apex] && !ok
				mu.Unlock()
			}
		}
	}()
	set := func(label string) { mu.Lock(); action = label; mu.Unlock() }
	wait := func(label string, apices ...string) {
		t.Helper()
		started := time.Now()
		for {
			all := true
			for _, apex := range apices {
				ok, _ := served(apex)
				all = all && ok
			}
			if all {
				t.Logf("%s served after %s", label, time.Since(started).Round(time.Millisecond))
				mu.Lock()
				for _, apex := range apices {
					watched[apex] = true
				}
				mu.Unlock()
				return
			}
			if time.Since(started) > time.Minute {
				t.Fatalf("%s: %v never served: %+v", label, apices, d.Status())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	apply := func(name, apex, ns string) {
		t.Helper()
		set(name + " " + ns)
		var z public.DNSZone
		if err := f.store.Get(ctx, store.Key{Kind: "DNSZone", Name: name}, &z); err != nil {
			z = public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: apex}}
		}
		z.Spec.Nameservers = []string{ns}
		if _, err := f.store.Apply(ctx, &z, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			t.Fatal(err)
		}
	}
	if parentFirst {
		apply("parent", "example.", "ns.example.")
		wait("parent", "example.")
	} else {
		apply("child", "child.example.", "ns.child.example.")
		wait("child", "child.example.")
	}
	for round := range 3 {
		if parentFirst || round > 0 {
			apply("child", "child.example.", "ns.child.example.")
		} else {
			apply("parent", "example.", "ns.example.")
		}
		wait(fmt.Sprint("pair ", round), "example.", "child.example.")
		apply("parent", "example.", fmt.Sprintf("ns%d.example.", round))
		time.Sleep(300 * time.Millisecond)
		apply("child", "child.example.", fmt.Sprintf("ns%d.child.example.", round))
		time.Sleep(300 * time.Millisecond)
		mu.Lock()
		delete(watched, "child.example.")
		mu.Unlock()
		set("delete child")
		if err := f.store.Delete(ctx, store.Key{Kind: "DNSZone", Name: "child"}, store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		wait(fmt.Sprint("parent after deletion ", round), "example.")
	}
	close(stop)
	<-sampled
	mu.Lock()
	defer mu.Unlock()
	for _, gap := range gaps {
		t.Error(gap)
	}
}
