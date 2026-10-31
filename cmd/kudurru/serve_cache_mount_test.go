package main

import (
	"context"
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

func TestRunServeSQLiteCacheFlushNativeScopedAnswers(t *testing.T) {
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
	cfg.Cache.MaxEntries = 1000
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
	const target, child = "cache-operator.example.org.", "child.cache-operator.example.org."
	query(target)
	query(child)
	warmed := calls.Load()
	query(target)
	query(child)
	if warmed != 2 || calls.Load() != warmed {
		t.Fatal("complete DNS stage did not cache both controlled answers", warmed, calls.Load())
	}
	var inspection api.CacheInspection
	mountedFactorRequest(t, client, base, http.MethodGet, "/cache/inspect?name="+target, login.CSRF, nil, http.StatusOK, &inspection)
	if !inspection.Found || !inspection.Cacheable || inspection.Entry == nil || inspection.Entry.Kind != "positive" || inspection.Entry.WireBytes == 0 || inspection.Entry.TTLSeconds != 300 {
		t.Fatal("mounted inspection missed real DNS entry", inspection)
	}
	for _, path := range []string{"/cache/inspect?name=never-queried.example.org", "/cache/inspect?name=" + target + "&do=true", "/cache/inspect?name=" + target + "&cd=true"} {
		inspection = api.CacheInspection{}
		mountedFactorRequest(t, client, base, http.MethodGet, path, login.CSRF, nil, http.StatusOK, &inspection)
		if inspection.Found || inspection.Entry != nil {
			t.Fatal("inspection crossed raw key", path)
		}
	}
	if calls.Load() != warmed {
		t.Fatal("inspection performed upstream I/O")
	}
	mountedFactorRequest(t, &http.Client{Timeout: time.Second}, base, http.MethodGet, "/cache/inspect?name="+target, "", nil, http.StatusUnauthorized, nil)
	var result api.FlushResult
	mountedFactorRequest(t, client, base, http.MethodPost, "/cache/flush", login.CSRF, map[string]string{"name": "CACHE-OPERATOR.example.org"}, http.StatusOK, &result)
	if result.Flushed != 1 || result.Name != target {
		t.Fatal("mounted exact-name flush missed the scoped cache entry", result)
	}
	query(child)
	if calls.Load() != warmed {
		t.Fatal("exact-name flush also invalidated its descendant")
	}
	query(target)
	if calls.Load() != warmed+1 {
		t.Fatal("flushed scoped answer survived in complete DNS cache")
	}
	mountedFactorRequest(t, client, base, http.MethodPost, "/cache/flush", login.CSRF, nil, http.StatusOK, &result)
	if result.Flushed != 2 {
		t.Fatal("full clear did not count actual entries", result.Flushed)
	}
	query(target)
	query(child)
	if calls.Load() != warmed+3 {
		t.Fatal("full clear retained native scoped answers")
	}
}
