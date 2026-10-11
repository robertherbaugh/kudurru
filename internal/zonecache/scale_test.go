package zonecache

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// scaleObjects returns n objects in the proportions of a large home or
// small-office install: mostly record sets (two labels each) over 50
// zones, plus policies, client groups and block lists.
func scaleObjects(n int) []store.Object {
	const zones, policies, groups, lists = 50, 200, 150, 100
	out := make([]store.Object, 0, n)
	for i := range zones {
		out = append(out, zone(fmt.Sprintf("z%02d", i), fmt.Sprintf("z%02d.example.", i)))
	}
	for i := range policies {
		out = append(out, policy("apps", fmt.Sprintf("p%03d", i), "ads"))
	}
	for i := range groups {
		out = append(out, clientgroup("apps", fmt.Sprintf("g%03d", i)))
	}
	for i := range lists {
		out = append(out, blocklist("apps", fmt.Sprintf("l%03d", i)))
	}
	for i := 0; len(out) < n; i++ {
		out = append(out, rrset(fmt.Sprintf("ns%02d", i%20), fmt.Sprintf("r%05d", i), fmt.Sprintf("z%02d", i%zones),
			map[string]string{"team": fmt.Sprintf("t%d", i%10), "app": fmt.Sprintf("a%d", i%300)}))
	}
	return out
}

func byKind(objs []store.Object) map[store.Kind][]store.Object {
	m := map[store.Kind][]store.Object{}
	for _, o := range objs {
		k := store.KindOf(o)
		m[k] = append(m[k], o)
	}
	return m
}

// TestRebuild10k measures the 10,000-object path end to end: initial sync
// to a Live snapshot, a single-change rebuild, the local.db write and the
// cold-start load. Bounds are loose (the race detector multiplies times);
// the report quotes runs without it.
func TestRebuild10k(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}
	const n = 10_000
	db, _ := openDB(t)
	st := newFakeStore()
	for _, o := range scaleObjects(n) {
		st.put(o, true)
	}
	c := newCache(t, st, Options{LocalDB: db, PersistInterval: time.Hour})
	if err := c.Register(zonesBuilder()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	startCache(t, c)
	snap := waitState(t, c, Live)
	toLive := time.Since(start)
	if snap.Objects.Len() != n {
		t.Fatalf("%d objects, want %d", snap.Objects.Len(), n)
	}
	if zs, _ := View[zoneSummary](snap, "zones"); len(zs) != 50 || len(zs["z07"]) != 190 {
		t.Fatalf("zones view: %d zones, %d record sets in z07", len(zs), len(zs["z07"]))
	}
	if got := len(snap.Objects.Select(KindDNSRecordSet, map[string]string{"team": "t3", "app": "a3"})); got != 32 {
		t.Fatalf("selector matched %d, want 32", got)
	}

	// One change: only DNSRecordSet is re-indexed.
	gen := snap.Generation
	changeAt := time.Now()
	st.put(rrset("ns00", "r00000", "z01", nil), true)
	eventually(t, 5*time.Second, "the change", func() bool { return c.Snapshot().Generation > gen })
	visible := time.Since(changeAt)

	full := timeIt(func() { c.indexOf(byKind(snap.Objects.List(KindDNSRecordSet)), nil, nil) })
	incr := timeIt(func() {
		c.indexOf(map[store.Kind][]store.Object{KindDNSZone: append([]store.Object(nil), snap.Objects.List(KindDNSZone)...)}, snap.Objects.idx, nil)
	})

	// local.db: the post-sync write, then the cold-start load and index.
	eventually(t, 30*time.Second, "the post-sync local.db write", func() bool { return loadCount(db) >= n })
	c.mu.Lock()
	c.persistDirty = true
	c.mu.Unlock()
	write := timeIt(func() {
		if err := c.persistOnce(context.Background()); err != nil {
			t.Error(err)
		}
	})
	var loaded int
	loadT := timeIt(func() { loaded = len(load(t, db).Objects) })
	if loaded != n {
		t.Fatalf("loaded %d objects", loaded)
	}
	t.Logf("10k objects: sync->Live %s (includes the %s debounce), one change visible after %s, record-set index %s, single-kind re-index %s, local.db write %s, load %s",
		toLive.Round(time.Millisecond), DefaultDebounce, visible.Round(time.Millisecond), full.Round(time.Microsecond),
		incr.Round(time.Microsecond), write.Round(time.Millisecond), loadT.Round(time.Millisecond))
	if visible > 2*time.Second {
		t.Errorf("a single change took %s to become visible", visible)
	}
}

func timeIt(fn func()) time.Duration {
	start := time.Now()
	fn()
	return time.Since(start)
}

func BenchmarkIndexFull10k(b *testing.B) {
	c := newCache(b, newFakeStore(), Options{})
	objs := byKind(scaleObjects(10_000))
	b.ReportAllocs()
	for b.Loop() {
		cp := make(map[store.Kind][]store.Object, len(objs))
		for k, v := range objs {
			cp[k] = append([]store.Object(nil), v...)
		}
		c.indexOf(cp, nil, nil)
	}
}

// BenchmarkIndexOneZone10k is the common change: one DNSZone edited in a
// 10k set; every other kind index is shared with the previous set.
func BenchmarkIndexOneZone10k(b *testing.B) {
	c := newCache(b, newFakeStore(), Options{})
	objs := byKind(scaleObjects(10_000))
	prev := c.indexOf(objs, nil, nil)
	zones := objs[KindDNSZone]
	b.ReportAllocs()
	for b.Loop() {
		c.indexOf(map[store.Kind][]store.Object{KindDNSZone: append([]store.Object(nil), zones...)}, prev, nil)
	}
}

// BenchmarkQueryPathView is what the authoritative stage does per query.
func BenchmarkQueryPathView(b *testing.B) {
	c := newCache(b, newFakeStore(), Options{})
	c.current.Store(&Snapshot{State: Live, views: map[string]any{"zones": zoneSummary{}}})
	b.ReportAllocs()
	for b.Loop() {
		s := c.Snapshot()
		if !s.Authoritative() {
			b.Fatal("not authoritative")
		}
		if _, ok := View[zoneSummary](s, "zones"); !ok {
			b.Fatal("no view")
		}
	}
}
