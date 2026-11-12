package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/robertherbaugh/kudurru/internal/config"
)

// startDelayedUpstream is a UDP upstream double that answers every A query
// after delay with a 2-second TTL, so the cache stores the answer and a
// later hit is due for prefetch. It counts the queries it received.
func startDelayedUpstream(t *testing.T, delay time.Duration) (string, *atomic.Int64) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var queries atomic.Int64
	started := make(chan struct{})
	srv := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		queries.Add(1)
		time.Sleep(delay)
		answer := new(dns.Msg)
		answer.SetReply(q)
		answer.RecursionAvailable = true
		answer.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 2}, A: net.IPv4(192, 0, 2, 54)}}
		_ = w.WriteMsg(answer)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = srv.Shutdown() })
	return pc.LocalAddr().String(), &queries
}

// TestRunServeTracingThroughCacheAndRace runs traced queries through
// runServe with the cache on and a parallel_best group of a fast and a
// slow server (D382). Concurrent identical queries join one coalesced
// fetch, which runs on a copy of the first request and so writes into
// that query's trace from another goroutine; the slow attempt ends after
// the fast one answered; and a later hit starts a prefetch that writes
// into a trace already finished. Under -race no write may race the
// exporter, every query exports exactly one well-formed tree, and the
// fetch's upstream attempts appear as client spans.
func TestRunServeTracingThroughCacheAndRace(t *testing.T) {
	fast, _ := startDelayedUpstream(t, 0)
	slow, slowQueries := startDelayedUpstream(t, 300*time.Millisecond)
	coll := &traceCollector{spans: map[string][]*tracepb.Span{}}
	otlpServer := httptest.NewServer(coll)
	defer otlpServer.Close()

	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.Enabled, cfg.DNSSEC.Enabled = false, false
	cfg.RateLimit.Enabled = false
	cfg.Cache.MaxEntries = 1000
	cfg.Cache.MinTTL = 0
	cfg.Cache.Prefetch = config.CachePrefetch{Threshold: 1, Window: config.Duration(10 * time.Second)}
	cfg.Cache.ServeStale = config.CacheServeStale{}
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{
		"default": {Servers: []string{"fast=" + fast, "slow=" + slow}, Strategy: "parallel_best", Timeout: config.Duration(2 * time.Second)},
	}
	cfg.Upstreams.Conditional = nil
	cfg.Tracing.Enabled = true
	cfg.Tracing.Endpoint = otlpServer.URL + "/v1/traces"
	cfg.Tracing.SampleRate = config.MaxTracingSampleRate // every query is kept
	cfg.Tracing.SlowThreshold = 0
	cfg.Tracing.Timeout = config.Duration(5 * time.Second)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("tracing mount shutdown failed", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("tracing mount shutdown did not join")
		}
	}()
	deadline := time.Now().Add(20 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("tracing mount never ready")
		}
		time.Sleep(20 * time.Millisecond)
	}

	ask := func() error {
		c := &dns.Client{Net: "udp", Timeout: 3 * time.Second}
		q := new(dns.Msg)
		q.SetQuestion("hot.public.example.", dns.TypeA)
		answer, _, err := c.Exchange(q, cfg.Listeners.DNS[0].Address)
		if err != nil {
			return err
		}
		if answer.Rcode != dns.RcodeSuccess || len(answer.Answer) != 1 {
			return fmt.Errorf("hot.public.example. answered %s with %d records", dns.RcodeToString[answer.Rcode], len(answer.Answer))
		}
		return nil
	}
	const wave = 8
	var wg sync.WaitGroup
	errs := make(chan error, wave)
	for range wave {
		wg.Go(func() {
			if err := ask(); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// After a second the 2-second entry is within the prefetch trigger: the
	// next hit answers from the cache, finishes its trace and starts a
	// background refresh carrying that finished trace.
	before := slowQueries.Load()
	time.Sleep(1200 * time.Millisecond)
	if err := ask(); err != nil {
		t.Fatal("prefetch hit", err)
	}
	waitFor := func(what string, fn func() bool) {
		t.Helper()
		until := time.Now().Add(15 * time.Second)
		for !fn() {
			if time.Now().After(until) {
				t.Fatal("timed out waiting for " + what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor("the prefetch to reach the slow server", func() bool { return slowQueries.Load() > before })
	// Let the slow attempt of the prefetch end into its finished trace.
	time.Sleep(500 * time.Millisecond)
	waitFor("every trace exported", func() bool { return coll.count() >= wave+1 })

	coll.mu.Lock()
	defer coll.mu.Unlock()
	if len(coll.spans) != wave+1 {
		t.Fatalf("%d traces exported for %d queries", len(coll.spans), wave+1)
	}
	exchanges := 0
	for id, spans := range coll.spans {
		byID := map[string]*tracepb.Span{}
		roots := 0
		for _, s := range spans {
			byID[hex.EncodeToString(s.SpanId)] = s
			if s.Name == "kudurru.dns.query" {
				roots++
			}
		}
		if roots != 1 || len(byID) != len(spans) {
			t.Fatalf("trace %s: %d roots, %d spans, %d distinct ids", id, roots, len(spans), len(byID))
		}
		for _, s := range spans {
			if s.EndTimeUnixNano < s.StartTimeUnixNano {
				t.Fatalf("trace %s: span %s ends before it starts", id, s.Name)
			}
			if s.Name == "kudurru.dns.query" {
				if len(s.ParentSpanId) != 0 {
					t.Fatalf("trace %s: a plain client's root has a parent", id)
				}
				continue
			}
			if byID[hex.EncodeToString(s.ParentSpanId)] == nil {
				t.Fatalf("trace %s: span %s has a parent outside the trace", id, s.Name)
			}
			if s.Name == "kudurru.upstream.exchange" {
				exchanges++
			}
		}
	}
	// The one coalesced fetch asked both servers inside the first query's
	// trace; the prefetch's attempts came after its trace finished.
	if exchanges < 1 || exchanges > 2 {
		t.Fatalf("%d upstream attempt spans across the traces, want the coalesced fetch's one or two", exchanges)
	}
}
