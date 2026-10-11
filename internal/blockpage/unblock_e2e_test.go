package blockpage_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	engine "github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/blockpage"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	"github.com/robertherbaugh/kudurru/internal/unblock"
)

// storeIndex rebuilds the policy index from the store on Rebuild: the
// test's stand-in for zonecache and the list resolver.
type storeIndex struct {
	st  store.Store
	ix  atomic.Pointer[policy.Index]
	gen uint64
}

func (s *storeIndex) Index() *policy.Index { return s.ix.Load() }

func (s *storeIndex) Rebuild(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	in := policy.Input{}
	var lists []*engine.List
	page, err := s.st.List(ctx, "BlockList", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range page.Items {
		bl := o.(*v1alpha1.BlockList)
		in.BlockLists = append(in.BlockLists, bl)
		mode := engine.ModeBlock
		if bl.Spec.Mode == "allow" {
			mode = engine.ModeAllow
		}
		var srcs []engine.Source
		for i, src := range bl.Spec.Sources {
			srcs = append(srcs, engine.LinesSource(engine.SourceName(i, src), parser.FormatAuto, src.Inline))
		}
		l, err := engine.Compile(engine.Spec{Name: bl.Namespace + "/" + bl.Name, Mode: mode, Sources: srcs}, nil, engine.CompileOptions{})
		if err != nil {
			t.Fatal(err)
		}
		lists = append(lists, l)
	}
	if page, err = s.st.List(ctx, "ClientGroup", store.ListOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, o := range page.Items {
		in.ClientGroups = append(in.ClientGroups, o.(*v1alpha1.ClientGroup))
	}
	if page, err = s.st.List(ctx, "Policy", store.ListOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, o := range page.Items {
		in.Policies = append(in.Policies, o.(*v1alpha1.Policy))
	}
	s.gen++
	if in.Lists, err = engine.NewSnapshot(s.gen, lists...); err != nil {
		t.Fatal(err)
	}
	s.ix.Store(policy.Build(in, policy.Options{DefaultNamespace: "default"}))
}

// fakeClock is the shared test clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// TestUnblockRequestEndToEnd: a blocked name lands on the block page, the
// user requests access, an administrator approves it for everyone for an
// hour through the API, the name resolves, and when the hour is over the
// leader's sweep removes the allow entry and the name is blocked again.
func TestUnblockRequestEndToEnd(t *testing.T) {
	ctx := context.Background()
	clk := &fakeClock{t: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	cfg, err := config.Load("", []string{"KUDURRU_BLOCKPAGE__ENABLED=true", "KUDURRU_LISTENERS__BLOCKPAGE__HTTP=127.0.0.1:0", "KUDURRU_BLOCKING__DEFAULTRESPONSE=blockPage"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st := memory.New()
	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "default"} }
	for _, o := range []store.Object{
		&v1alpha1.ClientGroup{ObjectMeta: meta("default"), Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{CIDR: "127.0.0.0/8"}}}},
		&v1alpha1.BlockList{ObjectMeta: meta("ads"), Spec: v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{Name: "inline", Inline: []string{"||ads.example^"}}}}},
		&v1alpha1.Policy{ObjectMeta: meta("default"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}, BlockListRefs: []string{"ads"}}},
	} {
		if _, err := st.Apply(ctx, o, store.ApplyOptions{Manager: store.ManagerBootstrap}); err != nil {
			t.Fatal(err)
		}
	}
	src := &storeIndex{st: st}
	src.Rebuild(t)

	bopts := blockingstage.OptionsFromConfig(cfg)
	bopts.Index = src
	bs, err := blockingstage.New(bopts)
	if err != nil {
		t.Fatal(err)
	}
	chain, err := pipeline.New(&ingress.Stage{NodeID: "node-a"}, identify.New(src, nil), bs, forwarder{})
	if err != nil {
		t.Fatal(err)
	}
	resolve := func() net.IP {
		t.Helper()
		q := new(dns.Msg)
		q.SetQuestion("www.ads.example.", dns.TypeA)
		resp, err := chain.Serve(&pipeline.Request{Msg: q, Client: pipeline.Client{Addr: netip.MustParseAddrPort("127.0.0.1:40000"), Proto: pipeline.ProtoUDP}})
		if err != nil || len(resp.Msg.Answer) != 1 {
			t.Fatalf("resolve: %v %v", resp, err)
		}
		return resp.Msg.Answer[0].(*dns.A).A
	}
	blockAddr := net.ParseIP("127.0.0.1")
	if got := resolve(); !got.Equal(blockAddr) {
		t.Fatalf("www.ads.example = %v, want the block page", got)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	book := unblock.New(unblock.Options{Store: st, Node: "node-a", Now: clk.Now, Logger: logger})
	pf := blockpage.PolicyFinder{Source: src}
	page, err := blockpage.New(blockpage.Options{
		Node: "node-a", Finder: pf, Policy: pf.Policy, Membership: pf.Membership, Requests: book,
		Settings: blockpage.SettingsFrom(cfg), Logger: logger, Now: clk.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := page.Handler("http")
	browse := func(method, path string, body url.Values) *httptest.ResponseRecorder {
		var rd io.Reader
		if body != nil {
			rd = strings.NewReader(body.Encode())
		}
		r := httptest.NewRequest(method, "http://placeholder"+path, rd)
		r.Host, r.RemoteAddr = "www.ads.example", "127.0.0.1:50000"
		if body != nil {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", "http://www.ads.example")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := browse(http.MethodGet, "/", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Request access</button>") {
		t.Fatalf("block page %d:\n%s", w.Code, w.Body.String())
	}
	if w := browse(http.MethodPost, blockpage.RequestPath, url.Values{"reason": {"I need it"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("post %d %s", w.Code, w.Body.String())
	}
	if w := browse(http.MethodGet, "/", nil); !strings.Contains(w.Body.String(), "has not decided yet") {
		t.Fatalf("pending page:\n%s", w.Body.String())
	}

	// The administrator approves it through the API.
	apiSrv, err := api.New(api.Options{
		Store: st, Validator: v, Unblock: book, Config: cfg, Logger: logger, NodeName: "node-a", Now: clk.Now,
		Authenticate: apitest.Authenticate, Authorizer: apitest.Authorizer(apitest.StandardBindings()...), CSRF: apitest.CSRF,
	})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", apitest.Bearer("admin"))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		apiSrv.ServeHTTP(w, r)
		return w
	}
	w := call(http.MethodGet, "/api/v1/unblockrequests?state=Pending", "")
	var list api.UnblockRequestList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("list %d %s", w.Code, w.Body.String())
	}
	req := list.Items[0]
	if req.Domain != "www.ads.example" || req.Client != "127.0.0.1" || req.Block == nil || req.Block.List != "default/ads" || req.Reason != "I need it" {
		t.Fatalf("request %+v", req)
	}
	w = call(http.MethodPost, "/api/v1/unblockrequests/default/"+req.Name+"/approve", `{"scope":"everyone","match":"exact","duration":"1h"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("approve %d %s", w.Code, w.Body.String())
	}
	src.Rebuild(t) // what zonecache and the list resolver do on the write
	if got := resolve(); got.Equal(blockAddr) {
		t.Fatal("still blocked after the approval")
	}
	if w := browse(http.MethodGet, "/", nil); !strings.Contains(w.Body.String(), "An administrator allowed this name until") {
		t.Fatalf("approved page:\n%s", w.Body.String())
	}

	// The hour ends: the leader's sweep removes the entry.
	clk.Add(time.Hour + time.Second)
	if res, err := book.Sweep(ctx); err != nil || res.Ended != 1 {
		t.Fatalf("sweep %+v %v", res, err)
	}
	src.Rebuild(t)
	if got := resolve(); !got.Equal(blockAddr) {
		t.Fatalf("www.ads.example = %v after the approval ended, want blocked", got)
	}
	if w := browse(http.MethodGet, "/", nil); !strings.Contains(w.Body.String(), "has ended. You can ask again.") {
		t.Fatalf("ended page:\n%s", w.Body.String())
	}
}
