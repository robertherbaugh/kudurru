package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestRunServeConfiguredTCPQueryLimit(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	cfg.Listeners.Limits.MaxTCPQueries = 2
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("runServe: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("listener fixture failed to stop")
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("server not ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	client := &dns.Client{Net: "tcp", Timeout: time.Second}
	conn, err := client.Dial(cfg.Listeners.DNS[0].Address)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	query := new(dns.Msg)
	query.SetQuestion("health.kudurru.internal.", dns.TypeTXT)
	for range 2 {
		answer, _, err := client.ExchangeWithConn(query, conn)
		if err != nil || answer.Rcode != dns.RcodeSuccess {
			t.Fatal("configured connection closed before its query bound")
		}
	}
	if _, _, err := client.ExchangeWithConn(query, conn); err == nil {
		t.Fatal("third query succeeded after configured two-query limit")
	}
}
