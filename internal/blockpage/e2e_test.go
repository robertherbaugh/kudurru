package blockpage_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	engine "github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/blockpage"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	logstage "github.com/robertherbaugh/kudurru/internal/stages/log"
)

// nopSink drops batches; the test reads the live tail.
type nopSink struct{}

func (nopSink) Name() string                                                       { return "nop" }
func (nopSink) Open(context.Context, json.RawMessage, prometheus.Registerer) error { return nil }
func (nopSink) Write([]*querylog.Event) error                                      { return nil }
func (nopSink) Flush(context.Context) error                                        { return nil }
func (nopSink) Close() error                                                       { return nil }

// forwarder answers every query that reaches it with a public address.
type forwarder struct{}

func (forwarder) Name() string { return "upstream" }
func (forwarder) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	m := new(dns.Msg)
	m.SetReply(req.Msg)
	rr, _ := dns.NewRR(req.Msg.Question[0].Name + " 300 IN A 198.51.100.7")
	m.Answer = []dns.RR{rr}
	return &pipeline.Response{Msg: m, Status: pipeline.StatusForwarded, Upstream: "default"}, nil
}

// TestBlockPageEndToEnd resolves a blocked name through the pipeline under
// the blockPage response, follows the answer to the block page listener
// with the name as Host, and finds the deciding list and policy on the
// page, as a browser would.
func TestBlockPageEndToEnd(t *testing.T) {
	// The configuration: block page on loopback, its address from the
	// listener.
	cfg, err := config.Load("", []string{
		"KUDURRU_BLOCKPAGE__ENABLED=true", "KUDURRU_LISTENERS__BLOCKPAGE__HTTP=127.0.0.1:0",
		"KUDURRU_BLOCKING__DEFAULTRESPONSE=blockPage", "KUDURRU_BLOCKPAGE__CONTACT=it@example.net",
	})
	if err != nil {
		t.Fatal(err)
	}
	v4, _ := cfg.BlockPageAddrs()
	if v4 != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("block page address %v", v4)
	}

	// Policies: one group, one policy, one list.
	l, err := engine.Compile(engine.Spec{Name: "default/ads", Mode: engine.ModeBlock, Sources: []engine.Source{
		engine.LinesSource("inline", parser.FormatAuto, []string{"||ads.example^"}),
	}}, nil, engine.CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := engine.NewSnapshot(1, l)
	if err != nil {
		t.Fatal(err)
	}
	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "default"} }
	ix := policy.Build(policy.Input{
		ClientGroups: []*v1alpha1.ClientGroup{{ObjectMeta: meta("lan"), Spec: v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{CIDR: "127.0.0.0/8"}}}}},
		Policies:     []*v1alpha1.Policy{{ObjectMeta: meta("home"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"lan"}, BlockListRefs: []string{"ads"}}}},
		BlockLists:   []*v1alpha1.BlockList{{ObjectMeta: meta("ads"), Spec: v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{URL: "https://lists.example/ads.txt"}}}}},
		Lists:        snap,
	}, policy.Options{})
	src := policy.Static{Idx: ix}

	// The pipeline with the query log observer feeding the live tail.
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
	tail := querylog.NewTail(256, 0, nil)
	async := querylog.NewAsyncWith(querylog.AsyncOptions{Sink: nopSink{}, Tail: tail, Interval: 10 * time.Millisecond})
	t.Cleanup(func() { _ = async.Close(context.Background()) })
	chain.Observe(&logstage.Observer{Node: "node-a", Collector: async})

	// The block page listener.
	pf := blockpage.PolicyFinder{Source: src}
	srv, err := blockpage.New(blockpage.Options{
		Node: cfg.Node.Name, HTTP: cfg.Listeners.BlockPage.HTTP,
		// The query log alone: the reason must come from the query's own
		// event (PolicyFinder has its own test).
		Finder:   &blockpage.QuerylogFinder{Tail: tail},
		Policy:   pf.Policy,
		Settings: blockpage.SettingsFrom(cfg),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	httpAddr, _ := srv.Addrs()
	_, port, _ := net.SplitHostPort(httpAddr)

	// The browser: resolve, then connect to the answer with the name as Host.
	q := new(dns.Msg)
	q.SetQuestion("www.ads.example.", dns.TypeA)
	q.SetEdns0(1232, false)
	resp, err := chain.Serve(&pipeline.Request{Msg: q, Client: pipeline.Client{Addr: netip.MustParseAddrPort("127.0.0.1:40000"), Proto: pipeline.ProtoUDP}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Answer) != 1 {
		t.Fatalf("answer %v", resp.Msg)
	}
	a, ok := resp.Msg.Answer[0].(*dns.A)
	if !ok || !a.A.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("blocked A answer %v, want the block page address", resp.Msg.Answer[0])
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+net.JoinHostPort(a.A.String(), port)+"/some/page", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "www.ads.example"
	hres, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(hres.Body)
	_ = hres.Body.Close()
	if hres.StatusCode != http.StatusOK || hres.Header.Get("X-Kudurru-Block") != "1" {
		t.Fatalf("block page: %d %v", hres.StatusCode, hres.Header)
	}
	page := string(body)
	for _, want := range []string{
		`<div class="domain">www.ads.example</div>`,
		"The name is on a block list.",
		"<dt>List</dt><dd><code>default/ads</code></dd>",
		"<dt>Rule</dt><dd><code>||ads.example^</code></dd>",
		"<dt>Policy</dt><dd><code>default/home</code></dd>",
		"<code>127.0.0.1</code>",
		`href="mailto:it@example.net"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("block page lacks %q", want)
		}
	}
}
