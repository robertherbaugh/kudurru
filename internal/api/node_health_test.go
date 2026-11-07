package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/health"
)

func healthPeerCredentials(t *testing.T) *internalapi.Credentials {
	t.Helper()
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{7}, 32), NodeName: "node-b"})
	if err != nil {
		t.Fatal(err)
	}
	return creds
}

func TestNodeHealthAuthenticatedRemote(t *testing.T) {
	for _, ready := range []bool{true, false} {
		t.Run(map[bool]string{true: "ready", false: "unready"}[ready], func(t *testing.T) {
			creds := healthPeerCredentials(t)
			checker := health.New(map[string]any{"version": "remote-only"})
			checker.Set("remote-dns", ready, "remote listener")
			peer, err := internalapi.NewServer(internalapi.ServerOptions{Credentials: creds,
				Health: func(ctx context.Context) (internalapi.Health, error) {
					return internalapi.LocalHealth(ctx, "node-b", checker.Handler())
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != internalapi.PathHealth || r.Method != http.MethodGet || !creds.ValidToken(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) {
					t.Error("internal request did not use the authenticated health route")
				}
				if r.Header.Get("Cookie") != "" || r.Header.Get("X-Caller-Secret") != "" {
					t.Error("public caller headers forwarded to peer")
				}
				peer.Handler().ServeHTTP(w, r)
			}))
			ts.TLS = creds.ServerTLS()
			ts.StartTLS()
			defer ts.Close()
			h := newHarness(t, func(o *Options) {
				o.Health = health.New(map[string]any{"version": "local-only"})
				o.Health.Set("local-dns", !ready, "local listener")
				o.InternalClient = internalapi.NewClient(creds, internalapi.ClientOptions{})
				o.Membership = staticMembership{peers: []cluster.Peer{{Name: "node-a", Self: true}, {Name: "node-b", Address: strings.TrimPrefix(ts.URL, "https://")}}}
			})
			var got NodeHealth
			h.do(http.MethodGet, "/api/v1/nodes/node-b/health", "admin", nil, "X-Caller-Secret", "not-forwarded", "Cookie", "public-session=not-forwarded").want(http.StatusOK).json(&got)
			if got.Node != "node-b" || got.Ready != ready || got.Health["version"] != "remote-only" || calls.Load() != 1 {
				t.Fatalf("remote health = %+v, calls = %d", got, calls.Load())
			}
			components := got.Health["components"].(map[string]any)
			if len(components) != 1 || components["remote-dns"].(map[string]any)["ready"] != ready {
				t.Fatalf("remote health mixed with local components: %+v", components)
			}
			for _, user := range []string{"viewer", "auditor", "operator", "zoneadmin", "nobody", ""} {
				code := http.StatusForbidden
				if user == "" {
					code = http.StatusUnauthorized
				}
				h.do(http.MethodGet, "/api/v1/nodes/node-b/health", user, nil).want(code)
			}
			h.do(http.MethodGet, "/api/v1/nodes/node-z/health", "admin", nil).want(http.StatusNotFound)
			h.do(http.MethodGet, "/api/v1/nodes/node-a/health", "admin", nil).want(http.StatusOK).json(&got)
			if got.Node != "node-a" || got.Ready == ready || got.Health["version"] != "local-only" || calls.Load() != 1 {
				t.Fatalf("self, denied or unknown health contacted peer: %+v, calls = %d", got, calls.Load())
			}
		})
	}
}

type healthMembershipFailure struct{ cluster.Membership }

func (healthMembershipFailure) Peers(context.Context) ([]cluster.Peer, error) {
	return nil, errors.New("private membership endpoint and credential detail")
}

func TestNodeHealthDiscoveryAndMissingTransport(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*Options)
		want string
	}{
		{name: "membership", want: "node health: membership unavailable", mod: func(o *Options) {
			o.Membership = healthMembershipFailure{Membership: cluster.Self{Name: "node-a"}}
		}},
		{name: "no client", want: "peer health: unavailable", mod: func(o *Options) {
			o.Membership = staticMembership{peers: []cluster.Peer{{Name: "node-b", Address: "127.0.0.1:8082"}}}
		}},
		{name: "no address", want: "peer health: unavailable", mod: func(o *Options) {
			o.InternalClient = internalapi.NewClient(healthPeerCredentials(t), internalapi.ClientOptions{})
			o.Membership = staticMembership{peers: []cluster.Peer{{Name: "node-b"}}}
		}},
		{name: "self alias", want: "peer health: unavailable", mod: func(o *Options) {
			o.InternalClient = internalapi.NewClient(healthPeerCredentials(t), internalapi.ClientOptions{})
			o.Membership = staticMembership{peers: []cluster.Peer{{Name: "node-b", Self: true, Address: "127.0.0.1:8082"}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.mod)
			got := h.do(http.MethodGet, "/api/v1/nodes/node-b/health", "admin", nil).want(http.StatusServiceUnavailable)
			if got.errKey() != KeyUnavailable || got.m()["error"].(map[string]any)["message"] != tc.want {
				t.Fatalf("error = %s", got.Body.String())
			}
			// Self health never depends on discovery or peer transport.
			h.do(http.MethodGet, "/api/v1/nodes/node-a/health", "admin", nil).want(http.StatusOK)
		})
	}
}

func TestNodeHealthPeerFailuresRedacted(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
		want string
	}{
		{name: "wrong auth", code: http.StatusUnauthorized, body: "private credential detail", want: "peer health: unavailable"},
		{name: "unavailable", code: http.StatusServiceUnavailable, body: "private endpoint detail", want: "peer health: unavailable"},
		{name: "older peer", code: http.StatusNotFound, want: "peer health: unsupported"},
		{name: "malformed", code: http.StatusOK, body: "private malformed detail", want: "peer health: invalid response"},
		{name: "wrong identity", code: http.StatusOK, body: `{"node":"node-c","ready":true,"health":{"status":"ready","components":{}}}`, want: "peer health: invalid response"},
		{name: "oversized", code: http.StatusOK, body: strings.Repeat("x", (1<<20)+1), want: "peer health: invalid response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			creds := healthPeerCredentials(t)
			peer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			}))
			peer.TLS = creds.ServerTLS()
			peer.StartTLS()
			defer peer.Close()
			h := newHarness(t, func(o *Options) {
				o.InternalClient = internalapi.NewClient(creds, internalapi.ClientOptions{})
				o.Membership = staticMembership{peers: []cluster.Peer{{Name: "node-b", Address: strings.TrimPrefix(peer.URL, "https://")}}}
			})
			got := h.do(http.MethodGet, "/api/v1/nodes/node-b/health", "admin", nil).want(http.StatusServiceUnavailable)
			if got.errKey() != KeyUnavailable || got.m()["error"].(map[string]any)["message"] != tc.want || strings.Contains(got.Body.String(), "private") || strings.Contains(got.Body.String(), "127.0.0.1") {
				t.Fatalf("error = %s", got.Body.String())
			}
		})
	}
}

func TestNodeHealthUnreachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(o *Options) {
		o.InternalClient = internalapi.NewClient(healthPeerCredentials(t), internalapi.ClientOptions{})
		o.Membership = staticMembership{peers: []cluster.Peer{{Name: "node-b", Address: addr}}}
	})
	got := h.do(http.MethodGet, "/api/v1/nodes/node-b/health", "admin", nil).want(http.StatusServiceUnavailable)
	if got.m()["error"].(map[string]any)["message"] != "peer health: connection refused" {
		t.Fatalf("unreachable peer = %s", got.Body.String())
	}
}

func TestNodeHealthRequestCancellationAndTimeout(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(map[bool]string{true: "canceled", false: "timeout"}[cancelRequest], func(t *testing.T) {
			creds := healthPeerCredentials(t)
			started := make(chan struct{})
			peer := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				close(started)
				<-r.Context().Done()
			}))
			peer.TLS = creds.ServerTLS()
			peer.StartTLS()
			defer peer.Close()
			h := newHarness(t, func(o *Options) {
				o.InternalClient = internalapi.NewClient(creds, internalapi.ClientOptions{})
				o.Membership = staticMembership{peers: []cluster.Peer{{Name: "node-b", Address: strings.TrimPrefix(peer.URL, "https://")}}}
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan response, 1)
			go func() {
				done <- h.serve(h.req(http.MethodGet, "/api/v1/nodes/node-b/health", "admin", nil).WithContext(ctx))
			}()
			<-started
			want := "peer health: timeout"
			if cancelRequest {
				cancel()
				want = "peer health: canceled"
			}
			select {
			case got := <-done:
				got.want(http.StatusServiceUnavailable)
				if got.m()["error"].(map[string]any)["message"] != want {
					t.Fatalf("error = %s", got.Body.String())
				}
			case <-time.After(3500 * time.Millisecond):
				t.Fatal("public request exceeded peer deadline")
			}
		})
	}
}
