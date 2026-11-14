package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
// fetch's upstream attempts appear as client spans. The late query must be
// the prefetching hit and not a plain miss: its trace's root says
// prefetch_refresh, kudurru_cache_prefetches_total rises by one, and that
// one refresh asks each server of the group exactly once.
func TestRunServeTracingThroughCacheAndRace(t *testing.T) {
	fast, fastQueries := startDelayedUpstream(t, 0)
	slow, slowQueries := startDelayedUpstream(t, 300*time.Millisecond)
	coll := &traceCollector{spans: map[string][]*tracepb.Span{}}
	otlpServer := httptest.NewServer(coll)
	defer otlpServer.Close()

	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Listeners.Metrics.Address = offlineFreeAddress(t, "tcp") // the prefetch counter is read from it
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
	// The coalesced fetch stored the 2-second entry before any query of the
	// wave was answered, so the entry is at least this old from here on.
	waveAnswered := time.Now()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// One coalesced fetch asked each server once. The wave is answered as
	// soon as the fast server replies, and the slow server's handler may not
	// have counted its query yet, so give that count a short bounded time to
	// arrive before requiring the exact numbers. The late query below is
	// timed from waveAnswered, not from here.
	for deadline := time.Now().Add(500 * time.Millisecond); slowQueries.Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if f, s := fastQueries.Load(), slowQueries.Load(); f != 1 || s != 1 {
		t.Fatalf("the wave asked the fast server %d and the slow one %d times, want one coalesced fetch", f, s)
	}
	prefetches := func() float64 {
		v, ok := mountedUnlabeledMetric(t, cfg.Listeners.Metrics.Address, "kudurru_cache_prefetches_total")
		if !ok {
			t.Fatal("kudurru_cache_prefetches_total is not exported")
		}
		return v
	}
	if p := prefetches(); p != 0 {
		t.Fatalf("%v prefetches before the hit", p)
	}
	// 1.2 s after the wave the 2-second entry has at most 0.8 s left, within
	// the prefetch trigger (1 s), and has not expired: the next hit answers
	// from the cache, finishes its trace and starts a background refresh
	// carrying that finished trace. The time is measured from the wave and
	// nothing waits on the exporter's flush tick in between.
	time.Sleep(time.Until(waveAnswered.Add(1200 * time.Millisecond)))
	if err := ask(); err != nil {
		t.Fatal("prefetch hit", err)
	}
	if late := time.Since(waveAnswered); late >= 2*time.Second {
		t.Fatalf("the late query was answered %v after the wave, past the entry's 2-second TTL", late)
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
	waitFor("the prefetch to reach the slow server", func() bool { return slowQueries.Load() > 1 })
	// Let the slow attempt of the prefetch end into its finished trace.
	time.Sleep(500 * time.Millisecond)
	waitFor("every trace exported", func() bool { return coll.count() >= wave+1 })
	if p := prefetches(); p != 1 {
		t.Fatalf("kudurru_cache_prefetches_total = %v after the hit, want 1", p)
	}
	if f, s := fastQueries.Load(), slowQueries.Load(); f != 2 || s != 2 {
		t.Fatalf("after the hit the fast server was asked %d and the slow one %d times, want one refresh of each", f, s)
	}

	coll.mu.Lock()
	defer coll.mu.Unlock()
	if len(coll.spans) != wave+1 {
		t.Fatalf("%d traces exported for %d queries", len(coll.spans), wave+1)
	}
	exchanges, hits := 0, 0
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
				status := ""
				for _, kv := range s.Attributes {
					if kv.Key == "kudurru.status" {
						status = kv.Value.GetStringValue()
					}
				}
				// Only the late query is a cache hit that started the
				// refresh; the wave's queries were answered by the fetch
				// (and a plain miss would say forwarded).
				if status == "prefetch_refresh" {
					hits++
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
	if hits != 1 {
		t.Fatalf("%d traces with status prefetch_refresh, want exactly the late query's", hits)
	}
	// The one coalesced fetch asked both servers inside the first query's
	// trace; the prefetch's attempts came after its trace finished.
	if exchanges < 1 || exchanges > 2 {
		t.Fatalf("%d upstream attempt spans across the traces, want the coalesced fetch's one or two", exchanges)
	}
}

// mountedUnlabeledMetric reads a sample without labels from a mounted
// metrics listener (mountedMetric reads labeled ones).
func mountedUnlabeledMetric(t *testing.T, address, family string) (float64, bool) {
	t.Helper()
	response, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + address + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		value, ok := strings.CutPrefix(scanner.Text(), family+" ")
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(strings.Fields(value)[0], 64)
		if err != nil {
			t.Fatal("metrics line", scanner.Text(), err)
		}
		return v, true
	}
	return 0, false
}
