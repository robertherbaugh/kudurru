package controller

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
)

// latencies collects reconcile durations per kind from the ReconcileHook.
type latencies struct {
	mu     sync.Mutex
	byKind map[store.Kind][]time.Duration
	wrote  chan store.Key // zone keys whose status was written
	// rsWrote receives the record sets whose status was written.
	rsWrote chan store.Key
}

func newLatencies() *latencies {
	return &latencies{byKind: map[store.Kind][]time.Duration{}, wrote: make(chan store.Key, 4096), rsWrote: make(chan store.Key, 4096)}
}

func (l *latencies) hook(key store.Key, took time.Duration, wrote bool, _ error) {
	l.mu.Lock()
	l.byKind[key.Kind] = append(l.byKind[key.Kind], took)
	l.mu.Unlock()
	if wrote && key.Kind == KindDNSZone {
		select {
		case l.wrote <- key:
		default:
		}
	}
	if wrote && key.Kind == KindDNSRecordSet {
		select {
		case l.rsWrote <- key:
		default:
		}
	}
}

func (l *latencies) summary(kind store.Kind) string {
	l.mu.Lock()
	d := slices.Clone(l.byKind[kind])
	l.mu.Unlock()
	return pct(d)
}

func pct(d []time.Duration) string {
	if len(d) == 0 {
		return "n=0"
	}
	slices.Sort(d)
	at := func(p float64) time.Duration { return d[min(len(d)-1, int(p*float64(len(d))))] }
	return fmt.Sprintf("n=%d p50=%s p90=%s p99=%s max=%s", len(d), at(.5), at(.9), at(.99), d[len(d)-1])
}

// measureEnv starts a controller over inner with a latency hook and one
// zone.
func measureEnv(t *testing.T, inner store.Store, coalesce time.Duration) (*env, *latencies) {
	t.Helper()
	lat := newLatencies()
	e := newEnvOver(t, inner, func(o *Options) {
		o.ReconcileHook = lat.hook
		o.Events = &MemorySink{}
		o.ZoneCoalesce = coalesce
	})
	e.apply(zoneObj("example", "example.com."))
	e.start()
	e.eventually("zone stamped", func() bool { return e.zone("example").Status.Serial != 0 })
	e.settle()
	return e, lat
}

// TestMeasureRecordSetChanges measures status writes and reconcile latency
// for 1,000 record-set changes against the memory driver, in a burst
// (applied back to back; the queues coalesce) and paced (each change
// converges before the next; end-to-end latency per event).
func TestMeasureRecordSetChanges(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement")
	}
	const n = 1000
	for _, coalesce := range []time.Duration{-1, DefaultZoneCoalesce} {
		t.Run(fmt.Sprintf("burst creates, zone coalescing %s", max(coalesce, 0)), func(t *testing.T) {
			burst(t, n, coalesce)
		})
	}
	t.Run("paced updates", func(t *testing.T) {
		e, lat := measureEnv(t, memory.New(), -1)
		const sets = 100
		objs := make([]*v1alpha1.DNSRecordSet, sets)
		var tuples []RecordTuple
		for i := range sets {
			name := fmt.Sprintf("h%03d.example.com", i)
			objs[i] = rrsetObj("apps", fmt.Sprintf("rs%03d", i), "example", time.Duration(i)*time.Second, ep(name, "A", "192.0.2.1"))
			e.apply(objs[i])
			tuples = append(tuples, RecordTuple{Name: name + ".", Type: "A", TTL: 300, Targets: []string{"192.0.2.1"}})
		}
		// The baseline is taken only once the setup has converged, so
		// none of its writes is counted against the changes below.
		e.waitConverged("example", "example.com.", tuples)
		drain(lat.wrote)
		drain(lat.rsWrote)
		w0 := e.st.Writes("")
		var e2e []time.Duration
		for i := range n {
			o := objs[i%sets].DeepCopy()
			o.Spec.Endpoints[0].Targets = []string{fmt.Sprintf("198.51.100.%d", i%250+1)}
			if i/sets%2 == 1 {
				o.Spec.Endpoints[0].Targets = append(o.Spec.Endpoints[0].Targets, "203.0.113.1")
			}
			objs[i%sets] = o
			t1 := time.Now()
			e.apply(o)
			select {
			case <-lat.wrote:
			case <-time.After(10 * time.Second):
				t.Fatalf("change %d: zone status not written", i)
			}
			e2e = append(e2e, time.Since(t1))
			// The record set's own status write (observedGeneration) runs
			// on another worker and may land after the zone's: wait for it
			// too, or under -cpu 1 it slips past the next change of the
			// same object and two generations share one write.
			key := store.KeyOf(o)
			for got := false; !got; {
				select {
				case k := <-lat.rsWrote:
					got = k == key
				case <-time.After(10 * time.Second):
					t.Fatalf("change %d: record set status not written", i)
				}
			}
		}
		e.settle()
		writes := e.st.Writes("") - w0
		t.Logf("paced: %d updates; status writes %d = %.3f per change (each change: rrset observedGeneration + zone serial)", n, writes, float64(writes)/n)
		t.Logf("paced end-to-end (Apply -> zone status written): %s", pct(e2e))
		t.Logf("paced reconcile latency DNSRecordSet: %s", lat.summary(KindDNSRecordSet))
		t.Logf("paced reconcile latency DNSZone: %s", lat.summary(KindDNSZone))
		if writes != 2*n {
			t.Errorf("paced status writes %d, want %d", writes, 2*n)
		}
	})
	t.Run("resync of 1000 converged record sets", func(t *testing.T) {
		e, _ := measureEnv(t, memory.New(), -1)
		var tuples []RecordTuple
		for i := range n {
			name := fmt.Sprintf("h%04d.example.com", i)
			e.apply(rrsetObj("apps", fmt.Sprintf("rs%04d", i), "example", time.Duration(i)*time.Second, ep(name, "A", "192.0.2.1")))
			tuples = append(tuples, RecordTuple{Name: name + ".", Type: "A", TTL: 300, Targets: []string{"192.0.2.1"}})
		}
		// Converged must mean converged: the first term's own writes
		// would otherwise be counted as the resync's.
		e.waitConverged("example", "example.com.", tuples)
		w0 := e.st.Writes("")
		r0 := sumCounter(t, e.ctrl.metrics.reconciles)
		t1 := time.Now()
		e.stop() // a new term relists everything, like a resync
		e.start()
		e.waitLong("relisted and reconciled", func() bool { return sumCounter(t, e.ctrl.metrics.reconciles) >= r0+n+1 })
		took := time.Since(t1)
		e.settle()
		t.Logf("new term over %d converged record sets: all reconciled in %s, %d status writes", n, took.Round(time.Millisecond), e.st.Writes("")-w0)
		if e.st.Writes("") != w0 {
			t.Errorf("resync wrote %d statuses", e.st.Writes("")-w0)
		}
	})
}

// burst applies n new record sets back to back and reports the status
// writes and reconcile latencies until the zone has converged.
func burst(t *testing.T, n int, coalesce time.Duration) {
	e, lat := measureEnv(t, memory.New(), coalesce)
	w0, z0 := e.st.Writes(""), e.st.Writes(KindDNSZone)
	s0 := e.zone("example").Status.Serial
	start := time.Now()
	var tuples []RecordTuple
	for i := range n {
		name := fmt.Sprintf("h%04d.example.com", i)
		e.apply(rrsetObj("apps", fmt.Sprintf("rs%04d", i), "example", time.Duration(i)*time.Second, ep(name, "A", "192.0.2.1")))
		tuples = append(tuples, RecordTuple{Name: name + ".", Type: "A", TTL: 300, Targets: []string{"192.0.2.1"}})
	}
	applied := time.Since(start)
	want := zoneHash(t, "example.com.", tuples)
	e.waitLong("zone converged", func() bool { return e.zone("example").Status.ContentHash == want })
	took := time.Since(start)
	e.waitConverged("example", "example.com.", tuples)
	writes, zoneWrites := e.st.Writes("")-w0, e.st.Writes(KindDNSZone)-z0
	t.Logf("burst: %d creates applied in %s, zone converged after %s; status writes %d (DNSRecordSet %d, DNSZone %d) = %.3f per change; serial bumps %d",
		n, applied.Round(time.Millisecond), took.Round(time.Millisecond), writes, e.st.Writes(KindDNSRecordSet), zoneWrites, float64(writes)/float64(n), e.zone("example").Status.Serial-s0)
	t.Logf("burst reconcile latency DNSRecordSet: %s", lat.summary(KindDNSRecordSet))
	t.Logf("burst reconcile latency DNSZone: %s", lat.summary(KindDNSZone))
	if got := e.st.Writes(KindDNSRecordSet); got != n {
		t.Errorf("record set status writes %d, want exactly %d (one per new object)", got, n)
	}
	if zoneWrites < 1 || zoneWrites > n {
		t.Errorf("zone status writes %d out of [1, %d]", zoneWrites, n)
	}
}

// TestMeasureKube repeats the burst against the kube driver (envtest).
// It is slow (client-side QPS 50) and runs with KUDURRU_MEASURE=1.
func TestMeasureKube(t *testing.T) {
	if os.Getenv("KUDURRU_MEASURE") == "" {
		t.Skip("set KUDURRU_MEASURE=1")
	}
	const n = 300
	lat := newLatencies()
	e := kubeEnv(t, func(o *Options) {
		o.ReconcileHook = lat.hook
		o.Events = &MemorySink{}
		o.ZoneCoalesce = DefaultZoneCoalesce
	})
	zn := uniq("m")
	origin := zn + ".example."
	e.apply(zoneObj(zn, origin))
	e.start()
	e.eventually("zone stamped", func() bool { return e.zone(zn).Status.Serial != 0 })
	e.settle()
	w0 := e.st.Writes("")
	start := time.Now()
	var tuples []RecordTuple
	for i := range n {
		name := fmt.Sprintf("h%04d.%s", i, origin)
		e.apply(rrsetObj("apps", fmt.Sprintf("%s-rs%04d", zn, i), zn, 0, ep(name, "A", "192.0.2.1")))
		tuples = append(tuples, RecordTuple{Name: name, Type: "A", TTL: 300, Targets: []string{"192.0.2.1"}})
	}
	applied := time.Since(start)
	want := zoneHash(t, origin, tuples)
	e.eventually("zone converged", func() bool { return e.zone(zn).Status.ContentHash == want })
	took := time.Since(start)
	e.settle()
	conflicts := 0.0
	ch := make(chan prometheus.Metric, 16)
	go func() { e.ctrl.metrics.reconciles.Collect(ch); close(ch) }()
	for m := range ch {
		var pb dto.Metric
		_ = m.Write(&pb)
		for _, l := range pb.GetLabel() {
			if l.GetName() == "result" && l.GetValue() == "conflict" {
				conflicts += pb.GetCounter().GetValue()
			}
		}
	}
	t.Logf("kube burst: %d creates applied in %s, zone converged after %s; status writes %d (DNSRecordSet %d, DNSZone %d) = %.3f per change; conflicts retried %.0f",
		n, applied.Round(time.Millisecond), took.Round(time.Millisecond), e.st.Writes("")-w0, e.st.Writes(KindDNSRecordSet), e.st.Writes(KindDNSZone)-1, float64(e.st.Writes("")-w0)/n, conflicts)
	t.Logf("kube reconcile latency DNSRecordSet: %s", lat.summary(KindDNSRecordSet))
	t.Logf("kube reconcile latency DNSZone: %s", lat.summary(KindDNSZone))
}

// convergeTimeout bounds waitLong. With zone coalescing off, each of 1,000
// record-set changes rebuilds the zone view and the zone model over up to
// 1,000 record sets; under -race on a loaded machine converging takes
// longer than eventually's 10 s.
const convergeTimeout = 2 * time.Minute

// waitLong is eventually with convergeTimeout, polling less often (the
// record-set condition below lists every object).
func (e *env) waitLong(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(convergeTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out after %s waiting for %s", convergeTimeout, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitConverged waits until zone serves exactly tuples and every record
// set's status is for its current generation, then settles.
//
// settle alone does not prove convergence after a large apply: it calls
// the writes done after 50 ms without one, and under -race and load a
// single reconcile that rebuilds the view or the zone of 1,000 record sets
// takes longer than that. settle then returned with hundreds of record-set
// writes still queued, which landed after the baseline (a resync of a
// "converged" store writing 454-949 statuses; paced updates counting
// 2,001-2,003 writes for 2,000), or it did not see the writes stop within
// its 10 s deadline at all.
func (e *env) waitConverged(zone, origin string, tuples []RecordTuple) {
	e.t.Helper()
	want := zoneHash(e.t, origin, tuples)
	e.waitLong("zone "+zone+" converged", func() bool { return e.zone(zone).Status.ContentHash == want })
	e.waitLong("record set statuses current", func() bool {
		list, err := e.st.List(context.Background(), KindDNSRecordSet, store.ListOptions{})
		if err != nil {
			e.t.Fatal(err)
		}
		for _, o := range list.Items {
			rs, ok := o.(*v1alpha1.DNSRecordSet)
			if !ok || rs.Status.ObservedGeneration != rs.Generation {
				return false
			}
			if c := condition(rs.Status.Conditions, v1alpha1.ConditionReady); c == nil || c.ObservedGeneration != rs.Generation {
				return false
			}
		}
		return true
	})
	e.settle()
}

func drain(ch chan store.Key) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
