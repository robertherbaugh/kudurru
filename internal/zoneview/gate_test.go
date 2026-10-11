package zoneview

import (
	"context"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/controller"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

func TestGateMapping(t *testing.T) {
	for _, tc := range []struct {
		snap *zonecache.Snapshot
		want zone.Gate
	}{
		{nil, zone.GateUnsynced},
		{&zonecache.Snapshot{State: zonecache.Unsynced}, zone.GateUnsynced},
		{&zonecache.Snapshot{State: zonecache.FromSnapshot}, zone.GateOpen},
		{&zonecache.Snapshot{State: zonecache.FromSnapshot, Stale: true}, zone.GateStale},
		{&zonecache.Snapshot{State: zonecache.Live}, zone.GateOpen},
	} {
		if got := Gate(tc.snap); got != tc.want {
			t.Errorf("%+v: %s, want %s", tc.snap, got, tc.want)
		}
	}
}

// silentStore never syncs: its watches deliver nothing, as an apiserver
// that is down would.
type silentStore struct{ store.Store }

func (silentStore) Watch(context.Context, store.Kind, store.WatchOptions) (store.Watcher, error) {
	return &silentWatcher{ch: make(chan store.Event)}, nil
}

type silentWatcher struct {
	ch   chan store.Event
	once sync.Once
}

func (w *silentWatcher) ResultChan() <-chan store.Event { return w.ch }
func (w *silentWatcher) Stop()                          { w.once.Do(func() { close(w.ch) }) }

func objects() []store.Object {
	home := &v1alpha1.DNSZone{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "DNSZone"},
		ObjectMeta: metav1.ObjectMeta{Name: "home", Generation: 1},
		Spec: v1alpha1.DNSZoneSpec{ZoneName: "home.example.", Type: v1alpha1.ZoneTypePrimary,
			Nameservers: []string{"ns1.home.example."}},
		Status: v1alpha1.DNSZoneStatus{Serial: 5},
	}
	corp := &v1alpha1.DNSZone{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "DNSZone"},
		ObjectMeta: metav1.ObjectMeta{Name: "corp", Generation: 1},
		Spec: v1alpha1.DNSZoneSpec{ZoneName: "corp.example.", Type: v1alpha1.ZoneTypeForward,
			Forward: &v1alpha1.ForwardSpec{UpstreamGroup: "corp"}},
	}
	nas := &v1alpha1.DNSRecordSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "DNSRecordSet"},
		ObjectMeta: metav1.ObjectMeta{Name: "nas", Namespace: "apps", Generation: 1},
		Spec: v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: "home"},
			Endpoints: []v1alpha1.Endpoint{{DNSName: "nas.home.example", RecordType: "A", Targets: []string{"10.0.0.20"}}}},
	}
	return []store.Object{home, corp, nas}
}

// TestStaleSnapshotGatesPrimaryOnly is D80 end to end: a pod restarted
// while the store is down serves from local.db; once the snapshot is older
// than maxAge, names under primary zones answer SERVFAIL (never NXDOMAIN,
// never forwarded) while forward zones and other names keep forwarding.
func TestStaleSnapshotGatesPrimaryOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := localdb.OpenSnapshotStore(ctx, filepath.Join(t.TempDir(), localdb.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Write(ctx, &localdb.Snapshot{Objects: objects(), WrittenAt: time.Now().Add(-25 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	mem := memory.New()
	defer func() { _ = mem.Close() }()
	zc, err := zonecache.New(silentStore{mem}, zonecache.Options{LocalDB: db, StartupTimeout: 20 * time.Millisecond, MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := zc.Register(Builder(nil)); err != nil {
		t.Fatal(err)
	}
	src := Source{Cache: zc}
	if g := src.Zones().Gate(); g != zone.GateUnsynced {
		t.Fatalf("before start: %s", g)
	}
	go func() { _ = zc.Start(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for src.Zones().Gate() != zone.GateStale {
		if time.Now().After(deadline) {
			t.Fatalf("gate %s, state %s", src.Zones().Gate(), zc.State())
		}
		time.Sleep(10 * time.Millisecond)
	}

	st := authoritative.New(src)
	for _, tc := range []struct {
		qname  string
		rcode  int
		status pipeline.Status
		next   bool
		group  string
	}{
		{"nas.home.example.", dns.RcodeServerFailure, pipeline.StatusServfailUnsynced, false, ""},
		{"nope.home.example.", dns.RcodeServerFailure, pipeline.StatusServfailUnsynced, false, ""},
		{"app.corp.example.", dns.RcodeSuccess, pipeline.StatusForwarded, true, "corp"},
		{"www.example.net.", dns.RcodeSuccess, pipeline.StatusForwarded, true, ""},
	} {
		m := new(dns.Msg)
		m.SetQuestion(tc.qname, dns.TypeA)
		req := &pipeline.Request{Msg: m, Client: pipeline.Client{Addr: netip.MustParseAddrPort("10.0.0.9:5000")}}
		var called *pipeline.Request
		resp, err := st.Serve(req, func(r *pipeline.Request) (*pipeline.Response, error) {
			called = r
			reply := pipeline.Reply(r, dns.RcodeSuccess, pipeline.StatusForwarded)
			return reply, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.Msg.Rcode != tc.rcode || resp.Status != tc.status || (called != nil) != tc.next {
			t.Errorf("%s: %s/%s next=%v", tc.qname, dns.RcodeToString[resp.Msg.Rcode], resp.Status, called != nil)
		}
		if called != nil && called.UpstreamGroup != tc.group {
			t.Errorf("%s: group %q, want %q", tc.qname, called.UpstreamGroup, tc.group)
		}
		if tc.status == pipeline.StatusServfailUnsynced && resp.Upstream != authoritative.UpstreamStale {
			t.Errorf("%s: upstream %q", tc.qname, resp.Upstream)
		}
	}
}

// TestContentHashParity runs the leader's controller and the zone cache
// over one store and checks that status.contentHash is exactly the hash
// of the zone the cache serves, through selector exclusions, invalid and
// duplicate record sets, CNAME and DNAME conflicts, disabled endpoints
// and a spec change.
func TestContentHashParity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := memory.New()
	defer func() { _ = st.Close() }()
	apply := func(o store.Object) {
		t.Helper()
		if _, err := st.Apply(ctx, o, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	dz := &v1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "home"},
		Spec: v1alpha1.DNSZoneSpec{ZoneName: "home.example.", Type: v1alpha1.ZoneTypePrimary, DefaultTTL: 600,
			Nameservers: []string{"ns1.home.example."}, SOA: &v1alpha1.SOASpec{RName: "admin@home.example", Minimum: 60},
			RecordSetSelector: &v1alpha1.RecordSetSelector{Namespaces: []string{"apps", "infra"}}},
	}
	apply(dz)
	rs := func(ns, name string, created time.Duration, eps ...v1alpha1.Endpoint) *v1alpha1.DNSRecordSet {
		return &v1alpha1.DNSRecordSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(time.Now().Add(created))},
			Spec:       v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: "home"}, Endpoints: eps},
		}
	}
	e := func(name, typ string, targets ...string) v1alpha1.Endpoint {
		return v1alpha1.Endpoint{DNSName: name, RecordType: typ, Targets: targets}
	}
	off := e("off.home.example", "A", "10.0.0.99")
	off.Kudurru = &v1alpha1.EndpointExt{Disabled: true}
	geo := e("geo.home.example", "A", "10.0.0.7")
	geo.SetIdentifier = "eu"
	for _, o := range []*v1alpha1.DNSRecordSet{
		rs("infra", "ns", -time.Hour, e("ns1.home.example", "A", "10.0.0.1"), e("dn.home.example", "DNAME", "example.net"), off),
		rs("apps", "web", -time.Minute, e("www.home.example", "CNAME", "web.home.example"), e("web.home.example", "A", "10.0.0.2"), geo),
		rs("apps", "late", 0, e("www.home.example", "A", "10.0.0.3"), e("x.dn.home.example", "A", "10.0.0.4"),
			e("off.home.example", "A", "10.0.0.5"), e("outside.example.net", "A", "10.0.0.6"), e("ok.home.example", "TXT", "fine")),
		rs("apps", "broken", 0, e("bad.home.example", "A", "nope")),
		rs("other", "unselected", 0, e("u.home.example", "A", "10.0.0.8")),
	} {
		apply(o)
	}

	ctrl, err := controller.New(st, controller.Options{ZoneCoalesce: -1})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = ctrl.Run(ctx) }()
	zc, err := zonecache.New(st, zonecache.Options{Debounce: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := zc.Register(Builder(nil)); err != nil {
		t.Fatal(err)
	}
	go func() { _ = zc.Start(ctx) }()
	src := Source{Cache: zc}

	parity := func(what string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			var cur v1alpha1.DNSZone
			if err := st.Get(ctx, store.Key{Kind: controller.KindDNSZone, Name: "home"}, &cur); err != nil {
				t.Fatal(err)
			}
			set := src.Zones()
			if z := set.Match("home.example."); set.Synced() && z != nil && cur.Status.ContentHash != "" &&
				z.ContentHash() == cur.Status.ContentHash && z.RecordCount() == cur.Status.RecordCount && z.Serial() == cur.Status.Serial {
				return
			}
			if time.Now().After(deadline) {
				z := set.Match("home.example.")
				t.Fatalf("%s: status %s/%d/%d, served %v", what, cur.Status.ContentHash, cur.Status.RecordCount, cur.Status.Serial, z)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	parity("initial")

	z := src.Zones().Match("home.example.")
	for name, kind := range map[string]zone.Kind{
		"web.home.example.": zone.Success, "geo.home.example.": zone.Success, "ok.home.example.": zone.Success,
		"off.home.example.": zone.NXDomain, "u.home.example.": zone.NXDomain, "bad.home.example.": zone.NXDomain,
	} {
		// Served names exist (ok.home.example. holds only TXT: NODATA for
		// A); names left out are NXDOMAIN.
		if r := z.Lookup(name, dns.TypeA); (r.Kind == zone.NXDomain) != (kind == zone.NXDomain) {
			t.Errorf("%s: %s, want %s", name, r.Kind, kind)
		}
	}

	// A spec-only change (the SOA minimum) moves both.
	var cur v1alpha1.DNSZone
	if err := st.Get(ctx, store.Key{Kind: controller.KindDNSZone, Name: "home"}, &cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.SOA.Minimum = 120
	apply(&cur)
	parity("after an SOA change")
}
