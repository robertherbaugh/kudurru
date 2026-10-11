package listfetcher

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

var testKey = bytes.Repeat([]byte{0x42}, 32)

// startInternal serves cache over the internal listener on a loopback port
// and returns its address.
func startInternal(t *testing.T, cache *DiskCache, status func(context.Context) internalapi.Status) (string, *internalapi.Credentials) {
	t.Helper()
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: testKey, NodeName: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := internalapi.NewServer(internalapi.ServerOptions{Credentials: creds, Lists: cache, Status: status, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return ln.Addr().String(), creds
}

type replica struct {
	cache *DiskCache
	sink  *sink
	r     *Resolver
}

func newReplica(t *testing.T, objs Objects, opts ResolverOptions) *replica {
	t.Helper()
	cache, err := OpenDiskCache(t.TempDir(), quiet())
	if err != nil {
		t.Fatal(err)
	}
	sk := &sink{}
	opts.Objects, opts.Cache, opts.Sink, opts.Logger = objs, cache, sk, quiet()
	if opts.Membership == nil {
		opts.Membership = &cluster.Static{SelfName: "node-b", LeaderName: "node-a", Members: []cluster.Peer{{Name: "node-a", Address: "127.0.0.1:1"}}}
	}
	if opts.Client == nil {
		creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: testKey, NodeName: "node-b"})
		if err != nil {
			t.Fatal(err)
		}
		opts.Client = internalapi.NewClient(creds, internalapi.ClientOptions{Timeout: 2 * time.Second})
	}
	r, err := NewResolver(opts)
	if err != nil {
		t.Fatal(err)
	}
	return &replica{cache: cache, sink: sk, r: r}
}

// TestDigestPullBetweenTwoNodes: the leader fetches once and publishes the
// digest; the replica pulls the body over the internal listener by digest,
// verifies it and compiles it.
func TestDigestPullBetweenTwoNodes(t *testing.T) {
	fx := newFixture(t)
	srv := newListServer(t, hostsV1)
	applyList(t, fx.st, "ads", v1alpha1.ListSource{URL: srv.URL + "/hosts"}, v1alpha1.ListSource{Inline: []string{"inline.example"}})
	ctx := context.Background()
	fx.f.Reconcile(ctx)

	addr, _ := startInternal(t, fx.cache, nil)
	objs := newStoreObjects(fx.st)
	rep := newReplica(t, objs, ResolverOptions{
		Membership: &cluster.Static{SelfName: "node-b", LeaderName: "node-a", Members: []cluster.Peer{{Name: "node-a", Address: addr}}},
		Downloader: NewDownloader(nil, DownloadOptions{}), // must not be used
		Now:        fx.clk.Now,
	})
	res, err := rep.r.Resolve(ctx)
	if err != nil || res.Waiting != 0 || res.Pending != 0 || !res.Rebuilt {
		t.Fatalf("resolve = %+v, %v", res, err)
	}
	if srv.hits.Load() != 1 {
		t.Errorf("list host hit %d times; replicas must not download", srv.hits.Load())
	}
	e, ok := rep.cache.Lookup(srv.URL + "/hosts")
	if !ok || e.Origin != OriginLeader || e.Digest != sha([]byte(hostsV1)) {
		t.Fatalf("replica cache entry = %+v, %v", e, ok)
	}
	snap := rep.sink.last()
	if snap == nil || snap.List("default/ads") == nil {
		t.Fatal("no compiled list handed to the sink")
	}
	if got := snap.Contains("tracker.example"); got["default/ads"] == "" {
		t.Errorf("tracker.example not blocked: %v", got)
	}
	if got := snap.Contains("inline.example"); got["default/ads"] == "" {
		t.Errorf("inline source missing: %v", got)
	}
	if got := testutil.ToFloat64(rep.r.m.pulls.WithLabelValues(OriginLeader, "ok")); got != 1 {
		t.Errorf("pulls = %v", got)
	}
	if ls := rep.r.Lists(); len(ls) != 1 || len(ls[0].Digests) != 2 || ls[0].Digests[0] != e.Digest {
		t.Errorf("Lists() = %+v", ls)
	}
	if ok, _ := rep.r.Ready(); !ok {
		t.Error("not ready after a complete load")
	}

	// Unchanged inputs: no second compile.
	if res, _ := rep.r.Resolve(ctx); res.Rebuilt || rep.sink.count() != 1 {
		t.Errorf("unchanged pass rebuilt (%d snapshots)", rep.sink.count())
	}

	// The leader refreshes to v2: the replica follows by digest.
	srv.set("", hostsV2, `"v2"`)
	fx.clk.Advance(2 * time.Hour)
	fx.f.Reconcile(ctx)
	if res, _ := rep.r.Resolve(ctx); !res.Rebuilt {
		t.Fatal("new digest not picked up")
	}
	if got := rep.sink.last().Contains("new.example"); got["default/ads"] == "" {
		t.Errorf("v2 not compiled: %v", got)
	}
	if srv.hits.Load() != 2 {
		t.Errorf("list host hit %d times", srv.hits.Load())
	}
}

// TestFallbackTiming: without a reachable leader a replica serves its disk
// cache and downloads itself only after FallbackAfter (fake clock).
func TestFallbackTiming(t *testing.T) {
	clk := newClock()
	st := memory.New()
	srv := newListServer(t, hostsV1)
	bl := applyList(t, st, "ads", v1alpha1.ListSource{URL: srv.URL})
	// The (now unreachable) leader published a digest.
	bl.Status.Sources = []v1alpha1.SourceStatus{{Name: srv.URL, Digest: sha([]byte(hostsV1))}}
	if err := st.UpdateStatus(context.Background(), bl); err != nil {
		t.Fatal(err)
	}
	var dlCache *DiskCache
	rep := newReplica(t, newStoreObjects(st), ResolverOptions{Now: clk.Now, InitStrategy: InitBlocking})
	dlCache = rep.cache
	dl := NewDownloader(dlCache, DownloadOptions{Now: clk.Now, AllowLocalAddresses: true})
	dl.sleep = func(context.Context, time.Duration) error { return nil }
	rep.r.opts.Downloader = dl
	ctx := context.Background()

	for _, step := range []struct {
		advance time.Duration
		hits    int64
		waiting int
	}{
		{0, 0, 1},
		{5 * time.Minute, 0, 1},
		{4*time.Minute + 59*time.Second, 0, 1},
		{time.Second, 1, 0}, // 10 minutes without a reachable leader
		{time.Minute, 1, 0}, // served from the self-fetched body, no refetch
	} {
		clk.Advance(step.advance)
		res, err := rep.r.Resolve(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if srv.hits.Load() != step.hits || res.Waiting != step.waiting {
			t.Fatalf("after +%v: hits=%d waiting=%d, want %d/%d", step.advance, srv.hits.Load(), res.Waiting, step.hits, step.waiting)
		}
		if ready, msgs := rep.r.Ready(); ready != (step.waiting == 0) {
			t.Fatalf("after +%v: ready=%v %v", step.advance, ready, msgs)
		}
	}
	e, _ := rep.cache.Lookup(srv.URL)
	if e.Origin != OriginSelf {
		t.Errorf("origin = %q", e.Origin)
	}
	if got := rep.sink.last().Contains("ads.example"); got["default/ads"] == "" {
		t.Error("self-fetched list not compiled")
	}
	if got := testutil.ToFloat64(rep.r.m.pulls.WithLabelValues(OriginSelf, "ok")); got != 1 {
		t.Errorf("self pulls = %v", got)
	}
}

// TestNoLeaderNoDigest: no leader known and nothing published (a replica
// started before the leader ever fetched) also falls back after the delay.
func TestNoLeaderNoDigest(t *testing.T) {
	clk := newClock()
	st := memory.New()
	srv := newListServer(t, hostsV1)
	applyList(t, st, "ads", v1alpha1.ListSource{URL: srv.URL})
	rep := newReplica(t, newStoreObjects(st), ResolverOptions{Now: clk.Now, Membership: &cluster.Static{SelfName: "b"}})
	dl := NewDownloader(rep.cache, DownloadOptions{Now: clk.Now, AllowLocalAddresses: true})
	rep.r.opts.Downloader = dl
	ctx := context.Background()
	if res, _ := rep.r.Resolve(ctx); res.Waiting != 1 {
		t.Fatalf("waiting = %d", res.Waiting)
	}
	if _, msgs := rep.r.Ready(); !strings.Contains(strings.Join(msgs, ";"), "publish a digest") {
		t.Errorf("ready messages = %v", msgs)
	}
	clk.Advance(DefaultFallbackAfter)
	if res, _ := rep.r.Resolve(ctx); res.Waiting != 0 || srv.hits.Load() != 1 {
		t.Fatalf("after fallback: %+v, hits %d", res, srv.hits.Load())
	}
}

func TestInitStrategies(t *testing.T) {
	st := memory.New()
	applyList(t, st, "ads", v1alpha1.ListSource{URL: "https://lists.invalid/hosts"})
	for _, tc := range []struct {
		strategy      InitStrategy
		allowDegraded bool
		ready         bool
	}{
		{InitFast, false, true},
		{InitBlocking, false, false},
		{InitBlocking, true, true},
		{InitFailOnError, false, false},
	} {
		rep := newReplica(t, newStoreObjects(st), ResolverOptions{InitStrategy: tc.strategy, AllowDegraded: tc.allowDegraded})
		if _, err := rep.r.Resolve(context.Background()); err != nil {
			t.Fatal(err)
		}
		if ready, msgs := rep.r.Ready(); ready != tc.ready {
			t.Errorf("%s allowDegraded=%v: ready=%v %v", tc.strategy, tc.allowDegraded, ready, msgs)
		}
		select {
		case <-rep.r.Loaded():
			t.Errorf("%s: loaded while a source is missing", tc.strategy)
		default:
		}
	}

	// failOnError: a definitive failure during the initial load is fatal.
	st2 := memory.New()
	applyList(t, st2, "cm", v1alpha1.ListSource{ConfigMapRef: &v1alpha1.ObjectRef{Name: "extra"}})
	rep := newReplica(t, newStoreObjects(st2), ResolverOptions{InitStrategy: InitFailOnError})
	if _, err := rep.r.Resolve(context.Background()); !errors.Is(err, ErrInitialLoad) {
		t.Errorf("failOnError: %v", err)
	}
	// blocking: the same failure completes the load (ready, reported).
	rep = newReplica(t, newStoreObjects(st2), ResolverOptions{InitStrategy: InitBlocking})
	if _, err := rep.r.Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ready, msgs := rep.r.Ready(); !ready || !strings.Contains(strings.Join(msgs, ";"), "configMapRef") {
		t.Errorf("blocking after a definitive failure: %v %v", ready, msgs)
	}
	if _, err := ParseInitStrategy("eventually"); err == nil {
		t.Error("unknown strategy accepted")
	}
}

// TestDiskSeedAndGC: a restarted node serves its cached body before any
// network pass, and entries no list references are removed.
func TestDiskSeedAndGC(t *testing.T) {
	st := memory.New()
	bl := applyList(t, st, "ads", v1alpha1.ListSource{URL: "https://lists.example/a"})
	rep := newReplica(t, newStoreObjects(st), ResolverOptions{})
	for key, body := range map[string]string{"https://lists.example/a": hostsV1, "https://lists.example/gone": hostsV2} {
		if _, _, err := rep.cache.Put(key, strings.NewReader(body), PutOptions{Origin: OriginLeader}); err != nil {
			t.Fatal(err)
		}
	}
	// A newer digest is published; the leader is unreachable.
	bl.Status.Sources = []v1alpha1.SourceStatus{{Name: "https://lists.example/a", Digest: sha([]byte("newer"))}}
	if err := st.UpdateStatus(context.Background(), bl); err != nil {
		t.Fatal(err)
	}
	rep.r.pass(context.Background(), true) // what Run does first
	if got := rep.sink.last().Contains("ads.example"); got["default/ads"] == "" {
		t.Fatal("cached body not served on the offline pass")
	}
	if _, ok := rep.cache.Lookup("https://lists.example/gone"); !ok {
		t.Fatal("offline pass removed cache entries")
	}
	res, _ := rep.r.Resolve(context.Background())
	if res.Pending != 1 || res.Waiting != 0 {
		t.Errorf("stale body: %+v", res)
	}
	if ready, msgs := rep.r.Ready(); !ready || !strings.Contains(strings.Join(msgs, ";"), "older body") {
		t.Errorf("ready = %v %v", ready, msgs)
	}
	if _, ok := rep.cache.Lookup("https://lists.example/gone"); ok {
		t.Error("unreferenced cache entry kept")
	}
}

// TestSetListsZonecache runs the resolver against a real zonecache on the
// memory driver: lists reach ObjectSet.Lists and follow edits.
func TestSetListsZonecache(t *testing.T) {
	st := memory.New()
	zc, err := zonecache.New(st, zonecache.Options{Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	objs := NewZoneCacheObjects(zc)
	cache, err := OpenDiskCache(t.TempDir(), quiet())
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewResolver(ResolverOptions{Objects: objs, Cache: cache, Sink: zc, Logger: quiet(), InitStrategy: InitBlocking})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 2)
	go func() { errs <- zc.Start(ctx) }()
	go func() { errs <- r.Run(ctx) }()

	applyInline := func(lines ...string) {
		bl := &v1alpha1.BlockList{
			ObjectMeta: metav1.ObjectMeta{Name: "kids", Namespace: "home"},
			Spec:       v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{Inline: lines}}},
		}
		if _, err := st.Apply(ctx, bl, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor := func(what string, cond func(*zonecache.Snapshot) bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if cond(zc.Snapshot()) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	applyInline("||games.example^")
	waitFor("the first list", func(s *zonecache.Snapshot) bool {
		l := s.Objects.Lists()
		return l != nil && l.Contains("www.games.example")["home/kids"] != ""
	})
	<-r.Loaded()
	if ok, _ := r.Ready(); !ok {
		t.Error("not ready after the load")
	}
	applyInline("||games.example^", "||video.example^")
	waitFor("the edited list", func(s *zonecache.Snapshot) bool {
		return s.Objects.Lists().Contains("video.example")["home/kids"] != ""
	})
	if err := st.Delete(ctx, store.Key{Kind: "BlockList", Namespace: "home", Name: "kids"}, store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	waitFor("the deletion", func(s *zonecache.Snapshot) bool { return s.Objects.Lists().List("home/kids") == nil })
	cancel()
	for range 2 {
		if err := <-errs; err != nil {
			t.Errorf("runnable: %v", err)
		}
	}
}

// TestDockerShape: one node that is its own leader (cluster.Self): the
// fetcher and the resolver share the cache and no pull ever happens.
func TestDockerShape(t *testing.T) {
	fx := newFixture(t)
	srv := newListServer(t, hostsV1)
	applyList(t, fx.st, "ads", v1alpha1.ListSource{URL: srv.URL})
	sk := &sink{}
	r, err := NewResolver(ResolverOptions{
		Objects: newStoreObjects(fx.st), Cache: fx.cache, Sink: sk, Logger: quiet(),
		Membership: cluster.Self{Name: "solo"}, Downloader: fx.dl, Now: fx.clk.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Before the fetcher ran there is nothing to serve, and the resolver
	// never downloads on the leader itself even after the fallback delay.
	if res, _ := r.Resolve(ctx); res.Waiting != 1 {
		t.Fatalf("before fetch: %+v", res)
	}
	fx.clk.Advance(time.Hour)
	if res, _ := r.Resolve(ctx); res.Waiting != 1 || srv.hits.Load() != 0 {
		t.Fatalf("the leader's resolver downloaded on its own: %+v, hits %d", res, srv.hits.Load())
	}
	fx.f.Reconcile(ctx)
	if res, _ := r.Resolve(ctx); res.Waiting != 0 || res.Pending != 0 {
		t.Fatalf("after fetch: %+v", res)
	}
	if srv.hits.Load() != 1 {
		t.Errorf("hits %d", srv.hits.Load())
	}
	if sk.last().Contains("ads.example")["default/ads"] == "" {
		t.Error("list not compiled")
	}
}
