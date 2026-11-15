package replica

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

type replicaDeadlineRecorder struct {
	*httptest.ResponseRecorder
	read, write time.Time
}

func (w *replicaDeadlineRecorder) SetReadDeadline(deadline time.Time) error {
	w.read = deadline
	return nil
}

func (w *replicaDeadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	w.write = deadline
	return nil
}

func admissionRequest(ctx context.Context, server *Server) *http.Request {
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "https://node"+PathImage, nil)
	r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13}
	r.Header.Set("Authorization", "Bearer "+server.opts.Credentials.Token())
	r.Header.Set(HeaderMember, "enrolled-peer")
	r.Header.Set(HeaderCluster, server.opts.Store.Authority().Term.ClusterID)
	return r
}

func TestPeerAdmissionRetainsOriginalDeadline(t *testing.T) {
	store, creds := openStore(t, replication.Primary), credentials(t, 1)
	for _, earlier := range []bool{false, true} {
		t.Run(map[bool]string{false: "configured", true: "earlier-original"}[earlier], func(t *testing.T) {
			original := t.Context()
			if earlier {
				var cancel context.CancelFunc
				original, cancel = context.WithTimeout(original, 5*time.Second)
				defer cancel()
			}
			writer := &replicaDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			var authorized context.Context
			server, err := NewServer(ServerOptions{Store: store, Credentials: creds, Timeout: 10 * time.Second,
				AuthorizePeer: func(ctx context.Context, _ Peer, r *http.Request) error {
					deadline, ok := ctx.Deadline()
					if !ok || r.Context() != ctx || writer.read != deadline || writer.write != deadline {
						t.Error("authorization did not receive the original transport bound")
					}
					if originalDeadline, ok := original.Deadline(); ok && deadline.After(originalDeadline) {
						t.Error("authorization extended the original deadline")
					}
					authorized = ctx
					return nil
				}})
			if err != nil {
				t.Fatal(err)
			}
			dispatched := false
			server.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				dispatched = true
				if r.Context() != authorized {
					t.Error("dispatch replaced the authorized context")
				}
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(writer, admissionRequest(original, server))
			if !dispatched || authorized == nil || authorized.Err() != context.Canceled {
				t.Fatal("admitted request did not dispatch and release its original scope")
			}
		})
	}
}

func TestPeerAdmissionCancellationRefusesDispatch(t *testing.T) {
	store, creds := openStore(t, replication.Primary), credentials(t, 1)
	for _, before := range []bool{false, true} {
		t.Run(map[bool]string{false: "during-authorization", true: "before-authorization"}[before], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if before {
				cancel()
			}
			calls := 0
			server, err := NewServer(ServerOptions{Store: store, Credentials: creds,
				AuthorizePeer: func(context.Context, Peer, *http.Request) error {
					calls++
					cancel()
					return nil // A late success is not permission to continue.
				}})
			if err != nil {
				t.Fatal(err)
			}
			writer := &replicaDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			server.guard(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("canceled peer authorization reached dispatch")
			})).ServeHTTP(writer, admissionRequest(ctx, server))
			if writer.Code != http.StatusServiceUnavailable || (before && calls != 0) || (!before && calls != 1) {
				t.Fatal("cancellation did not retain its original admission boundary", writer.Code, calls)
			}
		})
	}
}

func TestPeerAdmissionAuthorizationTimeout(t *testing.T) {
	store, creds := openStore(t, replication.Primary), credentials(t, 1)
	for _, lateSuccess := range []bool{false, true} {
		t.Run(map[bool]string{false: "refused", true: "late-success"}[lateSuccess], func(t *testing.T) {
			expired := false
			server, err := NewServer(ServerOptions{Store: store, Credentials: creds, Timeout: 50 * time.Millisecond,
				AuthorizePeer: func(ctx context.Context, _ Peer, _ *http.Request) error {
					select {
					case <-ctx.Done():
						expired = ctx.Err() == context.DeadlineExceeded
					case <-time.After(5 * time.Second):
					}
					if lateSuccess {
						return nil
					}
					return ErrUnauthorized
				}})
			if err != nil {
				t.Fatal(err)
			}
			writer := &replicaDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			server.guard(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("expired authorizer dispatched work")
			})).ServeHTTP(writer, admissionRequest(t.Context(), server))
			want := http.StatusUnauthorized
			if lateSuccess {
				want = http.StatusServiceUnavailable
			}
			if !expired || writer.Code != want {
				t.Fatal("server timeout did not bound peer authorization", expired, writer.Code)
			}
		})
	}
}

func TestPeerAdmissionConcurrentTLSRequests(t *testing.T) {
	store, creds := openStore(t, replication.Primary), credentials(t, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var calls, dispatched atomic.Int32
	server, err := NewServer(ServerOptions{Store: store, Credentials: creds, MaxConcurrent: 1, Timeout: 5 * time.Second,
		AuthorizePeer: func(ctx context.Context, _ Peer, _ *http.Request) error {
			if calls.Add(1) == 1 {
				close(entered)
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	address := tlsServer(t, creds, server.guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dispatched.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})))
	transport := &http.Transport{TLSClientConfig: creds.ClientTLS(), ForceAttemptHTTP2: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	request := func() (int, error) {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+address+PathImage, nil)
		if err != nil {
			return 0, err
		}
		r.Header = admissionRequest(t.Context(), server).Header
		response, err := client.Do(r)
		if err != nil {
			return 0, err
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return response.StatusCode, nil
	}
	type outcome struct {
		code int
		err  error
	}
	first := make(chan outcome, 1)
	go func() { code, err := request(); first <- outcome{code, err} }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first peer never entered authorization")
	}
	if code, err := request(); err != nil || code != http.StatusServiceUnavailable || calls.Load() != 1 || dispatched.Load() != 0 {
		t.Fatal("concurrency bound was bypassed by peer authorization", code, err, calls.Load())
	}
	once.Do(func() { close(release) })
	select {
	case got := <-first:
		if got.err != nil || got.code != http.StatusNoContent {
			t.Fatal("admitted first request failed", got.code, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first authorization did not join")
	}
	if code, err := request(); err != nil || code != http.StatusNoContent || calls.Load() != 2 || dispatched.Load() != 2 {
		t.Fatal("completed request did not release its admission slot", code, err)
	}
}
