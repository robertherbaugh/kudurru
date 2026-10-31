package externaldns

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func webhookSecretFile(t *testing.T) (string, string) {
	t.Helper()
	value := rand.Text()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(" \n"+value+"\r\n"), 0o600); err != nil {
		t.Fatal("could not prepare test secret file")
	}
	return path, value
}

func authenticatedWebhook(t *testing.T) (*Server, string) {
	t.Helper()
	base, _ := newTestServer(t)
	opts := base.opts
	path, token := webhookSecretFile(t)
	opts.TokenFile = path
	s, err := New(opts)
	if err != nil {
		t.Fatal("could not initialize authenticated test webhook")
	}
	return s, token
}

type webhookBodyProbe struct{ reads atomic.Int32 }

func (r *webhookBodyProbe) Read([]byte) (int, error) {
	r.reads.Add(1)
	return 0, io.EOF
}

func TestWebhookTokenRejectsBeforeDispatch(t *testing.T) {
	s, token := authenticatedWebhook(t)
	var logs bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&logs, nil))
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/"},
		{http.MethodGet, "/records"},
		{http.MethodPost, "/records"},
		{http.MethodPost, "/adjustendpoints"},
		{http.MethodGet, "/unknown"},
		{http.MethodPut, "/records"},
	} {
		for name, values := range map[string][]string{
			"missing":        nil,
			"empty":          {""},
			"wrong":          {"Bearer " + rand.Text()},
			"raw":            {token},
			"basic":          {"Basic " + token},
			"no token":       {"Bearer"},
			"empty token":    {"Bearer "},
			"spaces":         {"Bearer  " + token},
			"trailing token": {"Bearer " + token + " another"},
			"comma":          {"Bearer " + token + ",Bearer " + token},
			"duplicate":      {"Bearer " + token, "Bearer " + token},
			"control":        {"Bearer " + token + "\x00"},
			"oversized":      {"Bearer " + strings.Repeat("x", maxTokenFileBytes+1)},
		} {
			t.Run(route.method+route.path+"/"+name, func(t *testing.T) {
				probe := &webhookBodyProbe{}
				req := httptest.NewRequest(route.method, route.path+"?token="+token, probe)
				for _, value := range values {
					req.Header.Add("Authorization", value)
				}
				// Neither a query token nor another header may bypass bearer auth.
				req.Header.Set("X-Webhook-Token", token)
				rec := httptest.NewRecorder()
				s.ServeHTTP(rec, req)
				if rec.Code != http.StatusUnauthorized || rec.Body.String() != "unauthorized\n" || rec.Header().Get("WWW-Authenticate") == "" || probe.reads.Load() != 0 {
					t.Fatal("unauthorized request reached a route or consumed its body")
				}
			})
		}
	}
	if strings.Contains(logs.String(), token) {
		t.Fatal("authentication logged the credential")
	}
	sets, err := s.Records(t.Context())
	if err != nil || len(sets) != 0 {
		t.Fatal("denied requests changed webhook records")
	}
}

func TestWebhookTokenAuthenticatedContract(t *testing.T) {
	s, token := authenticatedWebhook(t)
	for _, route := range []struct {
		method, path, body string
		code               int
	}{
		{http.MethodGet, "/", "", http.StatusOK},
		{http.MethodPost, "/adjustendpoints", "[]", http.StatusOK},
		{http.MethodPost, "/records", createBody, http.StatusNoContent},
		{http.MethodGet, "/records", "", http.StatusOK},
		{http.MethodPost, "/records", "malformed", http.StatusBadRequest},
	} {
		req := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
		req.Header.Set("Authorization", "bEaReR "+token)
		req.Header.Set("Content-Type", MediaType)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != route.code {
			t.Fatalf("authenticated %s %s: status %d, want %d", route.method, route.path, rec.Code, route.code)
		}
		if route.code == http.StatusOK && rec.Header().Get("Content-Type") != MediaType {
			t.Fatal("authentication changed the webhook protocol media type")
		}
	}
	eps, err := s.Records(t.Context())
	if err != nil || len(eps) != 3 {
		t.Fatal("authenticated webhook did not retain its normal record behavior")
	}
}

type refusingWebhookStore struct {
	store.Store
	calls atomic.Int32
}

func (s *refusingWebhookStore) Apply(context.Context, store.Object, store.ApplyOptions) (store.Object, error) {
	s.calls.Add(1)
	return nil, errors.New("current write authority unavailable")
}

func TestWebhookTokenDoesNotGrantWriteAuthority(t *testing.T) {
	s, token := authenticatedWebhook(t)
	guard := &refusingWebhookStore{Store: s.opts.Store}
	s.opts.Store = guard
	req := httptest.NewRequest(http.MethodPost, "/records", strings.NewReader(createBody))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code < http.StatusBadRequest || guard.calls.Load() != 1 {
		t.Fatal("transport authentication bypassed or replayed a refused store mutation")
	}
	eps, err := s.Records(t.Context())
	if err != nil || len(eps) != 0 {
		t.Fatal("refused mutation changed records")
	}
}

func TestWebhookTokenFileValidation(t *testing.T) {
	base, _ := newTestServer(t)
	for name, data := range map[string][]byte{
		"empty":               nil,
		"whitespace":          []byte(" \r\n\t"),
		"short":               []byte("short"),
		"oversized":           bytes.Repeat([]byte("x"), maxTokenFileBytes+1),
		"embedded space":      []byte(strings.Repeat("a", 20) + " " + strings.Repeat("b", 20)),
		"control":             append([]byte(strings.Repeat("a", 20)), 0),
		"comma":               []byte(strings.Repeat("a", 20) + ","),
		"padding before text": []byte("=" + strings.Repeat("a", 20)),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal("could not prepare invalid test file")
			}
			opts := base.opts
			opts.TokenFile = path
			if got, err := New(opts); got != nil || !errors.Is(err, errTokenFile) {
				t.Fatal("invalid configured file did not fail webhook initialization")
			}
		})
	}
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(t.TempDir(), "missing"), t.TempDir(), fifo} {
		opts := base.opts
		opts.TokenFile = path
		if got, err := New(opts); got != nil || !errors.Is(err, errTokenFile) {
			t.Fatal("missing or special configured file did not fail initialization")
		}
	}
	path, _ := webhookSecretFile(t)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	opts := base.opts
	opts.TokenFile = path
	if got, err := New(opts); got != nil || !errors.Is(err, errTokenFile) {
		t.Fatal("unreadable configured file did not fail initialization")
	}
}

func TestWebhookTokenStartupSnapshotAndConfigWiring(t *testing.T) {
	base, _ := newTestServer(t)
	path, oldToken := webhookSecretFile(t)
	link := filepath.Join(t.TempDir(), "mounted-token")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "kudurru.yaml")
	if err := os.WriteFile(cfgPath, []byte("externalDNS:\n  webhook:\n    enabled: true\n    tokenFile: "+link+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	// This is the exact configuration-to-constructor path used by runServe.
	opts := OptionsFrom(cfg.ExternalDNS.Webhook)
	opts.Store, opts.Validator, opts.Logger = base.opts.Store, base.opts.Validator, base.opts.Logger
	s, err := New(opts)
	if err != nil {
		t.Fatal("production configuration did not initialize authentication")
	}
	check := func(s *Server, token string, code int) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/records", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != code {
			t.Fatalf("startup credential snapshot: status %d, want %d", rec.Code, code)
		}
	}
	newToken := rand.Text()
	if err := os.WriteFile(path, []byte(newToken+"\n"), 0o600); err != nil {
		t.Fatal("could not rotate test credential")
	}
	check(s, oldToken, http.StatusOK)
	check(s, newToken, http.StatusUnauthorized)
	restarted, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	check(restarted, newToken, http.StatusOK)
	check(restarted, oldToken, http.StatusUnauthorized)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	check(restarted, newToken, http.StatusOK)
	if got, err := New(opts); got != nil || !errors.Is(err, errTokenFile) {
		t.Fatal("broken mounted token did not refuse restart")
	}
}

func TestWebhookTokenHTTPListener(t *testing.T) {
	s, token := authenticatedWebhook(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{}
	client := &http.Client{Transport: transport, Timeout: time.Second}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	defer func() {
		// Release this test's client connections before starting server shutdown.
		transport.CloseIdleConnections()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("webhook listener did not shut down cleanly")
			}
		case <-time.After(6 * time.Second):
			t.Error("webhook listener did not stop")
		}
	}()
	for _, authenticated := range []bool{false, true} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String()+"/records", nil)
		if err != nil {
			t.Fatal(err)
		}
		if authenticated {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal("local webhook request failed")
		}
		// Reading to EOF lets the transport finish and reuse the connection
		// rather than racing another dial against the shutdown below.
		_, drainErr := io.Copy(io.Discard, resp.Body)
		closeErr := resp.Body.Close()
		if drainErr != nil || closeErr != nil {
			t.Fatal("could not finish local webhook response")
		}
		want := http.StatusUnauthorized
		if authenticated {
			want = http.StatusOK
		}
		if resp.StatusCode != want {
			t.Fatalf("HTTP auth: status %d, want %d", resp.StatusCode, want)
		}
	}
}
