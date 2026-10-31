package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
)

func TestRunServeSQLiteCacheGrowthCanonicalSettings(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	started := make(chan struct{})
	upstream := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		calls.Add(1)
		answer := new(dns.Msg)
		answer.SetReply(q)
		answer.RecursionAvailable = true
		answer.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300}, A: net.IPv4(192, 0, 2, 80)}}
		_ = w.WriteMsg(answer)
	})}
	go func() { _ = upstream.ActivateAndServe() }()
	<-started
	defer func() { _ = upstream.Shutdown() }()
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 64
	cfg.Cache.Prefetch.Threshold = 0
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{"controlled=" + pc.LocalAddr().String()}, Strategy: "strict", Timeout: config.Duration(time.Second)}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("cache mount shutdown failed", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("cache mount shutdown did not join")
		}
	}()
	deadline := time.Now().Add(20 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("cache mount never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	var login api.SessionInfo
	mountedFactorRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
	var me api.Me
	mountedFactorRequest(t, client, base, http.MethodGet, "/auth/me", login.CSRF, nil, http.StatusOK, &me)
	query := func(name string) {
		t.Helper()
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeA)
		answer, _, err := (&dns.Client{Timeout: 2 * time.Second}).Exchange(q, cfg.Listeners.DNS[0].Address)
		if err != nil || answer.Rcode != dns.RcodeSuccess || len(answer.Answer) != 1 {
			t.Fatal("native cache query failed", err)
		}
	}
	// Warm the one-shard startup cache through the complete DNS assembly.
	query("before-growth.example.org.")
	warmed := calls.Load()
	query("before-growth.example.org.")
	if calls.Load() != warmed {
		t.Fatal("startup cache did not warm")
	}
	// This mutation is one authenticated HTTP attempt. Observe fresh reads while
	// the committed settings propagate to a new complete serving publication.
	var settings api.SettingsBody
	mountedFactorRequest(t, client, base, http.MethodPatch, "/settings", login.CSRF,
		map[string]any{"cache": map[string]any{"maxEntries": 1_000_000}}, http.StatusOK, &settings)
	if settings.Persisted == nil || !*settings.Persisted {
		t.Fatal("growth was not durably admitted")
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		mountedFactorRequest(t, client, base, http.MethodGet, "/settings", login.CSRF, nil, http.StatusOK, &settings)
		if settings.Settings["cache"].(map[string]any)["maxEntries"] == float64(1_000_000) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("growth settings never published")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Retained live entries from the old publication must not be adopted by
	// the successor merely because the only setting change was capacity.
	before := calls.Load()
	query("before-growth.example.org.")
	if calls.Load() != before+1 {
		t.Fatal("successor adopted another publication's live cache entry")
	}
	query("before-growth.example.org.")
	if calls.Load() != before+1 {
		t.Fatal("successor failed to cache its own answer")
	}
	// The operational watcher runs independently of DNS publication. Observe
	// applied capacity by retaining far more than the startup bound. No mutation
	// is retried, and every query still uses normal captured DNS owners.
	deadline = time.Now().Add(5 * time.Second)
	for {
		for i := range 512 {
			query(fmt.Sprintf("growth-%d.example.org.", i))
		}
		filled := calls.Load()
		for i := range 512 {
			query(fmt.Sprintf("growth-%d.example.org.", i))
		}
		if calls.Load() == filled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("published growth did not reach production cache storage")
		}
	}
	var result api.FlushResult
	mountedFactorRequest(t, client, base, http.MethodPost, "/cache/flush", login.CSRF, nil, http.StatusOK, &result)
	if result.Flushed < 514 {
		t.Fatal("grown storage owner not shared with current flush command", result.Flushed)
	}
	if cfg.Cache.MaxEntries != 64 {
		t.Fatal("reload changed startup host settings")
	}
}
