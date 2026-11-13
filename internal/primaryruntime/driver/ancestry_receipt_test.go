package driver

import (
	"fmt"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// D404: a parent/child pair of primary zones must converge when the
// lifecycle receipt of either zone is behind its DNSZone, as it is after
// every write of the committed-status controller (a status write advances
// the resourceVersion only). Before D404 the fence of a late child required
// both receipts to be exact, while realigning either receipt required the
// fence: a parent behind its DNSZone could never fence a new child, and a
// child behind its DNSZone could never be fenced by its parent. Both zones
// then stayed out of service ("zone awaiting committed publication") until
// the process restarted.

// statusWrite advances a DNSZone's resourceVersion the way the
// committed-status controller does, without changing its intent.
func (f *fixture) statusWrite(name string) {
	f.t.Helper()
	var z public.DNSZone
	if err := f.s.Get(f.ctx(), store.Key{Kind: "DNSZone", Name: name}, &z); err != nil {
		f.t.Fatal(err)
	}
	z.Status.Serial++
	if err := f.s.UpdateStatus(f.ctx(), &z); err != nil {
		f.t.Fatal(err)
	}
}

// admitted reports whether a complete image prepared from the store now
// admits apex: what the serving publisher would publish.
func (f *fixture) admitted(apex string) bool {
	image, _, err := f.s.ReadServingCandidate(f.ctx())
	if err != nil {
		return false
	}
	opts := f.image
	opts.Now = time.Now()
	p, err := pr.PrepareImage(f.ctx(), *image, opts)
	return err == nil && p.Admitted(apex, time.Now())
}

// restart replaces the runtime as a process restart does and returns a
// fresh driver start for the same store and writer.
func (f *fixture) restart() {
	f.t.Helper()
	r, err := pr.New(pr.Options{Store: f.s, Repository: f.repo, Reader: primaryzone.NewReader(f.s, primaryzone.ReaderOptions{PageSize: 1}), LifecycleCAS: f.a.LifecycleCAS, Admit: f.a.Admission, Publish: f.pub.publish, Notify: f.notify, Timeout: time.Second})
	if err != nil {
		f.t.Fatal(err)
	}
	f.r = r
}

func (f *fixture) receiptBehind(name string) bool {
	l := f.ledger(name)
	var z public.DNSZone
	if l == nil || f.s.Get(f.ctx(), store.Key{Kind: "DNSZone", Name: name}, &z) != nil {
		return false
	}
	return l.Spec.Source.ResourceVersion != z.ResourceVersion
}

func TestLateChildConvergesWhileParentReceiptIsBehind(t *testing.T) {
	f := setup(t)
	f.zone("parent", "example.")
	_, stop := f.start()
	awaitConverged(t, func() bool { return f.admitted("example.") })
	stop()
	f.statusWrite("parent")
	if !f.receiptBehind("parent") {
		t.Fatal("fixture did not put the parent receipt behind its DNSZone")
	}
	f.zone("child", "child.example.")
	f.restart()
	_, stop = f.start()
	defer stop()
	awaitConverged(t, func() bool { return f.admitted("example.") && f.admitted("child.example.") })
}

func TestPreparingChildConvergesWhileItsReceiptIsBehind(t *testing.T) {
	f := setup(t)
	f.zone("parent", "example.")
	_, stop := f.start()
	awaitConverged(t, func() bool { return f.admitted("example.") })
	stop()
	// The driver's first step for a new child: a Preparing lifecycle with an
	// unfenced obligation to its parent. A status write of the child lands
	// before the parent fences it.
	child := f.zone("child", "child.example.")
	l, err := pr.NewLifecycle(f.writer.Term.ClusterID, child, nil)
	if err != nil {
		t.Fatal(err)
	}
	l.Spec.Parents = []zk.ParentFence{pr.ParentObligation(f.ledger("parent"), l)}
	f.restart()
	if _, err := f.r.Transition(f.ctx(), nil, l); err != nil {
		t.Fatal(err)
	}
	f.statusWrite("child")
	if !f.receiptBehind("child") {
		t.Fatal("fixture did not put the child receipt behind its DNSZone")
	}
	_, stop = f.start()
	defer stop()
	awaitConverged(t, func() bool { return f.admitted("example.") && f.admitted("child.example.") })
}

func TestLateParentConvergesWhileChildReceiptIsBehind(t *testing.T) {
	f := setup(t)
	f.zone("child", "child.example.")
	_, stop := f.start()
	awaitConverged(t, func() bool { return f.admitted("child.example.") })
	stop()
	f.statusWrite("child")
	f.zone("parent", "example.")
	f.restart()
	_, stop = f.start()
	defer stop()
	awaitConverged(t, func() bool { return f.admitted("example.") && f.admitted("child.example.") })
}

// Both orders, and the child deleted and created again, with a status
// write of a zone after each change of its lifecycle phase, incarnation or
// parent obligations, as the committed-status controller writes after a
// committed change. (Like that controller, it does not write again for a
// receipt realignment alone, which would never settle.)
//
// Each step commits several lifecycle and boundary transactions under a
// concurrent status writer, so refusals and retry backoff are part of the
// path. Under the race detector one step took 3 to 11.6 s on a loaded
// shared runner, against the shared 8 s await. The tests in this file use
// awaitConverged, sized for that as the servingbuild pair test's minute is,
// and log every wait over 3 s so a slowdown stays visible.
func TestParentAndChildConvergeUnderStatusWrites(t *testing.T) {
	for _, parentFirst := range []bool{true, false} {
		f := setup(t)
		d, stop := f.start()
		writes := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			seen := map[string]string{}
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-writes:
					return
				case <-tick.C:
				}
				for _, name := range []string{"parent", "child"} {
					l := f.ledger(name)
					var z public.DNSZone
					if l == nil {
						continue
					}
					key := fmt.Sprint(l.Spec.Phase, l.Spec.Incarnation, l.Spec.Parents)
					if seen[name] == key || f.s.Get(f.ctx(), store.Key{Kind: "DNSZone", Name: name}, &z) != nil {
						continue
					}
					seen[name] = key
					z.Status.Serial++
					_ = f.s.UpdateStatus(f.ctx(), &z)
				}
			}
		}()
		if parentFirst {
			f.zone("parent", "example.")
			d.Wake()
			awaitConverged(t, func() bool { return f.admitted("example.") })
		} else {
			f.zone("child", "child.example.")
			d.Wake()
			awaitConverged(t, func() bool { return f.admitted("child.example.") })
		}
		for round := range 3 {
			if parentFirst || round > 0 {
				f.zone("child", "child.example.")
			} else {
				f.zone("parent", "example.")
			}
			d.Wake()
			awaitConverged(t, func() bool { return f.admitted("example.") && f.admitted("child.example.") })
			if err := f.s.Delete(f.ctx(), store.Key{Kind: "DNSZone", Name: "child"}, store.DeleteOptions{Manager: "api"}); err != nil {
				t.Fatal(err)
			}
			d.Wake()
			awaitConverged(t, func() bool {
				l := f.ledger("child")
				return l != nil && l.Spec.Phase == zk.PhaseRetired && f.admitted("example.")
			})
		}
		close(writes)
		<-done
		stop()
	}
}

// awaitConverged is the bound of the tests in this file: every wait spans
// several committed lifecycle and boundary steps.
func awaitConverged(t *testing.T, fn func() bool) {
	t.Helper()
	awaitWithin(t, time.Minute, 3*time.Second, fn)
}

// awaitWithin polls fn until it holds or bound expires, and logs a wait
// longer than slow.
func awaitWithin(t *testing.T, bound, slow time.Duration, fn func() bool) {
	t.Helper()
	start := time.Now()
	for time.Since(start) < bound {
		if fn() {
			if waited := time.Since(start); waited > slow {
				t.Logf("converged after %s", waited.Round(time.Millisecond))
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition did not converge within %s", bound)
}
