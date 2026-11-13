package driver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A new child zone's lifecycle steps run back to back (D404): each step
// that commits a lifecycle change and needs another (creation, the parent
// fence, the Preparing-to-Fencing CAS) ends the pass, and the next pass
// resumes at the same zone. Before, every one of those steps waited behind a
// step of every other zone, and each such step reconciles that zone and
// waits for its publication. On a mounted node under load a new child took
// up to 12 s to answer without the race detector, longer with more zones.
//
// The test counts the publication waits of unrelated active zones (two per
// step: the republish and the acknowledgment wait) from the moment the
// child's lifecycle exists until the child is active. With the focus the
// child's remaining steps (the parent fence, the Preparing-to-Fencing CAS,
// then activation) run back to back and no other zone is stepped between
// them; and the first pass on a roster with the new zone steps it first,
// so only the rest of the pass already running when it was created can come
// first. Without this each of those steps waited behind a full pass: 72
// waits of 8 zones after the child's lifecycle existed, 73 after its
// DNSZone was created (focus04-before.log).
func TestNewChildStepsAreNotQueuedBehindEveryZone(t *testing.T) {
	const others = 8
	f := setup(t)
	f.zone("family-parent", "family.example.")
	for i := range others {
		f.zone(fmt.Sprintf("other-%d", i), fmt.Sprintf("other%d.example.", i))
	}
	// The production operation timeout (10 s), not the fixture's 1 s: under
	// the race detector on a loaded runner a fence or activation step can
	// take longer than a second, and a step that times out is retried after
	// a backoff, during which the rotation rightly steps the other zones.
	// The count below is about the order of steps that succeed.
	r, err := pr.New(pr.Options{Store: f.s, Repository: f.repo, Reader: primaryzone.NewReader(f.s, primaryzone.ReaderOptions{PageSize: 1}), LifecycleCAS: f.a.LifecycleCAS, Admit: f.a.Admission, Publish: f.pub.publish, Notify: f.notify, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	f.r = r
	_, stop := f.startWithin(10 * time.Second)
	defer stop()
	awaitConverged(t, func() bool {
		if !f.active("family.example.", 1) {
			return false
		}
		for i := range others {
			if !f.active(fmt.Sprintf("other%d.example.", i), 1) {
				return false
			}
		}
		return true
	})

	var (
		mu        sync.Mutex
		counting  bool
		lifecycle bool
		waits     int
		total     int
	)
	f.pub.setObserve(func(m pr.Member) {
		mu.Lock()
		defer mu.Unlock()
		if !counting || m.Zone.Apex == "family.example." || m.Zone.Apex == "child.family.example." {
			return
		}
		total++
		if !lifecycle {
			lifecycle = f.ledger("family-child") != nil
		}
		if lifecycle {
			waits++
		}
	})
	mu.Lock()
	counting = true
	mu.Unlock()
	started := time.Now()
	f.zone("family-child", "child.family.example.")
	awaitConverged(t, func() bool { return f.active("child.family.example.", 1) })
	mu.Lock()
	counting = false
	got, all := waits, total
	mu.Unlock()
	t.Logf("child active %s after its creation; %d publication waits of the %d other zones after its DNSZone was created, %d after its lifecycle existed", time.Since(started).Round(time.Millisecond), all, others, got)
	// From the DNSZone's creation: at most the rest of the pass that was
	// already running on the roster read before it (at most three waits
	// per zone: an activation's publication, the republish and the
	// acknowledgment wait); the next pass steps the new zone first.
	if all > 3*others {
		t.Fatalf("the new child waited behind %d publication waits of %d other zones; want at most %d", all, others, 3*others)
	}
	// At most the one step that was already waiting when the lifecycle was
	// committed.
	if got > 2 {
		t.Fatalf("the child's steps waited behind %d publication waits of %d other zones; want at most 2", got, others)
	}
}

// Stepping zones that are being created ahead of the rotation must not
// starve the zones late in it: the rotation's cursor keeps its place, so an
// edit of the last zone is still reconciled while another zone's status is
// written every few milliseconds and a new zone is created meanwhile.
func TestLeadingZonesKeepTheRotationFair(t *testing.T) {
	const others = 8
	f := setup(t)
	for i := range others {
		f.zone(fmt.Sprintf("other-%d", i), fmt.Sprintf("other%d.example.", i))
	}
	_, stop := f.start()
	defer stop()
	awaitConverged(t, func() bool {
		for i := range others {
			if !f.active(fmt.Sprintf("other%d.example.", i), 1) {
				return false
			}
		}
		return true
	})
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
			}
			var z public.DNSZone
			if err := f.s.Get(f.ctx(), store.Key{Kind: "DNSZone", Name: "other-0"}, &z); err != nil {
				t.Error(err)
				return
			}
			z.Status.Serial++
			if err := f.s.UpdateStatus(f.ctx(), &z); err != nil && !errors.Is(err, store.ErrConflict) {
				t.Error(err)
				return
			}
		}
	})
	defer func() { close(done); wg.Wait() }()
	last := fmt.Sprintf("other-%d", others-1)
	var z public.DNSZone
	if err := f.s.Get(f.ctx(), store.Key{Kind: "DNSZone", Name: last}, &z); err != nil {
		t.Fatal(err)
	}
	z.Spec.DefaultTTL = 120
	saved, err := f.s.Apply(f.ctx(), &z, store.ApplyOptions{Manager: "api"})
	if err != nil {
		t.Fatal(err)
	}
	edited := saved.GetResourceVersion()
	f.zone("new-zone", "new.example.")
	awaitConverged(t, func() bool {
		l := f.ledger(last)
		return l != nil && l.Spec.Source.ResourceVersion == edited
	})
}

// Only a zone that is being created takes the focus (D404). An active
// nonprimary zone whose DNSZone changes before every pass refreshes its
// lifecycle source on each of its steps; that refresh wakes the loop but
// must not end the pass and resume at the same zone. Otherwise the zone took
// up to maxFocus+1 steps for every step of the rest of the rotation. Here a
// status write of the forward zone's DNSZone follows every committed change
// of its lifecycle, so its source is always ahead at the next roster read.
// The test reads the forward zone's lifecycle sequence at every publication
// wait of the other zones and fails when it advanced more than a fair
// rotation allows between two of them (one step per pass, so at most two
// across a pass boundary).
func TestRefreshedActiveZoneDoesNotTakeTheFocus(t *testing.T) {
	const others = 8
	f := setup(t)
	for i := range others {
		f.zone(fmt.Sprintf("other-%d", i), fmt.Sprintf("other%d.example.", i))
	}
	fwd := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "forward"}, Spec: public.DNSZoneSpec{ZoneName: "forward.test.", Type: public.ZoneTypeForward, Forward: &public.ForwardSpec{UpstreamGroup: "default"}, Policy: public.ZonePolicy{QueryAccess: public.AccessAllow}}}
	if _, err := f.s.Apply(f.ctx(), fwd, store.ApplyOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	var (
		mu       sync.Mutex
		counting bool
		last     uint64
		seen     bool
		worst    uint64
		waits    int
	)
	cas := func(ctx context.Context, current, next *zk.ZoneLifecycle) (*zk.ZoneLifecycle, error) {
		out, err := f.a.LifecycleCAS(ctx, current, next)
		mu.Lock()
		touch := counting
		mu.Unlock()
		if err == nil && touch && next.Spec.Zone.ObjectName == "forward" {
			var z public.DNSZone
			if err := f.s.Get(f.ctx(), store.Key{Kind: "DNSZone", Name: "forward"}, &z); err != nil {
				t.Error(err)
			} else {
				z.Status.Serial++
				if err := f.s.UpdateStatus(f.ctx(), &z); err != nil {
					t.Error(err)
				}
			}
		}
		return out, err
	}
	r, err := pr.New(pr.Options{Store: f.s, Repository: f.repo, Reader: primaryzone.NewReader(f.s, primaryzone.ReaderOptions{PageSize: 1}), LifecycleCAS: cas, Admit: f.a.Admission, Publish: f.pub.publish, Notify: f.notify, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	f.r = r
	d, stop := f.start()
	defer stop()
	awaitConverged(t, func() bool {
		if !f.active("forward.test.", 1) {
			return false
		}
		for i := range others {
			if !f.active(fmt.Sprintf("other%d.example.", i), 1) {
				return false
			}
		}
		return true
	})
	f.pub.setObserve(func(m pr.Member) {
		mu.Lock()
		defer mu.Unlock()
		if !counting || m.Zone.Apex == "forward.test." {
			return
		}
		l := f.ledger("forward")
		if l == nil {
			return
		}
		if seen && l.Spec.Sequence-last > worst {
			worst = l.Spec.Sequence - last
		}
		last, seen = l.Spec.Sequence, true
		waits++
	})
	start := f.ledger("forward").Spec.Sequence
	mu.Lock()
	counting = true
	mu.Unlock()
	// Start the chain: one status write; each refresh then causes the next.
	f.statusWrite("forward")
	d.Wake()
	awaitConverged(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return waits >= 10*others
	})
	mu.Lock()
	counting = false
	got, n := worst, waits
	mu.Unlock()
	refreshed := f.ledger("forward").Spec.Sequence - start
	t.Logf("forward zone refreshed %d times during %d publication waits of %d other zones; at most %d refreshes between two of them", refreshed, n, others, got)
	if refreshed < uint64(others) {
		t.Fatalf("the forward zone refreshed only %d times; its DNSZone did not stay ahead of the driver", refreshed)
	}
	if got > 2 {
		t.Fatalf("the refreshed forward zone took %d steps between two steps of the other zones; want at most 2", got)
	}
}
