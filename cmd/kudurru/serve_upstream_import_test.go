package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/config"
)

func TestRunServeImportedUpstreamRoutes(t *testing.T) {
	startUpstream := func(last byte) (string, *atomic.Int64) {
		t.Helper()
		conn, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		calls := new(atomic.Int64)
		started := make(chan struct{})
		server := &dns.Server{PacketConn: conn, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
			calls.Add(1)
			answer := new(dns.Msg)
			answer.SetReply(q)
			answer.RecursionAvailable = true
			answer.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(192, 0, 2, last)}}
			_ = w.WriteMsg(answer)
		})}
		go func() { _ = server.ActivateAndServe() }()
		<-started
		t.Cleanup(func() { _ = server.Shutdown() })
		return conn.LocalAddr().String(), calls
	}
	defaultAddress, defaultCalls := startUpstream(10)
	importedAddress, importedCalls := startUpstream(20)
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{defaultAddress}, Strategy: "strict"}}
	cfg.Upstreams.ImportFile = "upstream-routes.conf"
	source := filepath.Join(t.TempDir(), "node.json")
	routes := fmt.Sprintf("[/corp.example.net/]%s\nserver=/public.corp.example.net/#\n", importedAddress)
	if err := os.WriteFile(filepath.Join(filepath.Dir(source), cfg.Upstreams.ImportFile), []byte(routes), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(15 * time.Second):
			t.Error("imported routing server did not stop")
		}
	})
	// This fixture proves forwarding; the shared authoritative-zone helper
	// intentionally requires AA and is unsuitable for these responses.
	deadline := time.Now().Add(20 * time.Second)
	for _, route := range []struct{ name, address string }{
		{"www.corp.example.net.", "192.0.2.20"},
		{"www.public.corp.example.net.", "192.0.2.10"},
		{"outside.example.net.", "192.0.2.10"},
	} {
		for {
			query := new(dns.Msg)
			query.SetQuestion(route.name, dns.TypeA)
			answer, _, err := (&dns.Client{Timeout: 200 * time.Millisecond}).ExchangeContext(ctx, query, cfg.Listeners.DNS[0].Address)
			if err == nil && answer != nil && answer.Rcode == dns.RcodeSuccess && !answer.Authoritative && answer.RecursionAvailable && len(answer.Answer) == 1 {
				if a, ok := answer.Answer[0].(*dns.A); ok && a.A.String() == route.address && offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") == http.StatusOK {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatal("mounted imported route never served the expected forwarding answer", route.name)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if defaultCalls.Load() < 2 || importedCalls.Load() == 0 {
		t.Fatal("native queries did not reach both canonical routes")
	}
}
