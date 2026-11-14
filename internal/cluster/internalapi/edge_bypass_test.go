package internalapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func edgeBypassFixture() EdgeBypassState {
	return EdgeBypassState{Version: 1, Node: "edge-b", Deployment: digestOf([]byte("deployment")), Run: digestOf([]byte("run")),
		SourceEpoch: 3, ChildEpoch: 2, Challenge: digestOf([]byte("original request")),
		Backends: []EdgeBypassBackend{{Name: "node-a", Address: "192.0.2.1:5353", InternalAddress: "192.0.2.1:8082"}}}
}

func TestEdgeBypassAuthenticatedOriginalRequest(t *testing.T) {
	want := edgeBypassFixture()
	var calls atomic.Int32
	peer := startNode(t, CredentialOptions{Key: key(7), NodeName: "edge-b"}, ServerOptions{
		EdgeBypass: func(ctx context.Context, deployment string) (EdgeBypassState, error) {
			calls.Add(1)
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > edgeBypassTimeout || deployment != want.Deployment {
				return EdgeBypassState{}, ErrEdgeBypassUnavailable
			}
			return want, nil
		},
	})
	got, err := peer.client.EdgeBypass(t.Context(), peer.addr, "edge-b", want.Deployment)
	if err != nil || got.Deployment != want.Deployment || got.Run != want.Run || got.ChildEpoch != want.ChildEpoch ||
		got.SourceEpoch != want.SourceEpoch || len(got.Backends) != 1 || got.Backends[0] != want.Backends[0] || got.Challenge == want.Challenge {
		t.Fatal("authenticated observation lost original identity or cut", err)
	}
	if _, err := peer.client.EdgeBypass(t.Context(), peer.addr, "edge-b", digestOf([]byte("other deployment"))); err == nil {
		t.Fatal("foreign deployment was observed")
	}
	if _, err := peer.client.EdgeBypass(t.Context(), peer.addr, "edge-c", want.Deployment); err == nil {
		t.Fatal("different authenticated node was accepted")
	}
	before := calls.Load()
	for _, query := range []string{"", "?deployment=x&challenge=y", "?deployment=" + want.Deployment + "&challenge=" + want.Challenge + "&challenge=" + want.Challenge} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", PathEdgeBypass+query, nil)
		req.Header.Set("Authorization", "Bearer "+peer.creds.Token())
		peer.server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatal("ambiguous request entered observer", rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	peer.server.Handler().ServeHTTP(rec, httptest.NewRequest("GET", PathEdgeBypass, nil))
	if rec.Code != http.StatusUnauthorized || calls.Load() != before {
		t.Fatal("request guard evaluated observation")
	}
	missing := startNode(t, CredentialOptions{Key: key(7), NodeName: "old-edge"}, ServerOptions{})
	if _, err := peer.client.EdgeBypass(t.Context(), missing.addr, "old-edge", want.Deployment); !errors.Is(err, ErrEdgeBypassUnsupported) {
		t.Fatal("missing capability did not refuse", err)
	}
}

func TestEdgeBypassRefusesResponseSubstitution(t *testing.T) {
	for _, name := range []string{"wrong-challenge", "wrong-deployment", "wrong-node", "missing-run", "missing-child", "missing-source", "empty-backends", "duplicate-backend", "many-backends", "invalid-endpoint", "new-version", "extra-field", "trailing-document", "oversized"} {
		t.Run(name, func(t *testing.T) {
			creds, err := NewCredentials(CredentialOptions{Key: key(7), NodeName: "edge-b"})
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			peer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != PathEdgeBypass || !creds.ValidToken(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) {
					t.Error("request bypassed authenticated endpoint")
				}
				out := edgeBypassFixture()
				out.Challenge = r.URL.Query().Get("challenge")
				switch name {
				case "wrong-challenge":
					out.Challenge = digestOf([]byte("another request"))
				case "wrong-deployment":
					out.Deployment = digestOf([]byte("another deployment"))
				case "wrong-node":
					out.Node = "another-edge"
				case "missing-run":
					out.Run = ""
				case "missing-child":
					out.ChildEpoch = 0
				case "missing-source":
					out.SourceEpoch = 0
				case "empty-backends":
					out.Backends = nil
				case "duplicate-backend":
					out.Backends = append(out.Backends, out.Backends[0])
				case "many-backends":
					out.Backends = make([]EdgeBypassBackend, MaxEdgeBypassBackends+1)
				case "invalid-endpoint":
					out.Backends[0].Address = "https://foreign.example/path"
				case "new-version":
					out.Version = 2
				}
				b, err := json.Marshal(out)
				if err != nil {
					t.Error(err)
					return
				}
				switch name {
				case "extra-field":
					b = append(b[:len(b)-1], []byte(",\"granted\":true}")...)
				case "trailing-document":
					b = append(b, []byte("{}")...)
				case "oversized":
					b = append(b, []byte(strings.Repeat(" ", maxEdgeBypassBytes))...)
				}
				w.(http.Flusher).Flush()
				_, _ = w.Write(b)
			}))
			peer.TLS = creds.ServerTLS()
			peer.StartTLS()
			defer peer.Close()
			_, err = NewClient(creds, ClientOptions{}).EdgeBypass(t.Context(), strings.TrimPrefix(peer.URL, "https://"), "edge-b", edgeBypassFixture().Deployment)
			want := ErrEdgeBypassInvalid
			if name == "oversized" {
				want = ErrTooLarge
			}
			if !errors.Is(err, want) || calls.Load() != 1 {
				t.Fatal("invalid original response was accepted or replayed", err, calls.Load())
			}
		})
	}
}

func TestEdgeBypassOriginalLifetimeAndRedirect(t *testing.T) {
	creds, err := NewCredentials(CredentialOptions{Key: key(7), NodeName: "edge-b"})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	entered := make(chan struct{}, 2)
	peer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"version":1`)
		w.(http.Flusher).Flush()
		entered <- struct{}{}
		<-r.Context().Done()
	}))
	peer.TLS = creds.ServerTLS()
	peer.StartTLS()
	defer peer.Close()
	client := NewClient(creds, ClientOptions{})
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
		done := make(chan error, 1)
		go func() {
			_, err := client.EdgeBypass(ctx, strings.TrimPrefix(peer.URL, "https://"), "edge-b", edgeBypassFixture().Deployment)
			done <- err
		}()
		<-entered
		want := context.DeadlineExceeded
		if canceled {
			cancel()
			want = context.Canceled
		}
		select {
		case err := <-done:
			if !errors.Is(err, want) {
				t.Error("body acquisition lost original lifetime", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("body acquisition extended original lifetime")
		}
		cancel()
	}
	if calls.Load() != 2 {
		t.Fatal("expired acquisition was retried")
	}
	var targetCalls atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	redirect := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	redirect.TLS = creds.ServerTLS()
	redirect.StartTLS()
	defer redirect.Close()
	if _, err := client.EdgeBypass(t.Context(), strings.TrimPrefix(redirect.URL, "https://"), "edge-b", edgeBypassFixture().Deployment); err == nil || targetCalls.Load() != 0 {
		t.Fatal("redirect forwarded original peer authority", err)
	}
}
