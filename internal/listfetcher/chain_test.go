package listfetcher

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// chainFixture runs a store, the zonecache with the policy view, a
// resolver and the identify → blocking → forwarder chain.
type chainFixture struct {
	t     *testing.T
	st    store.Store
	chain *pipeline.Chain
}

func newChain(t *testing.T) *chainFixture {
	t.Helper()
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
	errs := make(chan error, 2)
	go func() { errs <- zc.Start(ctx) }()
	go func() { errs <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		for range 2 {
			if err := <-errs; err != nil {
				t.Errorf("runnable: %v", err)
			}
		}
	})
	src := policy.Source{Cache: zc}
	bs, err := blocking.New(blocking.Options{Index: src})
	if err != nil {
		t.Fatal(err)
	}
	chain, err := pipeline.New(identify.New(src, nil), bs, &forwarder{})
	if err != nil {
		t.Fatal(err)
	}
	return &chainFixture{t: t, st: st, chain: chain}
}

func (c *chainFixture) apply(obj store.Object) {
	c.t.Helper()
	if _, err := c.st.Apply(context.Background(), obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		c.t.Fatal(err)
	}
}

// waitBlocked waits until name, asked by client, is blocked (or not).
func (c *chainFixture) waitBlocked(name, client string, blocked bool) {
	c.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		req := &pipeline.Request{Msg: new(dns.Msg).SetQuestion(name, dns.TypeA),
			Client: pipeline.Client{Addr: netip.AddrPortFrom(netip.MustParseAddr(client), 5353), Proto: pipeline.ProtoUDP}}
		resp, err := c.chain.Serve(req)
		if err != nil {
			c.t.Fatal(err)
		}
		if (resp.Status.Class() == pipeline.ClassBlocked) == blocked {
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("%s as %s: blocked %v, want %v", name, client, !blocked, blocked)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
