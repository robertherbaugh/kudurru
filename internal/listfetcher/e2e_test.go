package listfetcher

import (
	"context"
	"net"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// TestEndToEndTwoNodes wires both halves the way serve does, on two
// in-process nodes sharing one store: node A runs zonecache, the fetcher
// (leader-only), its resolver and the internal listener; node B runs
// zonecache and a resolver that pulls from A. A new BlockList reaches B's
// zonecache without B touching the list host.
func TestEndToEndTwoNodes(t *testing.T) {
	st := memory.New()
	srv := newListServer(t, hostsV1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 8)
	run := func(fn func(context.Context) error) { go func() { errs <- fn(ctx) }() }

	type nodeT struct {
		zc    *zonecache.Cache
		objs  *ZoneCacheObjects
		cache *DiskCache
		res   *Resolver
	}
	newNode := func(name string, membership cluster.Membership, client *internalapi.Client, dl func(*DiskCache) *Downloader) *nodeT {
		zc, err := zonecache.New(st, zonecache.Options{Logger: quiet()})
		if err != nil {
			t.Fatal(err)
		}
		n := &nodeT{zc: zc, objs: NewZoneCacheObjects(zc)}
		if n.cache, err = OpenDiskCache(t.TempDir(), quiet()); err != nil {
			t.Fatal(err)
		}
		n.res, err = NewResolver(ResolverOptions{
			Objects: n.objs, Cache: n.cache, Sink: zc, Membership: membership, Client: client,
			Downloader: dl(n.cache), Logger: quiet().With("node", name), RetryInterval: 50 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		run(zc.Start)
		run(n.res.Run)
		return n
	}

	// Node A: the leader.
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: testKey, NodeName: "a"})
	if err != nil {
		t.Fatal(err)
	}
	a := newNode("a", cluster.Self{Name: "a"}, nil, func(c *DiskCache) *Downloader { return NewDownloader(c, DownloadOptions{AllowLocalAddresses: true}) })
	f, err := New(Options{Store: st, Objects: a.objs, Cache: a.cache, Downloader: NewDownloader(a.cache, DownloadOptions{AllowLocalAddresses: true}), Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	run(f.Run)
	srvA, err := internalapi.NewServer(internalapi.ServerOptions{
		Credentials: creds, Lists: a.cache, Logger: quiet(),
		Status: func(context.Context) internalapi.Status {
			return internalapi.Status{Node: "a", Leader: "a", State: a.zc.State().String(), Lists: a.res.Lists()}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	run(func(ctx context.Context) error { return srvA.Serve(ctx, ln) })

	// Node B: a replica that must never download.
	credsB, _ := internalapi.NewCredentials(internalapi.CredentialOptions{Key: testKey, NodeName: "b"})
	clientB := internalapi.NewClient(credsB, internalapi.ClientOptions{})
	b := newNode("b", &cluster.Static{SelfName: "b", LeaderName: "a", Members: []cluster.Peer{{Name: "a", Address: ln.Addr().String()}}},
		clientB, func(*DiskCache) *Downloader { return nil })

	start := time.Now()
	if _, err := st.Apply(ctx, &v1alpha1.BlockList{
		ObjectMeta: metav1.ObjectMeta{Name: "ads", Namespace: "dns"},
		Spec:       v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{URL: srv.URL}}},
	}, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if l := b.zc.Snapshot().Objects.Lists(); l != nil && l.Contains("tracker.example")["dns/ads"] != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replica never compiled the list; status: %+v", getStatus(st))
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Logf("apply -> compiled on the replica: %v", time.Since(start))
	if srv.hits.Load() != 1 {
		t.Errorf("list host hit %d times", srv.hits.Load())
	}
	if e, _ := b.cache.Lookup(srv.URL); e.Origin != OriginLeader {
		t.Errorf("replica origin = %q", e.Origin)
	}
	st2, err := clientB.Status(ctx, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if st2.Node != "a" || len(st2.Lists) != 1 || st2.Lists[0].Name != "dns/ads" {
		t.Errorf("leader status = %+v", st2)
	}
	cancel()
	for range 6 {
		if err := <-errs; err != nil {
			t.Errorf("runnable: %v", err)
		}
	}
}

func getStatus(st store.Store) v1alpha1.BlockListStatus {
	bl := &v1alpha1.BlockList{}
	_ = st.Get(context.Background(), store.Key{Kind: "BlockList", Namespace: "dns", Name: "ads"}, bl)
	return bl.Status
}
