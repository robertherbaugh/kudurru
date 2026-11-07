package main

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/listfetcher"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestServingBlockingRefreshRetainsPendingAndSelectedPublication(t *testing.T) {
	f := newServingCommandFixture(t)
	key := "https://lists.example/remote"
	put := func(body string) listfetcher.Entry {
		t.Helper()
		e, _, err := f.s.opts.Lists.Put(key, strings.NewReader(body), listfetcher.PutOptions{Fetched: f.now})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	entry := put("previous.example\n")
	obj, err := f.s.opts.Store.Apply(t.Context(), &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "remote", Namespace: "default"}, Spec: public.BlockListSpec{Sources: []public.ListSource{{URL: key, Format: "domains"}}}}, store.ApplyOptions{Manager: store.ManagerSystem})
	if err != nil {
		t.Fatal(err)
	}
	bl := obj.(*public.BlockList)
	status := func(e listfetcher.Entry) {
		t.Helper()
		if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "BlockList", Namespace: "default", Name: "remote"}, bl); err != nil {
			t.Fatal(err)
		}
		bl.Status.Sources = []public.SourceStatus{{Name: key, Digest: e.Digest, LastFetched: &metav1.Time{Time: f.now}}}
		if err := f.s.opts.Store.UpdateStatus(t.Context(), bl); err != nil {
			t.Fatal(err)
		}
	}
	status(entry)
	if _, err := f.s.opts.Store.Apply(t.Context(), &public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}, Spec: public.PolicySpec{BlockListRefs: []string{"remote"}}}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
	id := identify.New(nil, nil)
	id.Capture = func(req *pipeline.Request) (*policy.Index, error) {
		return servingdns.Component[*policy.Index](req, "policy")
	}
	bs, err := blockingstage.New(blockingstage.Options{Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	bs.Capture = func(req *pipeline.Request) (*blockingstage.View, error) {
		return servingdns.Component[*blockingstage.View](req, "blocking")
	}
	capture := func(name string) *pipeline.Request {
		t.Helper()
		lease, err := f.s.publisher.Capture()
		if err != nil {
			t.Fatal(err)
		}
		wrapped := servingdns.Wrap(lease)
		t.Cleanup(wrapped.Release)
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeA)
		return &pipeline.Request{Ctx: t.Context(), Serving: wrapped, Received: f.now, Deadline: f.now.Add(time.Minute), Msg: q, Client: pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.1:5300"), Proto: pipeline.ProtoUDP, IdentityCaptured: true}}
	}
	// Capture both an unstarted request and one already past identification.
	pending, selected := capture("previous.example."), capture("previous.example.")
	if _, err := id.Serve(selected, func(*pipeline.Request) (*pipeline.Response, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	oldPolicy := selected.Policy
	// Replacing disk bytes alone cannot change either captured policy or install
	// a new complete candidate whose source status still names the old digest.
	entry = put("refreshed.example\n")
	response := httptest.NewRecorder()
	f.http.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next, err := f.s.Settings(r.Context())
		if err == nil {
			next.Blocking.RefreshInterval = config.Duration(time.Minute)
			next.Blocking.AllowDegraded = false
			err = f.s.WriteSettings(r.Context(), next, []string{"blocking.refreshInterval", "blocking.allowDegraded"})
		}
		if err != nil {
			t.Error("canonical blocking settings command", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, f.request())
	if response.Code != http.StatusNoContent {
		t.Fatal("settings command did not commit")
	}
	awaitServingRuntime(t, f.s, func() bool {
		s := f.s.publisher.Status()
		return !s.ProtectedReady && !s.Preparing && s.Reason != ""
	})
	if _, err := f.s.captureOperationalSettings(t.Context()); err == nil {
		t.Fatal("operational controls adopted incomplete candidate settings")
	}
	status(entry)
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
	next, err := f.s.captureOperationalSettings(t.Context())
	if err != nil || next.Blocking.RefreshInterval.Std() != time.Minute || next.Blocking.AllowDegraded {
		t.Fatal("complete successor did not carry new operational controls", err)
	}
	ask := func(req *pipeline.Request, blocked bool) {
		t.Helper()
		out, err := id.Serve(req, func(r *pipeline.Request) (*pipeline.Response, error) {
			return bs.Serve(r, func(r *pipeline.Request) (*pipeline.Response, error) {
				return pipeline.Reply(r, dns.RcodeSuccess, pipeline.StatusForwarded), nil
			})
		})
		if err != nil || out == nil {
			t.Fatal("captured blocking query", err)
		}
		defer out.Close()
		if (out.Status == pipeline.StatusBlockedList) != blocked || req.Deadline != f.now.Add(time.Minute) || req.Received != f.now || !req.Client.IdentityCaptured {
			t.Fatal("refresh replaced original policy, identity or request deadline")
		}
	}
	ask(pending, true)
	ask(selected, true)
	if selected.Policy != oldPolicy {
		t.Fatal("selected request adopted successor policy")
	}
	ask(capture("previous.example."), false)
	ask(capture("refreshed.example."), true)
}
