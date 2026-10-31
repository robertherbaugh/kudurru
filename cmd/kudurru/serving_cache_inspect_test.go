package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/api"
	cachedata "github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	cachestage "github.com/robertherbaugh/kudurru/internal/stages/cache"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

func TestServingCacheInspectionRetainsOriginalHTTPPublication(t *testing.T) {
	f := newBrandingHTTPFixture(t)
	u := upstreamstage.NewStage(nil) // Any legacy/live fallback fails.
	u.Capture = func(req *pipeline.Request) (*upstreamstage.View, error) {
		return servingdns.Component[*upstreamstage.View](req, "resolution")
	}
	c := cachestage.New(cachedata.New(cachedata.Options{MaxEntries: 100}), cachestage.Settings{}, u)
	c.Capture = func(req *pipeline.Request) (*cachestage.View, error) {
		return servingdns.Component[*cachestage.View](req, "cache")
	}
	capture := func(req *pipeline.Request) (pipeline.ServingLease, error) {
		l, err := servinghttp.CloneLease(req.Ctx)
		return servingdns.Wrap(l), err
	}
	inspect := newCacheInspector(c, u, capture)
	in := api.CacheInspectRequest{Name: "held-cache.example.", Type: dns.TypeA, Client: netip.MustParseAddr("127.0.0.1")}
	request := func(handler http.HandlerFunc) {
		r := httptest.NewRequest(http.MethodGet, "http://kudurru.test/api/v1/cache/inspect", nil)
		r.Header.Set("Authorization", "Bearer "+f.token)
		rec := httptest.NewRecorder()
		called := false
		f.adapter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; handler(w, r) })).ServeHTTP(rec, r)
		if !called {
			t.Fatalf("original HTTP capture refused: %d", rec.Code)
		}
	}
	request(func(_ http.ResponseWriter, r *http.Request) {
		q := new(dns.Msg)
		q.SetQuestion(in.Name, in.Type)
		req := &pipeline.Request{Ctx: r.Context(), Msg: q, Client: pipeline.Client{Addr: netip.AddrPortFrom(in.Client, 0)}}
		lease, err := capture(req)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		req.Serving = lease
		answer, err := c.Serve(req, func(req *pipeline.Request) (*pipeline.Response, error) {
			m := new(dns.Msg)
			m.SetReply(req.Msg)
			m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: in.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(192, 0, 2, 1)}}
			return &pipeline.Response{Msg: m, Status: pipeline.StatusForwarded, Origin: pipeline.OriginForwarded, Cacheable: true, CacheScope: req.Resolution.(*upstream.QueryPlan).Scope()}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		answer.Close()
		c.Wait()
		if _, found, _, err := inspect(r.Context(), in); err != nil || !found {
			t.Fatal("original warmed cache missing", found, err)
		}
		f.patch(t, `{"cache":{"maxTTL":"61m"}}`, http.StatusOK)
		f.patch(t, `{"cache":{"maxTTL":null}}`, http.StatusOK)
		if _, found, _, err := inspect(r.Context(), in); err != nil || !found {
			t.Fatal("held HTTP cut recaptured after ABA", found, err)
		}
		request(func(_ http.ResponseWriter, fresh *http.Request) {
			if _, found, _, err := inspect(fresh.Context(), in); err != nil || found {
				t.Fatal("successor exposed retired cache", found, err)
			}
		})
		ctx, cancel := context.WithCancel(r.Context())
		cancel()
		if _, found, _, err := inspect(ctx, in); err == nil || found {
			t.Fatal("canceled request exposed cache")
		}
	})
	if _, _, _, err := inspect(t.Context(), in); err == nil {
		t.Fatal("missing original HTTP capture accepted")
	}
}
