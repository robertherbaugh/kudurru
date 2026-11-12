package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/proxyproto"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	"github.com/robertherbaugh/kudurru/internal/tracing"
)

// optionRecorder is an upstream double that answers A queries and keeps
// the TRACEPARENT data each name arrived with ("" when none).
type optionRecorder struct {
	addr string
	mu   sync.Mutex
	seen map[string][]byte
	hits map[string]int
}

func startOptionRecorder(t *testing.T) *optionRecorder {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &optionRecorder{addr: pc.LocalAddr().String(), seen: map[string][]byte{}, hits: map[string]int{}}
	started := make(chan struct{})
	srv := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		name := q.Question[0].Name
		var data []byte
		if opt := q.IsEdns0(); opt != nil {
			for _, o := range opt.Option {
				if l, ok := o.(*dns.EDNS0_LOCAL); ok && l.Code == tracing.DefaultOptionCode {
					data = bytes.Clone(l.Data)
				}
			}
		}
		r.mu.Lock()
		r.seen[name], r.hits[name] = data, r.hits[name]+1
		r.mu.Unlock()
		answer := new(dns.Msg)
		answer.SetReply(q)
		answer.RecursionAvailable = true
		answer.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 0}, A: net.IPv4(192, 0, 2, 53)}}
		_ = w.WriteMsg(answer)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = srv.Shutdown() })
	return r
}

func (r *optionRecorder) option(name string) (data []byte, hits int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen[name], r.hits[name]
}

// traceCollector is an OTLP/HTTP double that decodes every export with the
// generated OTLP types. stall makes it hold requests until released.
type traceCollector struct {
	mu      sync.Mutex
	spans   map[string][]*tracepb.Span // by hex trace id
	stalled chan struct{}
	entered chan struct{}
	posts   int
}

func (c *traceCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	stalled, entered := c.stalled, c.entered
	c.posts++
	c.mu.Unlock()
	if stalled != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-stalled:
		case <-r.Context().Done():
			return
		}
	}
	body := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		z, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "gzip", http.StatusBadRequest)
			return
		}
		body = z
	}
	raw, _ := io.ReadAll(body)
	var td tracepb.TracesData
	if r.URL.Path != "/v1/traces" || r.Header.Get("Content-Type") != "application/x-protobuf" || proto.Unmarshal(raw, &td) != nil {
		http.Error(w, "bad export", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	for _, rs := range td.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				id := hex.EncodeToString(s.TraceId)
				c.spans[id] = append(c.spans[id], s)
			}
		}
	}
	c.mu.Unlock()
}

func (c *traceCollector) trace(id string) []*tracepb.Span {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.spans[id]
}

func (c *traceCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.spans)
}

// TestRunServeTracingTrustedEdgeTraceparent proves OBS-4 through runServe
// (D382): a TRACEPARENT from an authenticated PROXYv2 edge peer yields a
// span tree at an OTLP/HTTP collector under the edge's trace id, and that
// id in GET /queries, in querylog.db and as a latency exemplar; the option
// reaches only the upstream group flagged tracePropagation, never the
// default (public) one; the same option from an ordinary client is
// removed and ignored; and a stalled collector does not delay answers.
func TestRunServeTracingTrustedEdgeTraceparent(t *testing.T) {
	public, inner := startOptionRecorder(t), startOptionRecorder(t)
	coll := &traceCollector{spans: map[string][]*tracepb.Span{}}
	otlpServer := httptest.NewServer(coll)
	defer otlpServer.Close()

	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.Enabled, cfg.DNSSEC.Enabled = false, false
	cfg.Cache.MaxEntries = 0
	cfg.RateLimit.Enabled = false
	cfg.Querylog.Enabled = true
	cfg.Querylog.Privacy = "full"
	cfg.Querylog.Exclude.SUDN = false
	cfg.Listeners.Metrics.Address = offlineFreeAddress(t, "tcp")
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{
		"default": {Servers: []string{"public=" + public.addr}, Strategy: "strict", Timeout: config.Duration(time.Second)},
		"inner":   {Servers: []string{"inner=" + inner.addr}, Strategy: "strict", Timeout: config.Duration(time.Second), TracePropagation: true},
	}
	cfg.Upstreams.Conditional = map[string]string{"inner.example": "inner"}
	secret := bytes.Repeat([]byte{0x54}, 32)
	cfg.Listeners.ProxyProtocolFrom = []string{"127.0.0.1/32"}
	cfg.Listeners.ProxyProtocolSecretFile = filepath.Join(t.TempDir(), "proxy-secret")
	if err := os.WriteFile(cfg.Listeners.ProxyProtocolSecretFile, secret, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Tracing.Enabled = true
	cfg.Tracing.Endpoint = otlpServer.URL + "/v1/traces"
	cfg.Tracing.SampleRate = 0 // only the edge's sampled flag (and failures) keep a trace
	cfg.Tracing.SlowThreshold = 0
	cfg.Tracing.QueueSize = 8
	cfg.Tracing.Timeout = config.Duration(5 * time.Second)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("tracing mount shutdown failed", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("tracing mount shutdown did not join")
		}
	}
	defer stop()
	deadline := time.Now().Add(20 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("tracing mount never ready")
		}
		time.Sleep(20 * time.Millisecond)
	}

	withOption := func(name string, trace tracing.TraceID, span tracing.SpanID) *dns.Msg {
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeA)
		q.SetEdns0(1232, false)
		opt := q.IsEdns0()
		opt.Option = append(opt.Option, &dns.EDNS0_LOCAL{Code: tracing.DefaultOptionCode, Data: tracing.AppendOption(nil, trace, span, tracing.FlagSampled)})
		return q
	}
	exchange := func(conn net.Conn, prefix []byte, q *dns.Msg) (time.Duration, error) {
		t.Helper()
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		wire, err := q.Pack()
		if err != nil {
			return 0, err
		}
		start := time.Now()
		if _, err := conn.Write(append(prefix, wire...)); err != nil {
			return 0, err
		}
		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err != nil {
			return 0, err
		}
		took := time.Since(start)
		answer := new(dns.Msg)
		if err := answer.Unpack(buf[:n]); err != nil || answer.Rcode != dns.RcodeSuccess || len(answer.Answer) != 1 {
			t.Fatalf("query %s not answered: %v %v", q.Question[0].Name, err, answer)
		}
		if opt := answer.IsEdns0(); opt != nil {
			for _, o := range opt.Option {
				if o.Option() == tracing.DefaultOptionCode {
					t.Fatal("the answer echoed TRACEPARENT")
				}
			}
		}
		return took, nil
	}
	// fromEdge sends q as dnsdist does: PROXYv2 with the client address and
	// the shared-secret TLV, from the listed peer 127.0.0.1.
	fromEdge := func(q *dns.Msg) time.Duration {
		t.Helper()
		conn, err := net.Dial("udp", cfg.Listeners.DNS[0].Address)
		if err != nil {
			t.Fatal(err)
		}
		h := proxyproto.Header{Command: proxyproto.CommandProxy, Family: proxyproto.FamilyINET, Transport: proxyproto.TransportDatagram, Source: netip.MustParseAddrPort("203.0.113.9:40000"), Destination: netip.MustParseAddrPort(cfg.Listeners.DNS[0].Address), RawTLVs: proxyproto.AppendTLV(nil, proxyproto.TLVTypeSecret, secret)}
		header, err := h.Append(nil)
		if err != nil {
			t.Fatal(err)
		}
		took, err := exchange(conn, header, q)
		if err != nil {
			t.Fatal("edge query", err)
		}
		return took
	}
	// fromClient sends q straight from 127.0.0.2, a client that is not a
	// configured proxy.
	fromClient := func(q *dns.Msg) {
		t.Helper()
		target, _ := net.ResolveUDPAddr("udp", cfg.Listeners.DNS[0].Address)
		conn, err := net.DialUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2)}, target)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := exchange(conn, nil, q); err != nil {
			t.Fatal("client query", err)
		}
	}
	id := func(b byte) (tracing.TraceID, tracing.SpanID) {
		return tracing.TraceID{0x5a, b, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, b}, tracing.SpanID{0x6b, b, 1, 2, 3, 4, 5, b}
	}
	t1, s1 := id(1)
	t2, s2 := id(2)
	t3, s3 := id(3)
	t4, s4 := id(4)
	fromEdge(withOption("www.public.example.", t1, s1))
	fromEdge(withOption("x.inner.example.", t2, s2))
	fromClient(withOption("untrusted.public.example.", t3, s3))
	fromEdge(withOption("sentinel.public.example.", t4, s4))

	for _, name := range []string{"www.public.example.", "untrusted.public.example.", "sentinel.public.example."} {
		if data, hits := public.option(name); hits != 1 || data != nil {
			t.Fatalf("public upstream: %s hits %d option %x", name, hits, data)
		}
	}
	data, hits := inner.option("x.inner.example.")
	propagated, ok := tracing.ParseOption(data)
	if hits != 1 || !ok || propagated.TraceID != t2 || propagated.SpanID == s2 || propagated.Flags != tracing.FlagSampled {
		pubData, pubHits := public.option("x.inner.example.")
		t.Fatalf("flagged upstream got %x (%+v) hits %d; public hits %d option %x; collector traces %d", data, propagated, hits, pubHits, pubData, coll.count())
	}

	wait := func(what string, fn func() bool) {
		t.Helper()
		until := time.Now().Add(15 * time.Second)
		for !fn() {
			if time.Now().After(until) {
				t.Fatal("timed out waiting for " + what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	wait("the exported traces", func() bool {
		return len(coll.trace(t1.String())) > 0 && len(coll.trace(t2.String())) > 0 && len(coll.trace(t4.String())) > 0
	})
	if coll.count() != 3 || coll.trace(t3.String()) != nil {
		t.Fatalf("collector holds %d traces; the client's injected id is among them: %v", coll.count(), coll.trace(t3.String()) != nil)
	}
	spans := coll.trace(t1.String())
	byID := map[string]*tracepb.Span{}
	var root, exchangeSpan *tracepb.Span
	for _, s := range spans {
		byID[hex.EncodeToString(s.SpanId)] = s
		switch s.Name {
		case tracing.RootName:
			root = s
		case "kudurru.upstream.exchange":
			exchangeSpan = s
		}
	}
	if root == nil || !bytes.Equal(root.ParentSpanId, s1[:]) || root.Kind != tracepb.Span_SPAN_KIND_SERVER {
		t.Fatalf("root span %v", root)
	}
	if exchangeSpan == nil || exchangeSpan.Kind != tracepb.Span_SPAN_KIND_CLIENT {
		t.Fatal("no client span for the upstream attempt")
	}
	// Walk from the attempt to the root through the stage spans.
	chain := []string{}
	for s := exchangeSpan; s != nil && s != root; s = byID[hex.EncodeToString(s.ParentSpanId)] {
		chain = append(chain, s.Name)
	}
	joined := strings.Join(chain, " < ")
	for _, stage := range []string{"kudurru.upstream", "kudurru.cache", "kudurru.postprocess", "kudurru.ingress"} {
		if !strings.Contains(joined, stage+" ") && !strings.HasSuffix(joined, stage) {
			t.Fatalf("attempt does not descend from %s: %s", stage, joined)
		}
	}
	attrs := map[string]string{}
	for _, kv := range root.Attributes {
		attrs[kv.Key] = kv.Value.GetStringValue()
	}
	if attrs["dns.question.name"] != "www.public.example." || attrs["client.address"] != "203.0.113.9" || attrs["kudurru.upstream"] != "default/public" {
		t.Fatalf("root attributes %v", attrs)
	}
	var innerAttempt bool
	for _, s := range coll.trace(t2.String()) {
		if s.Name == "kudurru.upstream.exchange" && bytes.Equal(s.SpanId, propagated.SpanID[:]) {
			innerAttempt = true
		}
	}
	if !innerAttempt {
		t.Fatal("the propagated parent id is not the exported attempt span")
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	var login api.SessionInfo
	controlMountedRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
	rows := map[string]sqlitelog.QueryRow{}
	wait("the query rows", func() bool {
		var page api.QueriesBody
		mountedFactorRequest(t, client, base, http.MethodGet, "/queries?limit=20", "", nil, http.StatusOK, &page)
		for _, r := range page.Queries {
			rows[r.Domain] = r
		}
		return len(rows) >= 4
	})
	if r := rows["www.public.example"]; r.TraceID != t1.String() || !r.Flags.Traceparent {
		t.Fatalf("API row %+v", r)
	}
	if r := rows["untrusted.public.example"]; r.TraceID != "" || !r.Flags.Traceparent {
		t.Fatalf("untrusted API row carries trace %q", r.TraceID)
	}

	metric := func(accept, family string, labels ...string) (string, float64) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "http://"+cfg.Listeners.Metrics.Address+"/metrics", nil)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		response, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		var all strings.Builder
		value := -1.0
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	lines:
		for scanner.Scan() {
			line := scanner.Text()
			all.WriteString(line + "\n")
			if family == "" || !strings.HasPrefix(line, family+"{") {
				continue
			}
			for _, l := range labels {
				if !strings.Contains(line, l) {
					continue lines
				}
			}
			value, _ = strconv.ParseFloat(line[strings.LastIndexByte(line, ' ')+1:], 64)
		}
		return all.String(), value
	}
	if _, v := metric("", "kudurru_tracing_traceparent_total", `result="untrusted"`); v != 1 {
		t.Fatalf("untrusted TRACEPARENT counted %v", v)
	}
	if _, v := metric("", "kudurru_tracing_traceparent_total", `result="accepted"`); v != 3 {
		t.Fatalf("accepted TRACEPARENT counted %v", v)
	}
	open, _ := metric("application/openmetrics-text; version=1.0.0", "")
	if !strings.Contains(open, `trace_id="`+t4.String()+`"`) && !strings.Contains(open, `trace_id="`+t1.String()+`"`) {
		t.Fatal("no latency exemplar links an exported trace")
	}

	// Stall the collector: one export enters and hangs; then answers must
	// stay fast while kept traces overflow the bounded queue.
	stalled := make(chan struct{})
	coll.mu.Lock()
	coll.stalled, coll.entered = stalled, make(chan struct{}, 1)
	entered := coll.entered
	coll.mu.Unlock()
	t5, s5 := id(5)
	fromEdge(withOption("stall.public.example.", t5, s5))
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the stalled export never started")
	}
	var slowest time.Duration
	for i := range 40 {
		tn, sn := id(byte(10 + i))
		if took := fromEdge(withOption("burst"+strconv.Itoa(i)+".public.example.", tn, sn)); took > slowest {
			slowest = took
		}
	}
	if slowest > time.Second {
		t.Fatalf("an answer took %v while the collector was stalled", slowest)
	}
	if _, v := metric("", "kudurru_tracing_traces_total", `result="queue_full"`); v < 1 {
		t.Fatalf("queue_full = %v with a stalled collector and a queue of 8", v)
	}
	close(stalled)
	coll.mu.Lock()
	coll.stalled = nil
	coll.mu.Unlock()

	stop()
	opts, err := sqlitelog.OptionsFrom(cfg.Querylog, cfg.QuerylogDataDir(), cfg.Node.Name)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlitelog.Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	persisted, err := reopened.Reader().Queries(t.Context(), sqlitelog.QueryFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	stored := map[string]string{}
	for _, r := range persisted.Queries {
		stored[r.Domain] = r.TraceID
	}
	if stored["www.public.example"] != t1.String() || stored["x.inner.example"] != t2.String() || stored["untrusted.public.example"] != "" {
		t.Fatalf("querylog.db trace ids %v", stored)
	}
}
