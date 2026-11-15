package servingbuild

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	cachedata "github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	cachestage "github.com/robertherbaugh/kudurru/internal/stages/cache"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/upstream"
	extensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// This exercises the actual builder's prepared cache partitions. It does not
// grant publication or replace root's separately captured insertion admission.
func TestManagedBuilderRawCachePartitionsForwardedAndCD(t *testing.T) {
	f := newManagedBuilderFixture(t, true)
	// A default forwarded route is cacheable; explicit nondefault hints are
	// intentionally excluded by the existing stage. Preserve anchor policy.
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var settings public.Kudurru
		if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
			return err
		}
		var upstreams map[string]any
		if err := json.Unmarshal(settings.Spec.Settings["upstreams"].Raw, &upstreams); err != nil {
			return err
		}
		groups := upstreams["groups"].(map[string]any)
		groups["native"], groups["default"] = groups["default"], groups["forward"]
		raw, err := json.Marshal(upstreams)
		if err != nil {
			return err
		}
		settings.Spec.Settings["upstreams"] = extensions.JSON{Raw: raw}
		_, err = tx.Apply(ctx, &settings, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	a, b := f.semantic(t), f.semantic(t)
	shared := cachedata.New(cachedata.Options{MaxEntries: 100})
	build := func(owner *semanticOwners) *cachestage.Stage {
		u := upstreamstage.NewStage(nil)
		u.Capture = func(*pipeline.Request) (*upstreamstage.View, error) { return owner.resolution, nil }
		c := cachestage.New(shared, cachestage.Settings{}, u)
		c.Capture = func(*pipeline.Request) (*cachestage.View, error) { return owner.cache, nil }
		t.Cleanup(c.Wait)
		return c
	}
	first, second := build(a), build(b)
	for _, cd := range []bool{false, true} {
		calls := 0
		next := func(req *pipeline.Request) (*pipeline.Response, error) {
			calls++
			plan := req.Resolution.(*upstream.QueryPlan)
			if plan.Recursive() {
				return nil, errors.New("fixture did not select independent forwarded route")
			}
			m := new(dns.Msg)
			m.SetReply(req.Msg)
			rr, err := dns.NewRR("partition.example. 60 IN A 192.0.2.6")
			if err != nil {
				return nil, err
			}
			m.Answer = []dns.RR{rr}
			return &pipeline.Response{Msg: m, Status: pipeline.StatusForwarded, Origin: pipeline.OriginForwarded, Cacheable: true, CacheScope: plan.Scope()}, nil
		}
		for index, stage := range []*cachestage.Stage{first, first, second, second} {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			m := new(dns.Msg)
			m.SetQuestion("partition.example.", dns.TypeA)
			m.CheckingDisabled = cd
			out, err := stage.Serve(&pipeline.Request{Ctx: ctx, Msg: m}, next)
			cancel()
			stage.Wait()
			if err != nil {
				t.Fatal(err)
			}
			want := pipeline.StatusForwarded
			if index%2 == 1 {
				want = pipeline.StatusCached
			}
			if out.Status != want {
				t.Fatal("owner/local cache partition lost", cd, index, out.Status, want)
			}
		}
		if calls != 2 {
			t.Fatal("new managed domain reused prior raw entry", cd, calls)
		}
	}
}
