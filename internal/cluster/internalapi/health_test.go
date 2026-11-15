package internalapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthAuthenticatedPeers(t *testing.T) {
	for _, ready := range []bool{true, false} {
		t.Run(map[bool]string{true: "ready", false: "unready"}[ready], func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if !ready {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
				_, _ = io.WriteString(w, `{"status":"`+map[bool]string{true: "ready", false: "not-ready"}[ready]+`","components":{"dns":{"ready":`+map[bool]string{true: "true", false: "false"}[ready]+`,"message":"peer listener"}},"version":"peer-only"}`)
			})
			peer := startNode(t, CredentialOptions{Key: key(7), NodeName: "node-b"}, ServerOptions{
				Health: func(ctx context.Context) (Health, error) { return LocalHealth(ctx, "node-b", handler) },
			})
			creds, err := NewCredentials(CredentialOptions{Key: key(7), NodeName: "node-a"})
			if err != nil {
				t.Fatal(err)
			}
			got, err := NewClient(creds, ClientOptions{}).Health(context.Background(), peer.addr, "node-b")
			if err != nil {
				t.Fatal(err)
			}
			if got.Node != "node-b" || got.Ready != ready || got.Health["version"] != "peer-only" {
				t.Fatalf("health = %+v", got)
			}
			comp := got.Health["components"].(map[string]any)["dns"].(map[string]any)
			if comp["ready"] != ready || comp["message"] != "peer listener" {
				t.Fatalf("components = %+v", comp)
			}
		})
	}
}

func TestHealthGuard(t *testing.T) {
	var calls atomic.Int32
	peer := startNode(t, CredentialOptions{Key: key(7), NodeName: "node-b"}, ServerOptions{
		Health: func(context.Context) (Health, error) {
			calls.Add(1)
			return Health{}, nil
		},
	})
	creds, err := NewCredentials(CredentialOptions{Key: key(7), NodeName: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	creds.tokens = [][]byte{[]byte("incorrect-peer-token")}
	if _, err := NewClient(creds, ClientOptions{}).Health(context.Background(), peer.addr, "node-b"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong token: %v", err)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, PathHealth, nil)
		peer.server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s: %d", method, rec.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unauthenticated request evaluated health")
	}
}

func TestHealthUnsupportedAndUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(context.Context) (Health, error)
		want error
	}{
		{name: "older peer", want: ErrHealthUnsupported},
		{name: "checker failure", fn: func(context.Context) (Health, error) { return Health{}, errors.New("private detail") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := startNode(t, CredentialOptions{Key: key(7), NodeName: "node-b"}, ServerOptions{Health: tc.fn})
			_, err := peer.client.Health(context.Background(), peer.addr, "node-b")
			if err == nil || tc.want != nil && !errors.Is(err, tc.want) || strings.Contains(err.Error(), "private detail") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestHealthRejectsInvalidResponses(t *testing.T) {
	valid := `{"node":"node-b","ready":true,"health":{"status":"ready","components":{}}}`
	for _, tc := range []struct {
		name, body, certNode string
		contentLength        int
		want                 error
	}{
		{name: "malformed", body: `{"node":`, want: ErrHealthInvalid},
		{name: "trailing document", body: valid + `{}`, want: ErrHealthInvalid},
		{name: "missing readiness", body: `{"node":"node-b","health":{"status":"ready","components":{}}}`, want: ErrHealthInvalid},
		{name: "null health", body: `{"node":"node-b","ready":true,"health":null}`, want: ErrHealthInvalid},
		{name: "empty health", body: `{"node":"node-b","ready":true,"health":{}}`, want: ErrHealthInvalid},
		{name: "inconsistent health", body: `{"node":"node-b","ready":true,"health":{"status":"not-ready","components":{}}}`, want: ErrHealthInvalid},
		{name: "wrong node", body: strings.Replace(valid, "node-b", "node-c", 1), want: ErrHealthIdentity},
		{name: "wrong certificate", body: valid, certNode: "node-c", want: ErrHealthIdentity},
		{name: "oversized", body: valid + strings.Repeat(" ", maxHealthBytes), want: ErrHealthTooLarge},
		{name: "oversized content length", contentLength: maxHealthBytes + 1, want: ErrHealthTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := tc.certNode
			if name == "" {
				name = "node-b"
			}
			creds, err := NewCredentials(CredentialOptions{Key: key(7), NodeName: name})
			if err != nil {
				t.Fatal(err)
			}
			peer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != PathHealth || !creds.ValidToken(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) {
					t.Error("request did not use authenticated health endpoint")
				}
				if tc.contentLength > 0 {
					w.Header().Set("Content-Length", strconv.Itoa(tc.contentLength))
					w.WriteHeader(http.StatusOK)
					return
				}
				// Flushing omits Content-Length, exercising the streaming size cap.
				w.(http.Flusher).Flush()
				_, _ = io.WriteString(w, tc.body)
			}))
			peer.TLS = creds.ServerTLS()
			peer.StartTLS()
			defer peer.Close()
			_, err = NewClient(creds, ClientOptions{}).Health(context.Background(), strings.TrimPrefix(peer.URL, "https://"), "node-b")
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v; want %v", err, tc.want)
			}
		})
	}
}

func TestHealthDeadlineAndCancellation(t *testing.T) {
	creds, err := NewCredentials(CredentialOptions{Key: key(7), NodeName: "node-b"})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 2)
	peer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Start a valid document, then stall its body to exercise the total deadline.
		_, _ = io.WriteString(w, `{"node":"node-b"`)
		w.(http.Flusher).Flush()
		started <- struct{}{}
		<-r.Context().Done()
	}))
	peer.TLS = creds.ServerTLS()
	peer.StartTLS()
	defer peer.Close()
	client := NewClient(creds, ClientOptions{})
	addr := strings.TrimPrefix(peer.URL, "https://")
	t.Run("total deadline", func(t *testing.T) {
		start := time.Now()
		_, err := client.Health(context.Background(), addr, "node-b")
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3500*time.Millisecond {
			t.Fatalf("deadline: %v after %v", err, time.Since(start))
		}
		<-started
	})
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := client.Health(ctx, addr, "node-b"); done <- err }()
		<-started
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("cancellation did not stop body read")
		}
	})
}

func TestHealthRedirectAndAddress(t *testing.T) {
	var redirected atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer other.Close()
	creds, err := NewCredentials(CredentialOptions{Key: key(7), NodeName: "node-b"})
	if err != nil {
		t.Fatal(err)
	}
	peer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	peer.TLS = creds.ServerTLS()
	peer.StartTLS()
	defer peer.Close()
	client := NewClient(creds, ClientOptions{})
	if _, err := client.Health(context.Background(), strings.TrimPrefix(peer.URL, "https://"), "node-b"); err == nil {
		t.Fatal("redirect accepted")
	}
	if redirected.Load() != 0 {
		t.Fatal("credentials or request forwarded on redirect")
	}
	for _, addr := range []string{"https://127.0.0.1:8082", "user@127.0.0.1:8082", "127.0.0.1:8082/path", "127.0.0.1:8082?query", "127.0.0.1:0", ":8082", "127.0.0.1:65536"} {
		if _, err := client.Health(context.Background(), addr, "node-b"); !errors.Is(err, ErrHealthInvalid) {
			t.Fatalf("invalid address %q: %v", addr, err)
		}
	}
}

func TestHealthLocalSemanticsAndBounds(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"status":"not-ready","components":{},"unready":["lameduck"]}`)
	})
	got, err := LocalHealth(context.Background(), "node-b", handler)
	if err != nil || got.Ready || got.Health["status"] != "not-ready" {
		t.Fatalf("unready health: %+v, %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LocalHealth(ctx, "node-b", handler); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled local health: %v", err)
	}
	large := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", maxHealthBytes+1))
	})
	if _, err := LocalHealth(context.Background(), "node-b", large); !errors.Is(err, ErrHealthTooLarge) {
		t.Fatalf("large local health: %v", err)
	}
	peer := startNode(t, CredentialOptions{Key: key(7), NodeName: "node-b"}, ServerOptions{Health: func(context.Context) (Health, error) {
		return Health{Node: "node-b", Health: map[string]any{"large": strings.Repeat("x", maxHealthBytes)}}, nil
	}})
	if _, err := peer.client.Health(context.Background(), peer.addr, "node-b"); err == nil {
		t.Fatal("server published oversized health")
	}
	if routeOf(PathHealth) != "health" {
		t.Fatal("health metrics route is unbounded")
	}
}
