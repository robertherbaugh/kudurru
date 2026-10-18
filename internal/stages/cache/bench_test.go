package cache

import (
	"context"
	"net/netip"
	"strconv"
	"testing"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// BenchmarkServe measures the cache stage end to end on the query path:
// key, lookup, unpack, TTL countdown and adapting the reply to the client
// (hit), and the bookkeeping around a forwarded miss with a zero-cost
// upstream (miss).
func BenchmarkServe(b *testing.B) {
	cfg, err := config.Load("", nil)
	if err != nil {
		b.Fatal(err)
	}
	opts := cache.OptionsFrom(cfg.Cache, cache.NewMetrics(nil))
	stage := New(cache.New(opts), SettingsFrom(cfg.Cache), nil)
	up := forwarded("192.0.2.1", 300)

	const names = 10_000
	reqs := make([]*pipeline.Request, names)
	for i := range reqs {
		m := new(dns.Msg)
		m.SetQuestion("host-"+strconv.Itoa(i)+".example.com.", dns.TypeA)
		m.SetEdns0(1232, false)
		reqs[i] = &pipeline.Request{Ctx: context.Background(), Msg: m, Client: pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.200:4000")}}
		if _, err := stage.Serve(reqs[i], up); err != nil {
			b.Fatal(err)
		}
	}

	b.Run("hit", func(b *testing.B) {
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			resp, err := stage.Serve(reqs[i%names], up)
			if err != nil || resp.Status != pipeline.StatusCached {
				b.Fatalf("%v %v", resp.Status, err)
			}
			i++
		}
	})
	b.Run("hit-parallel", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				// Requests are not shared across goroutines in the server;
				// copy the message so each goroutine has its own.
				r := *reqs[i%names]
				r.Msg = r.Msg.Copy()
				if _, err := stage.Serve(&r, up); err != nil {
					b.Error(err)
					return
				}
				i++
			}
		})
	})
	b.Run("miss", func(b *testing.B) {
		b.ReportAllocs()
		m := new(dns.Msg)
		req := &pipeline.Request{Ctx: context.Background(), Msg: m}
		i := 0
		for b.Loop() {
			m.SetQuestion("miss-"+strconv.Itoa(i)+".example.net.", dns.TypeA)
			if _, err := stage.Serve(req, up); err != nil {
				b.Fatal(err)
			}
			i++
		}
	})
}
