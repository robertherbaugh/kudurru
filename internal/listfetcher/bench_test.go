package listfetcher

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
)

// benchBody is a hosts list of n entries (about 25 bytes per line).
func benchBody(n int) []byte {
	var b bytes.Buffer
	for i := range n {
		fmt.Fprintf(&b, "0.0.0.0 host-%07d.ads.example\n", i)
	}
	return b.Bytes()
}

// BenchmarkPullByDigest measures one replica pull of a 1M-entry list over
// the internal listener (TLS 1.3, token check, streaming sha256 check) into
// the replica's disk cache (tee to temp, fsync, rename).
func BenchmarkPullByDigest(b *testing.B) {
	body := benchBody(1_000_000)
	digest := sha(body)
	leaderCache, err := OpenDiskCache(b.TempDir(), quiet())
	if err != nil {
		b.Fatal(err)
	}
	if _, _, err := leaderCache.Put("https://lists.example/big", bytes.NewReader(body), PutOptions{}); err != nil {
		b.Fatal(err)
	}
	creds, _ := internalapi.NewCredentials(internalapi.CredentialOptions{Key: testKey})
	srv, _ := internalapi.NewServer(internalapi.ServerOptions{Credentials: creds, Lists: leaderCache, Logger: quiet(), RateLimit: 1e6, Burst: 1e6})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx, ln) }()
	client := internalapi.NewClient(creds, internalapi.ClientOptions{})
	replicaCache, err := OpenDiskCache(b.TempDir(), quiet())
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := range b.N {
		rc, _, err := client.OpenList(ctx, ln.Addr().String(), digest)
		if err != nil {
			b.Fatal(err)
		}
		// A new key each time so Put really writes and renames.
		if _, _, err := replicaCache.Put(fmt.Sprintf("k%d", i), rc, PutOptions{ExpectDigest: digest, MaxSize: DefaultMaxHTTPSize}); err != nil {
			b.Fatal(err)
		}
		_ = rc.Close()
	}
	b.ReportMetric(float64(len(body))/(1<<20), "MiB/list")
}

// BenchmarkResolvePass measures a resolver pass over 100 lists whose inputs
// did not change (the cost of every BlockList status event on a node).
func BenchmarkResolvePass(b *testing.B) {
	st := newBenchStore(b, 100)
	cache, _ := OpenDiskCache(b.TempDir(), quiet())
	r, err := NewResolver(ResolverOptions{Objects: newStoreObjects(st), Cache: cache, Sink: &sink{}, Logger: quiet()})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	r.pass(ctx, false)
	b.ResetTimer()
	for range b.N {
		if res := r.pass(ctx, false); res.Rebuilt {
			b.Fatal("unchanged pass rebuilt")
		}
	}
}

// BenchmarkFetchAndPublish1M measures the leader's work when a 1M-entry
// list changed: download, parser validation, compile for the counts and
// one status write.
func BenchmarkFetchAndPublish1M(b *testing.B) {
	bodies := [2][]byte{benchBody(1_000_000), append(benchBody(1_000_000), "0.0.0.0 extra.example\n"...)}
	fx := &fixture{clk: newClock()}
	fx.st = &countingStore{Store: memory.New()}
	fx.cache, _ = OpenDiskCache(b.TempDir(), quiet())
	fx.dl = NewDownloader(fx.cache, DownloadOptions{Now: fx.clk.Now})
	f, err := New(Options{Store: fx.st, Objects: newStoreObjects(fx.st), Cache: fx.cache, Downloader: fx.dl, Logger: quiet(), Now: fx.clk.Now})
	if err != nil {
		b.Fatal(err)
	}
	srv := &listServer{etag: `"0"`}
	srv.Server = httptest.NewServer(http.HandlerFunc(srv.serve))
	defer srv.Close()
	applyList(b, fx.st, "big", v1alpha1.ListSource{URL: srv.URL})
	ctx := context.Background()
	b.SetBytes(int64(len(bodies[0])))
	b.ResetTimer()
	for i := range b.N {
		srv.set("", string(bodies[i%2]), fmt.Sprintf(`"%d"`, i))
		f.Reconcile(ctx)
		fx.clk.Advance(2 * time.Hour)
	}
	b.StopTimer()
	if got := getList(b, fx.st, "big").Status.Entries; got < 1_000_000 {
		b.Fatalf("entries = %d", got)
	}
}

func newBenchStore(b *testing.B, n int) store.Store {
	st := memory.New()
	for i := range n {
		applyList(b, st, fmt.Sprintf("l%03d", i), v1alpha1.ListSource{Inline: []string{fmt.Sprintf("||l%03d.example^", i), "a.example", "b.example"}})
	}
	return st
}
