package zonecache

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/health"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
)

func apply(t testing.TB, s store.Store, objs ...store.Object) {
	t.Helper()
	for _, o := range objs {
		if _, err := s.Apply(context.Background(), o, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
}

func newCache(t testing.TB, st store.Store, opts Options) *Cache {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = quietLogger()
	}
	if opts.Registerer == nil {
		opts.Registerer = prometheus.NewRegistry()
	}
	c, err := New(st, opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Cache.Ready plugs straight into the ops listener.
var _ health.ReadyFunc = (*Cache)(nil).Ready

func TestDefaultKinds(t *testing.T) {
	want := []store.Kind{"BlockList", "ClientGroup", "DNSRecordSet", "DNSZone", "Integration", "Kudurru", "Policy"}
	if got := DefaultKinds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultKinds() = %v, want %v", got, want)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(nil, Options{}); err == nil {
		t.Fatal("New(nil) must fail")
	}
	reg := prometheus.NewRegistry()
	if _, err := New(memory.New(), Options{Registerer: reg}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(memory.New(), Options{Registerer: reg}); err == nil {
		t.Fatal("registering the metrics twice on one registry must fail")
	}
	c := newCache(t, memory.New(), Options{})
	if err := c.Register(zonesBuilder()); err != nil {
		t.Fatal(err)
	}
	if err := c.Register(zonesBuilder()); err == nil {
		t.Fatal("duplicate builder name accepted")
	}
	if err := c.Register(NewBuilder("", nil)); err == nil {
		t.Fatal("unnamed builder accepted")
	}
	snap := c.Snapshot()
	if snap == nil || snap.State != Unsynced || snap.Generation != 0 || snap.Authoritative() || snap.Objects.Len() != 0 {
		t.Fatalf("initial snapshot: %+v", snap)
	}
	if c.NeedLeaderElection() {
		t.Fatal("zonecache is always-on")
	}
}

func TestLiveSyncWithMemoryDriver(t *testing.T) {
	st := memory.New()
	apply(t, st,
		zone("home", "home.example."),
		zone("lab", "lab.example."),
		rrset("apps", "nas", "home", map[string]string{"team": "infra", "tier": "storage"}),
		rrset("apps", "web", "home", map[string]string{"team": "web"}),
		rrset("dns", "ns1", "lab", map[string]string{"team": "infra"}),
		rrset("dns", "orphan", "missing", nil),
		policy("apps", "kids", "ads"),
		clientgroup("apps", "kids"),
		blocklist("apps", "ads"),
	)
	reg := prometheus.NewRegistry()
	c := newCache(t, st, Options{Registerer: reg})
	if err := c.Register(zonesBuilder()); err != nil {
		t.Fatal(err)
	}
	startCache(t, c)
	select {
	case <-c.Initialized():
	case <-time.After(10 * time.Second):
		t.Fatal("not initialized")
	}
	snap := waitState(t, c, Live)
	if !c.Synced() || c.State() != Live || !snap.Authoritative() {
		t.Fatalf("synced=%v state=%s authoritative=%v", c.Synced(), c.State(), snap.Authoritative())
	}
	if ok, msgs := c.Ready(); !ok || len(msgs) != 0 {
		t.Fatalf("Ready() = %v %v", ok, msgs)
	}
	zs, ok := View[zoneSummary](snap, "zones")
	if !ok {
		t.Fatal("no zones view")
	}
	want := zoneSummary{"home": {"apps/nas", "apps/web"}, "lab": {"dns/ns1"}}
	if !reflect.DeepEqual(zs, want) {
		t.Fatalf("zones view %v, want %v", zs, want)
	}
	set := snap.Objects
	if set.Len() != 9 || set.Count(KindDNSRecordSet) != 4 || len(set.Zones()) != 2 || len(set.Policies()) != 1 ||
		len(set.ClientGroups()) != 1 || len(set.BlockLists()) != 1 {
		t.Fatalf("counts: len=%d rrsets=%d", set.Len(), set.Count(KindDNSRecordSet))
	}
	if got := set.ZoneRefs(); !reflect.DeepEqual(got, []string{"home", "lab", "missing"}) {
		t.Fatalf("ZoneRefs = %v", got)
	}
	if z, ok := set.Zone("lab"); !ok || z.Spec.ZoneName != "lab.example." {
		t.Fatalf("Zone(lab) = %v %v", z, ok)
	}
	if _, ok := set.Get(store.Key{Kind: KindPolicy, Namespace: "apps", Name: "kids"}); !ok {
		t.Fatal("Get(policy) missed")
	}
	if got := names(set.WithLabel(KindDNSRecordSet, "team", "infra")); !reflect.DeepEqual(got, []string{"apps/nas", "dns/ns1"}) {
		t.Fatalf("WithLabel = %v", got)
	}
	for _, v := range c.kinds {
		if testutil.ToFloat64(c.m.objects.WithLabelValues(string(v))) != float64(set.Count(v)) {
			t.Errorf("kudurru_zonecache_objects{kind=%s} disagrees with the snapshot", v)
		}
	}
	if testutil.ToFloat64(c.m.rebuilds) < 1 {
		t.Error("no rebuild counted")
	}

	// Live changes: update, add and delete, then one debounced rebuild.
	gen := snap.Generation
	apply(t, st, rrset("apps", "nas", "lab", map[string]string{"team": "infra"}), zone("work", "work.example."))
	if err := st.Delete(context.Background(), store.Key{Kind: KindDNSRecordSet, Namespace: "apps", Name: "web"}, store.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the changes", func() bool {
		zs, _ := View[zoneSummary](c.Snapshot(), "zones")
		return reflect.DeepEqual(zs, zoneSummary{"home": {}, "lab": {"apps/nas", "dns/ns1"}, "work": {}})
	})
	now := c.Snapshot()
	if now.Generation <= gen || !now.Objects.Changed(KindDNSRecordSet) || !now.Objects.Changed(KindDNSZone) {
		t.Fatalf("generation %d -> %d, changed rrsets=%v zones=%v", gen, now.Generation, now.Objects.Changed(KindDNSRecordSet), now.Objects.Changed(KindDNSZone))
	}
	if now.Objects.Changed(KindPolicy) {
		t.Error("Policy reported changed")
	}
	// Unchanged kinds share their index with the previous snapshot.
	if &now.Objects.List(KindPolicy)[0] != &snap.Objects.List(KindPolicy)[0] {
		t.Error("the Policy index was rebuilt although no Policy changed")
	}
}

func names(objs []store.Object) []string {
	out := make([]string, len(objs))
	for i, o := range objs {
		out[i] = o.GetNamespace() + "/" + o.GetName()
	}
	return out
}

func TestDebounce(t *testing.T) {
	const window = 200 * time.Millisecond
	st := memory.New()
	c := newCache(t, st, Options{Debounce: window})
	// The test waits on what the publish hook saw, not on c.Snapshot(): the
	// hooks run after the snapshot is stored, so a count read as soon as
	// Snapshot shows a change can miss the rebuild that published it (under
	// load the burst's rebuild was then counted with the spaced changes).
	var (
		mu        sync.Mutex
		published []*Snapshot
	)
	c.OnPublish(func(s *Snapshot) {
		mu.Lock()
		published = append(published, s)
		mu.Unlock()
	})
	since := func(i int) []*Snapshot {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(published[i:])
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(published)
	}
	// waitPublished waits for a published snapshot satisfying cond and
	// returns the snapshots published from index from up to that one.
	waitPublished := func(from int, what string, cond func(*Snapshot) bool) []*Snapshot {
		t.Helper()
		var out []*Snapshot
		eventually(t, 5*time.Second, what, func() bool {
			for i, s := range since(from) {
				if cond(s) {
					out = since(from)[:i+1]
					return true
				}
			}
			return false
		})
		return out
	}
	startCache(t, c)
	waitPublished(0, "a Live snapshot", func(s *Snapshot) bool { return s.State == Live })
	base := count()

	// A burst well inside one window folds into one rebuild. The margin
	// (half the window) leaves room for the store's asynchronous watch
	// delivery under load.
	start := time.Now()
	for i := range 50 {
		apply(t, st, rrset("apps", "r"+itoa(i), "home", nil))
	}
	burst := time.Since(start)
	snaps := waitPublished(base, "50 record sets", func(s *Snapshot) bool { return s.Objects.Count(KindDNSRecordSet) == 50 })
	visible := time.Since(start)
	if got := len(snaps); burst < window/2 && got != 1 {
		t.Errorf("a %s burst of 50 changes produced %d rebuilds, want 1", burst, got)
	} else if got > 3 {
		t.Errorf("%d rebuilds for one burst", got)
	}
	if visible < window {
		t.Errorf("changes visible after %s, before the %s debounce window", visible, window)
	}

	// Two changes further apart than the window are two rebuilds: zone b
	// arrives a full window after zone a was published. Only rebuilds that
	// changed the zone count are counted: a change can reach a rebuild
	// after its kick was drained and leave one no-op rebuild behind.
	base = count()
	apply(t, st, zone("a", "a.example."))
	waitPublished(base, "zone a", func(s *Snapshot) bool { return s.Objects.Count(KindDNSZone) == 1 })
	time.Sleep(window)
	apply(t, st, zone("b", "b.example."))
	snaps = waitPublished(base, "zone b", func(s *Snapshot) bool { return s.Objects.Count(KindDNSZone) == 2 })
	got, zones := 0, 0
	for _, s := range snaps {
		if n := s.Objects.Count(KindDNSZone); n != zones {
			got, zones = got+1, n
		}
	}
	if got != 2 {
		t.Errorf("two spaced changes produced %d rebuilds, want 2", got)
	}
}

func itoa(i int) string {
	const digits = "0123456789"
	if i < 10 {
		return digits[i : i+1]
	}
	return itoa(i/10) + digits[i%10:i%10+1]
}

func TestBuilderErrorIsolation(t *testing.T) {
	st := memory.New()
	apply(t, st, zone("home", "home.example."))
	reg := prometheus.NewRegistry()
	c := newCache(t, st, Options{Registerer: reg, Debounce: 10 * time.Millisecond})
	var mode atomic.Value // "ok" | "error" | "panic"
	mode.Store("ok")
	if err := c.Register(NewBuilder("flaky", func(s ObjectSet) (any, error) {
		switch mode.Load() {
		case "error":
			return nil, errors.New("bad record data")
		case "panic":
			panic("boom")
		}
		return s.Count(KindDNSZone), nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := c.Register(NewBuilder("good", func(s ObjectSet) (any, error) { return s.Count(KindDNSZone), nil })); err != nil {
		t.Fatal(err)
	}
	startCache(t, c)
	waitState(t, c, Live)
	viewIs := func(name string, n int) func() bool {
		return func() bool { v, ok := View[int](c.Snapshot(), name); return ok && v == n }
	}
	eventually(t, 5*time.Second, "both views", func() bool { return viewIs("flaky", 1)() && viewIs("good", 1)() })

	for i, m := range []string{"error", "panic"} {
		mode.Store(m)
		apply(t, st, zone("z"+itoa(i), "z"+itoa(i)+".example."))
		eventually(t, 5*time.Second, "the good view to move on ("+m+")", viewIs("good", 2+i))
		snap := c.Snapshot()
		if v, ok := View[int](snap, "flaky"); !ok || v != 1 {
			t.Fatalf("%s: flaky view = %v %v, want the previous value 1", m, v, ok)
		}
		conds := snap.Degraded()
		if len(conds) != 1 || conds[0].Reason != ReasonBuildFailed || conds[0].Builder != "flaky" {
			t.Fatalf("%s: degraded = %v", m, conds)
		}
		if m == "panic" && !strings.Contains(conds[0].Message, "panic") {
			t.Errorf("panic not reported: %q", conds[0].Message)
		}
		ok, msgs := c.Ready()
		if !ok || len(msgs) != 1 || !strings.Contains(msgs[0], "BuildFailed") {
			t.Fatalf("%s: Ready() = %v %v; a builder failure degrades but keeps readiness", m, ok, msgs)
		}
	}
	if got := testutil.ToFloat64(c.m.buildErrors.WithLabelValues("flaky")); got < 2 {
		t.Errorf("kudurru_zonecache_build_errors_total{builder=flaky} = %v", got)
	}
	mode.Store("ok")
	apply(t, st, zone("z9", "z9.example."))
	eventually(t, 5*time.Second, "recovery", viewIs("flaky", 4))
	if conds := c.Degraded(); len(conds) != 0 {
		t.Fatalf("conditions after recovery: %v", conds)
	}
}

func TestBuilderNeverSucceeded(t *testing.T) {
	st := memory.New()
	c := newCache(t, st, Options{Debounce: time.Millisecond})
	if err := c.Register(NewBuilder("broken", func(ObjectSet) (any, error) { return nil, errors.New("nope") })); err != nil {
		t.Fatal(err)
	}
	startCache(t, c)
	snap := waitState(t, c, Live)
	if _, ok := snap.View("broken"); ok {
		t.Fatal("a builder that never succeeded has a view")
	}
	if _, ok := View[int](snap, "broken"); ok {
		t.Fatal("View[int] of a failed builder reported ok")
	}
}

func TestLateRegistrationAndSetLists(t *testing.T) {
	st := memory.New()
	apply(t, st, policy("apps", "kids", "ads"))
	c := newCache(t, st, Options{Debounce: time.Millisecond})
	startCache(t, c)
	waitState(t, c, Live)

	var mu sync.Mutex
	var seen []bool
	if err := c.Register(NewBuilder("policies", func(s ObjectSet) (any, error) {
		mu.Lock()
		seen = append(seen, s.ListsChanged())
		mu.Unlock()
		if s.Lists() == nil {
			return 0, nil
		}
		return len(s.Lists().Lists()), nil
	})); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the late builder's first view", func() bool { v, ok := View[int](c.Snapshot(), "policies"); return ok && v == 0 })

	l, err := blocking.Compile(blocking.Spec{Name: "ads", Mode: blocking.ModeBlock,
		Sources: []blocking.Source{blocking.LinesSource("inline", parser.FormatDomains, []string{"ads.example"})}}, nil, blocking.CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lists, err := blocking.NewSnapshot(1, l)
	if err != nil {
		t.Fatal(err)
	}
	c.SetLists(lists)
	eventually(t, 5*time.Second, "the lists", func() bool { v, ok := View[int](c.Snapshot(), "policies"); return ok && v == 1 })
	if c.Snapshot().Objects.Lists() != lists {
		t.Fatal("ObjectSet.Lists is not the snapshot given to SetLists")
	}
	mu.Lock()
	defer mu.Unlock()
	if !seen[len(seen)-1] {
		t.Error("ListsChanged false on the rebuild SetLists caused")
	}
}

func TestResyncAfterWatchClose(t *testing.T) {
	st := newFakeStore()
	st.put(zone("home", "home.example."), true)
	st.put(rrset("apps", "keep", "home", nil), true)
	st.put(rrset("apps", "gone", "home", nil), true)
	reg := prometheus.NewRegistry()
	c := newCache(t, st, Options{Registerer: reg, Debounce: time.Millisecond})
	if err := c.Register(zonesBuilder()); err != nil {
		t.Fatal(err)
	}
	startCache(t, c)
	waitState(t, c, Live)
	eventually(t, 5*time.Second, "the initial view", func() bool {
		zs, _ := View[zoneSummary](c.Snapshot(), "zones")
		return reflect.DeepEqual(zs, zoneSummary{"home": {"apps/gone", "apps/keep"}})
	})

	// The watch breaks and changes happen that it never reports.
	st.breakWatches(KindDNSRecordSet)
	st.del(store.Key{Kind: KindDNSRecordSet, Namespace: "apps", Name: "gone"}, false)
	st.put(rrset("apps", "new", "home", nil), false)
	eventually(t, 5*time.Second, "the re-list", func() bool {
		zs, _ := View[zoneSummary](c.Snapshot(), "zones")
		return reflect.DeepEqual(zs, zoneSummary{"home": {"apps/keep", "apps/new"}})
	})
	if got := testutil.ToFloat64(c.m.resyncs.WithLabelValues(string(KindDNSRecordSet))); got != 1 {
		t.Errorf("kudurru_store_watch_resyncs_total{kind=DNSRecordSet} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(c.m.resyncs.WithLabelValues(string(KindDNSZone))); got != 0 {
		t.Errorf("resyncs{kind=DNSZone} = %v, want 0", got)
	}
	if c.State() != Live {
		t.Fatalf("state %s during resync", c.State())
	}
	// The re-established watch carries live events again.
	st.put(rrset("apps", "later", "home", nil), true)
	eventually(t, 5*time.Second, "a live event after the resync", func() bool {
		return len(c.Snapshot().Objects.RecordSetsForZone("home")) == 3
	})
}

func TestWatchErrorsAreRetried(t *testing.T) {
	st := newFakeStore()
	st.put(zone("home", "home.example."), true)
	st.failWatches = 7 // spread over the five kinds: some kinds retry twice
	c := newCache(t, st, Options{Debounce: time.Millisecond})
	startCache(t, c)
	snap := waitState(t, c, Live)
	if snap.Objects.Count(KindDNSZone) != 1 {
		t.Fatal("zone missing after retries")
	}
	total := 0
	for _, k := range c.kinds {
		total += st.calls(k)
	}
	if total < len(c.kinds)+7 {
		t.Errorf("%d Watch calls, want at least %d", total, len(c.kinds)+7)
	}
}

func TestDeletesDuringInitialList(t *testing.T) {
	st := newFakeStore()
	st.holdBookmarks()
	st.put(rrset("apps", "a", "home", nil), true)
	st.put(rrset("apps", "b", "home", nil), true)
	c := newCache(t, st, Options{Debounce: time.Millisecond, StartupTimeout: time.Hour})
	startCache(t, c)
	eventually(t, 5*time.Second, "both watches open", func() bool { return st.calls(KindDNSRecordSet) == 1 })
	st.del(store.Key{Kind: KindDNSRecordSet, Namespace: "apps", Name: "a"}, true)
	time.Sleep(20 * time.Millisecond)
	if c.State() != Unsynced || c.Snapshot().State != Unsynced {
		t.Fatal("live before the Bookmarks")
	}
	if ok, msgs := c.Ready(); ok || len(msgs) != 1 || !strings.Contains(msgs[0], "DNSRecordSet") {
		t.Fatalf("Ready() before sync = %v %v", ok, msgs)
	}
	st.releaseBookmarks()
	snap := waitState(t, c, Live)
	if got := names(snap.Objects.List(KindDNSRecordSet)); !slices.Equal(got, []string{"apps/b"}) {
		t.Fatalf("record sets %v", got)
	}
}

func TestExpiryTriggersRebuild(t *testing.T) {
	st := memory.New()
	rs := rrset("apps", "temp", "home", nil)
	// metav1.Time serialises with second precision: 1-2 s ahead.
	expires := time.Now().Truncate(time.Second).Add(2 * time.Second)
	rs.Spec.Endpoints[0].Kudurru = &v1alpha1.EndpointExt{ExpiresAt: &metav1.Time{Time: expires}}
	apply(t, st, rs)
	c := newCache(t, st, Options{Debounce: time.Millisecond})
	// The builder serves an endpoint only while it has not expired.
	if err := c.Register(NewBuilder("active", func(s ObjectSet) (any, error) {
		n := 0
		for _, r := range s.RecordSets() {
			for _, ep := range r.Spec.Endpoints {
				if ep.Kudurru == nil || ep.Kudurru.ExpiresAt == nil || s.Now().Before(ep.Kudurru.ExpiresAt.Time) {
					n++
				}
			}
		}
		return n, nil
	})); err != nil {
		t.Fatal(err)
	}
	startCache(t, c)
	waitState(t, c, Live)
	eventually(t, 5*time.Second, "the endpoint in service", func() bool { v, ok := View[int](c.Snapshot(), "active"); return ok && v == 1 })
	eventually(t, 5*time.Second, "the endpoint to expire without a store change", func() bool {
		v, _ := View[int](c.Snapshot(), "active")
		return v == 0
	})
	snap := c.Snapshot()
	if snap.Objects.Now().Before(expires) || !snap.Objects.Changed(KindDNSRecordSet) {
		t.Fatalf("expiry rebuild at %s (expiry %s), changed=%v", snap.Objects.Now(), expires, snap.Objects.Changed(KindDNSRecordSet))
	}
}

func TestRVMax(t *testing.T) {
	for _, tc := range []struct{ a, b, want string }{
		{"", "", ""},
		{"", "5", "5"},
		{"5", "", "5"},
		{"5", "7", "7"},
		{"7", "5", "7"},
		{"10", "9", "10"},
		{"x", "5", "5"},
		{"5", "x", "5"},
		{"x", "y", "x"},
	} {
		if got := rvMax(tc.a, tc.b); got != tc.want {
			t.Errorf("rvMax(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestStateString(t *testing.T) {
	for s, want := range map[State]string{Unsynced: "Unsynced", FromSnapshot: "FromSnapshot", Live: "Live", State(9): "State(?)"} {
		if s.String() != want {
			t.Errorf("%d.String() = %q", s, s.String())
		}
	}
}
