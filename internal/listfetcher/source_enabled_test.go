package listfetcher

import (
	"context"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

func off() *bool { b := false; return &b }

// TestDisabledSourceFetcher: a source with enabled: false is not
// downloaded (not even when due or failing), keeps its cache entry and the
// digest and counts of its last fetch in status, reports enabled: false
// and no error, and leaves the list's entries.
func TestDisabledSourceFetcher(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	srv := newListServer(t, hostsV1)
	url := srv.URL + "/hosts"
	inline := v1alpha1.ListSource{Name: "mine", Inline: []string{"||inline.example^"}}
	applyList(t, fx.st, "ads", v1alpha1.ListSource{URL: url}, inline)
	fx.f.Reconcile(ctx)
	before := getList(t, fx.st, "ads").Status
	if before.Entries != 3 || before.Sources[0].Entries != 3 || srv.hits.Load() != 1 {
		t.Fatalf("first fetch: %+v (hits %d)", before, srv.hits.Load())
	}

	applyList(t, fx.st, "ads", v1alpha1.ListSource{URL: url, Enabled: off()}, inline)
	srv.set("500", "", "")
	fx.clk.Advance(2 * time.Hour) // due, and the host now fails
	fx.f.Reconcile(ctx)
	if err := fx.f.RefreshNow("default", "ads"); err != nil {
		t.Fatal(err)
	}
	fx.f.Reconcile(ctx)
	if srv.hits.Load() != 1 {
		t.Errorf("a disabled source was fetched %d times", srv.hits.Load()-1)
	}
	s := getList(t, fx.st, "ads").Status
	src := s.Sources[0]
	if src.Enabled == nil || *src.Enabled || src.Error != "" || src.Digest != before.Sources[0].Digest ||
		src.ETag != before.Sources[0].ETag || src.Entries != 3 || src.LastFetched == nil {
		t.Errorf("disabled source status = %+v", src)
	}
	if s.Sources[1].Enabled != nil {
		t.Errorf("enabled source carries enabled: %v", *s.Sources[1].Enabled)
	}
	if s.Entries != 1 {
		t.Errorf("list entries = %d, want only the inline source's 1", s.Entries)
	}
	if _, ok := fx.cache.Lookup(url); !ok {
		t.Error("the disabled source's cache entry was dropped")
	}

	// Switched back on: fetched again (the host is healthy again).
	srv.set("", "", "")
	applyList(t, fx.st, "ads", v1alpha1.ListSource{URL: url}, inline)
	fx.f.Reconcile(ctx)
	s = getList(t, fx.st, "ads").Status
	if srv.hits.Load() != 2 || s.Sources[0].Enabled != nil || s.Entries != 3 || s.Sources[0].Error != "" {
		t.Errorf("re-enabled: hits %d, status %+v", srv.hits.Load(), s)
	}
}

// TestDisabledSourceResolver: the resolver compiles only enabled sources
// and keeps the cache entry of a disabled one on a live pass; a list whose
// every source is disabled is loaded with 0 entries.
func TestDisabledSourceResolver(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	srv := newListServer(t, hostsV1)
	url := srv.URL + "/hosts"
	applyList(t, fx.st, "ads", v1alpha1.ListSource{URL: url}, v1alpha1.ListSource{Name: "mine", Inline: []string{"||inline.example^"}})
	fx.f.Reconcile(ctx)
	sk := &sink{}
	r, err := NewResolver(ResolverOptions{Objects: newStoreObjects(fx.st), Cache: fx.cache, Sink: sk, Logger: quiet(), Now: fx.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(ctx); err != nil {
		t.Fatal(err)
	}
	if l := sk.last().List("default/ads"); l == nil || l.Stats().Entries != 3 {
		t.Fatalf("before: %+v", sk.last().Lists())
	}

	applyList(t, fx.st, "ads", v1alpha1.ListSource{URL: url, Enabled: off()}, v1alpha1.ListSource{Name: "mine", Inline: []string{"||inline.example^"}})
	if _, err := r.Resolve(ctx); err != nil {
		t.Fatal(err)
	}
	snap := sk.last()
	if got := snap.Contains("tracker.example"); got["default/ads"] != "" {
		t.Errorf("a disabled source still matches: %v", got)
	}
	if got := snap.Contains("inline.example"); got["default/ads"] == "" {
		t.Errorf("the enabled source no longer matches: %v", got)
	}
	if _, ok := fx.cache.Lookup(url); !ok {
		t.Error("the live pass collected the disabled source's cache entry")
	}

	applyList(t, fx.st, "ads", v1alpha1.ListSource{URL: url, Enabled: off()}, v1alpha1.ListSource{Name: "mine", Inline: []string{"||inline.example^"}, Enabled: off()})
	res, err := r.Resolve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	l := sk.last().List("default/ads")
	if l == nil || l.Stats().Entries != 0 || res.Waiting != 0 || res.Pending != 0 {
		t.Errorf("all disabled: list %v, pass %+v", l, res)
	}
	fx.f.Reconcile(ctx)
	if s := getList(t, fx.st, "ads").Status; s.Entries != 0 || len(s.Sources) != 2 || s.Sources[0].Error != "" {
		t.Errorf("all disabled status = %+v", s)
	}
}

// forwarder is the terminal stage of the chain test: every query it sees
// was let through.
type forwarder struct{ seen []string }

func (f *forwarder) Name() string { return "upstream" }

func (f *forwarder) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	f.seen = append(f.seen, req.Question().Name)
	m := new(dns.Msg)
	m.SetReply(req.Msg)
	return &pipeline.Response{Msg: m, Status: pipeline.StatusForwarded, Upstream: "test"}, nil
}

// TestDisabledSourceChain is the end-to-end check: store → zonecache →
// resolver → policy index → identify and blocking stages. Switching one
// source of a referenced list off unblocks its domains while the list's
// other source keeps blocking; switching it on blocks them again.
func TestDisabledSourceChain(t *testing.T) {
	st := memory.New()
	zc, err := zonecache.New(st, zonecache.Options{Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	if err := zc.Register(policy.Builder(policy.Options{}, quiet())); err != nil {
		t.Fatal(err)
	}
	cache, err := OpenDiskCache(t.TempDir(), quiet())
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewResolver(ResolverOptions{Objects: NewZoneCacheObjects(zc), Cache: cache, Sink: zc, Logger: quiet(), RetryInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 2)
	go func() { errs <- zc.Start(ctx) }()
	go func() { errs <- r.Run(ctx) }()

	src := policy.Source{Cache: zc}
	bs, err := blocking.New(blocking.Options{Index: src})
	if err != nil {
		t.Fatal(err)
	}
	up := &forwarder{}
	chain, err := pipeline.New(identify.New(src, nil), bs, up)
	if err != nil {
		t.Fatal(err)
	}
	apply := func(obj store.Object) {
		t.Helper()
		if _, err := st.Apply(ctx, obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "default"} }
	apply(&v1alpha1.ClientGroup{ObjectMeta: meta("default"), Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{CIDR: "0.0.0.0/0"}}}})
	apply(&v1alpha1.Policy{ObjectMeta: meta("default"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}, BlockListRefs: []string{"ads"}}})
	list := func(adsOn bool) *v1alpha1.BlockList {
		ads := v1alpha1.ListSource{Name: "ads", Inline: []string{"||ads.example^"}}
		if !adsOn {
			ads.Enabled = off()
		}
		return &v1alpha1.BlockList{ObjectMeta: meta("ads"), Spec: v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{
			ads, {Name: "trackers", Inline: []string{"||tracker.example^"}},
		}}}
	}
	status := func(name string) pipeline.Status {
		t.Helper()
		req := &pipeline.Request{Msg: new(dns.Msg).SetQuestion(name, dns.TypeA),
			Client: pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.10:5353"), Proto: pipeline.ProtoUDP}}
		resp, err := chain.Serve(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Status
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	blocked := func(name string) func() bool {
		return func() bool { return status(name).Class() == pipeline.ClassBlocked }
	}

	apply(list(true))
	waitFor("ads.example blocked", blocked("ads.example."))
	waitFor("tracker.example blocked", blocked("tracker.example."))

	apply(list(false))
	waitFor("ads.example let through", func() bool { return !blocked("ads.example.")() })
	if !blocked("tracker.example.")() {
		t.Error("the enabled source stopped blocking")
	}
	if !slices.Contains(up.seen, "ads.example.") {
		t.Errorf("upstream saw %v", up.seen)
	}

	apply(list(true))
	waitFor("ads.example blocked again", blocked("ads.example."))
	cancel()
	for range 2 {
		if err := <-errs; err != nil {
			t.Errorf("runnable: %v", err)
		}
	}
}
