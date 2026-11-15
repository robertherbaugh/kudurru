package externaldns

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func relayCA(t *testing.T, upstream *httptest.Server) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}), 0o600); err != nil {
		t.Fatal("fixture CA failed")
	}
	return file
}

func TestRelayActualAuthenticatedWebhook(t *testing.T) {
	backend, token := authenticatedWebhook(t)
	upstream := httptest.NewTLSServer(backend)
	defer upstream.Close()
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		t.Fatal("fixture token failed")
	}
	relay, err := NewRelay(RelayOptions{Upstream: upstream.URL, TokenFile: file, CAFile: relayCA(t, upstream)})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodGet, "/", "", 200},
		{http.MethodPost, "/adjustendpoints", `[{"dnsName":"relay.example.com","recordType":"A","targets":["192.0.2.8"]}]`, 200},
		{http.MethodPost, "/records", `{"create":[{"dnsName":"relay.example.com","recordType":"A","targets":["192.0.2.8"]}]}`, 204},
		{http.MethodGet, "/records", "", 200},
		{http.MethodPost, "/records", `{"delete":[{"dnsName":"relay.example.com","recordType":"A","targets":["192.0.2.8"]}]}`, 204},
	} {
		w := do(t, relay, step.method, step.path, step.body)
		if w.Code != step.status {
			t.Fatalf("relay %s %s status=%d", step.method, step.path, w.Code)
		}
		if step.path == "/records" && step.method == http.MethodGet {
			var endpoints []Endpoint
			if json.Unmarshal(w.Body.Bytes(), &endpoints) != nil || len(endpoints) != 1 || endpoints[0].DNSName != "relay.example.com" {
				t.Fatal("relay did not expose actual upstream committed record")
			}
		}
	}
	// Files are a startup snapshot. A subsequent relay must use the new token;
	// the existing instance does not switch authority behind an active operation.
	if err := os.WriteFile(file, []byte(strings.Repeat("x", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := do(t, relay, http.MethodGet, "/records", "").Code; got != 200 {
		t.Fatal("startup credential changed")
	}
	next, err := NewRelay(RelayOptions{Upstream: upstream.URL, TokenFile: file, CAFile: relayCA(t, upstream)})
	if err != nil {
		t.Fatal(err)
	}
	if got := do(t, next, http.MethodGet, "/records", "").Code; got != 401 {
		t.Fatal("wrong upstream credential was accepted")
	}
	untrusted, err := NewRelay(RelayOptions{Upstream: upstream.URL, TokenFile: file})
	if err != nil {
		t.Fatal(err)
	}
	if got := do(t, untrusted, http.MethodGet, "/records", "").Code; got != 502 {
		t.Fatal("untrusted upstream accepted")
	}
}

func TestRelayNoRedirectReplayOrCallerAuthority(t *testing.T) {
	var calls, leaked atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		for _, h := range []string{"Authorization", "Cookie", "Forwarded", "X-Forwarded-Host", "X-Kudurru-CSRF"} {
			if req.Header.Get(h) != "" {
				leaked.Add(1)
			}
		}
		if req.URL.Path == "/prefix/records" {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Error("fixture hijack failed")
				return
			}
			_ = conn.Close() // Deliberately lose the mutation acknowledgment.
			return
		}
		w.Header().Set("Location", "/records")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()
	relay, err := NewRelay(RelayOptions{Upstream: upstream.URL + "/prefix"})
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/records", "/adjustendpoints"} {
		req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(`{}`))
		for _, h := range []string{"Authorization", "Cookie", "Forwarded", "X-Forwarded-Host", "X-Kudurru-CSRF"} {
			req.Header.Set(h, "caller-data")
		}
		w := httptest.NewRecorder()
		relay.ServeHTTP(w, req)
		if w.Code != 502 || w.Header().Get("Location") != "" {
			t.Fatal("uncertain operation or redirect accepted")
		}
	}
	if calls.Load() != 2 || leaked.Load() != 0 {
		t.Fatal("operation replayed or caller authority forwarded")
	}
	for _, route := range []string{"/other", "/records?target=other", "/rec%6frds", "/records/"} {
		if do(t, relay, http.MethodGet, route, "").Code != 404 {
			t.Fatal("unsupported operation forwarded")
		}
	}
	if calls.Load() != 2 {
		t.Fatal("unsupported request reached upstream")
	}
}

func TestRelayRetainsUpstreamWriteRefusal(t *testing.T) {
	backend, _ := authenticatedWebhook(t)
	guard := &refusingWebhookStore{Store: backend.opts.Store}
	backend.opts.Store = guard
	upstream := httptest.NewTLSServer(backend)
	defer upstream.Close()
	relay, err := NewRelay(RelayOptions{Upstream: upstream.URL, TokenFile: backend.opts.TokenFile, CAFile: relayCA(t, upstream)})
	if err != nil {
		t.Fatal(err)
	}
	if do(t, relay, http.MethodPost, "/records", createBody).Code < 400 || guard.calls.Load() != 1 {
		t.Fatal("relay bypassed or retried upstream mutation refusal")
	}
	endpoints, err := backend.Records(t.Context())
	if err != nil || len(endpoints) != 0 {
		t.Fatal("refused relayed write changed records")
	}
}

func TestRelayBoundedCancellationAndShutdown(t *testing.T) {
	entered := make(chan struct{}, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		entered <- struct{}{}
		<-req.Context().Done()
	}))
	defer upstream.Close()
	relay, err := NewRelay(RelayOptions{Upstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- relay.Serve(ctx, ln) }()
	client := &http.Client{Timeout: 5 * time.Second}
	requests := make(chan struct{}, 2)
	for range 2 {
		go func() {
			res, err := client.Get("http://" + ln.Addr().String() + "/records")
			if err == nil {
				_ = res.Body.Close()
			}
			requests <- struct{}{}
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("relay request did not start")
		}
	}
	busy := do(t, relay, http.MethodGet, "/records", "")
	if busy.Code != 503 {
		t.Fatal("concurrency bound not enforced")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("relay shutdown failed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay shutdown did not join")
	}
	for range 2 {
		select {
		case <-requests:
		case <-time.After(time.Second):
			t.Fatal("request remained active after shutdown")
		}
	}
}

func TestRelayInputAndResponseBounds(t *testing.T) {
	for _, url := range []string{"", "http://remote.example", "https://user@server.example", "https://server.example?a=b", "https://server.example/#fragment", "https://server.example/a/../b", "https://server.example/%2f", "https://server.example"} {
		if _, err := NewRelay(RelayOptions{Upstream: url}); err == nil {
			t.Fatal("unsafe or unauthenticated upstream accepted")
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.Copy(w, io.LimitReader(infiniteRelayInput{}, MaxBodyBytes+1))
	}))
	defer upstream.Close()
	relay, err := NewRelay(RelayOptions{Upstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	if do(t, relay, http.MethodGet, "/records", "").Code != 502 {
		t.Fatal("oversized response accepted")
	}
	req := httptest.NewRequest(http.MethodPost, "/records", io.LimitReader(infiniteRelayInput{}, MaxBodyBytes+1))
	w := httptest.NewRecorder()
	relay.ServeHTTP(w, req)
	if w.Code != 413 {
		t.Fatal("oversized body accepted")
	}
}

type infiniteRelayInput struct{}

func (infiniteRelayInput) Read(p []byte) (int, error) { clear(p); return len(p), nil }
