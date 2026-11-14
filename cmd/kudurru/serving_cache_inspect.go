package main

import (
	"context"
	"net/netip"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	cachestage "github.com/robertherbaugh/kudurru/internal/stages/cache"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
)

func newCacheInspector(c *cachestage.Stage, resolver *upstreamstage.Stage, capture pipeline.Capture) api.CacheInspectRead {
	return func(ctx context.Context, in api.CacheInspectRequest) (cache.Lookup, bool, bool, error) {
		if c == nil || resolver == nil || ctx == nil || ctx.Err() != nil {
			return cache.Lookup{}, false, false, pipeline.ErrServingCapture
		}
		q := new(dns.Msg)
		q.SetQuestion(in.Name, in.Type)
		q.CheckingDisabled = in.CD
		q.SetEdns0(1232, in.DO)
		req := &pipeline.Request{Ctx: ctx, Msg: q, Received: time.Now(), Client: pipeline.Client{Addr: netip.AddrPortFrom(in.Client, 0), Synthetic: true}}
		if capture != nil {
			lease, err := capture(req)
			if lease != nil {
				defer lease.Release()
			}
			if err != nil || lease == nil {
				return cache.Lookup{}, false, false, pipeline.ErrServingCapture
			}
			req.Serving = lease
		}
		if _, err := resolver.Bind(req); err != nil {
			return cache.Lookup{}, false, false, err
		}
		return c.Inspect(req)
	}
}
