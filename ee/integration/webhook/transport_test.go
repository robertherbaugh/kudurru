package webhook

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
)

type requestHost struct {
	mu        sync.Mutex
	mutations []bool
	accounts  []string
	before    func(context.Context) error
}

func (*requestHost) Logger() *slog.Logger           { return slog.New(slog.DiscardHandler) }
func (*requestHost) Metrics() prometheus.Registerer { return prometheus.NewRegistry() }
func (*requestHost) Resolve(context.Context, dns.Question) (*dns.Msg, error) {
	return nil, host.ErrUnsupported
}
func (*requestHost) SetCondition(context.Context, string, string, string, bool) error { return nil }
func (h *requestHost) Request(ctx context.Context, provider, account string, mutation bool, fn func(context.Context) error) error {
	// A bare transport charges the shared account; a provisioned instance
	// charges its own owner UID (D379).
	if provider != "webhook" || account != "all" && account != "original" {
		return host.ErrInvalid
	}
	h.mu.Lock()
	h.mutations = append(h.mutations, mutation)
	h.accounts = append(h.accounts, account)
	h.mu.Unlock()
	if h.before != nil {
		if err := h.before(ctx); err != nil {
			return err
		}
	}
	return fn(ctx)
}

func serverTransport(t *testing.T, h *requestHost, server *httptest.Server) *transport {
	t.Helper()
	tr, err := newTransport(h, transportConfig{endpoint: server.URL + "/provider", timeout: time.Second,
		bearer: []byte("explicit-test-token"), ca: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tr.close)
	return tr
}

type inspectRoundTrip struct {
	base http.RoundTripper
	bad  atomic.Bool
}

func (r *inspectRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.GetBody != nil || !req.Close {
		r.bad.Store(true)
	}
	return r.base.RoundTrip(req)
}

func TestTransportCapturedTLSAndOneRequestPerCharge(t *testing.T) {
	var mu sync.Mutex
	var peers []string
	var invalid atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		peers = append(peers, r.RemoteAddr)
		mu.Unlock()
		if r.ProtoMajor != 1 || r.Header.Get("Authorization") != "Bearer explicit-test-token" || r.Header.Get("Cookie") != "" || r.URL.RawQuery != "" || !r.Close {
			invalid.Store(true)
		}
		if r.Method == http.MethodPost {
			b, err := io.ReadAll(r.Body)
			if err != nil || string(b) != "{}" || r.URL.Path != "/provider/events" || r.Header.Get("X-Kudurru-Delivery") != "original-delivery" {
				invalid.Store(true)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", integrationMedia)
		_, _ = io.WriteString(w, "{}")
	}))
	defer server.Close()
	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxyCalls.Add(1) }))
	defer proxy.Close()
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("HTTP_PROXY", proxy.URL)
	h := &requestHost{}
	tr := serverTransport(t, h, server)
	inspect := &inspectRoundTrip{base: tr.client.Transport}
	tr.client.Transport = inspect
	read, err := tr.request(t.Context(), http.MethodGet, "/", "", nil, nil)
	if err != nil || read.media != integrationMedia || string(read.body) != "{}" {
		t.Fatalf("negotiation result: %v", err)
	}
	ack, err := tr.request(t.Context(), http.MethodPost, "/events", integrationMedia, []byte("{}"), http.Header{"X-Kudurru-Delivery": {"original-delivery"}})
	if err != nil || ack.status != http.StatusNoContent || len(ack.body) != 0 {
		t.Fatalf("wire acknowledgment: %v", err)
	}
	if inspect.bad.Load() || invalid.Load() || proxyCalls.Load() != 0 {
		t.Fatal("request transport changed captured framing or authority")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(peers) != 2 || peers[0] == peers[1] {
		t.Fatal("requests did not use two fresh connections")
	}
	if len(h.mutations) != 2 || h.mutations[0] || !h.mutations[1] {
		t.Fatal("host charges did not cover the exact read and mutation")
	}
}

func TestTransportRedirectAndStatusNeverReplay(t *testing.T) {
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var targetCalls, calls atomic.Int32
			target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls.Add(1) }))
			defer target.Close()
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Location", target.URL)
				w.Header().Set("Retry-After", "9999")
				w.WriteHeader(code)
				_, _ = io.WriteString(w, "private-provider-response")
			}))
			defer server.Close()
			tr := serverTransport(t, &requestHost{}, server)
			_, err := tr.request(t.Context(), http.MethodPost, "/records", externalMedia, []byte("{}"), nil)
			var failure *host.Failure
			if !errors.As(err, &failure) || strings.Contains(err.Error(), "private-provider-response") {
				t.Fatal("missing sanitized status failure")
			}
			want := host.FailurePermanent
			if code == http.StatusServiceUnavailable {
				want = host.FailureTemporary
			}
			if failure.Kind != want || code == http.StatusServiceUnavailable && failure.RetryAfter != 15*time.Minute {
				t.Fatal("wrong retry classification")
			}
			if calls.Load() != 1 || targetCalls.Load() != 0 {
				t.Fatal("mutation was redirected or replayed")
			}
		})
	}
}

func TestTransportOriginalContextThroughBodyAndStop(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel", "stop"} {
		t.Run(mode, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 32))
				w.Header().Set("Content-Type", integrationMedia)
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				close(entered)
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer server.Close()
			defer close(release)
			tr := serverTransport(t, &requestHost{}, server)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := context.Canceled
			if mode == "deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, time.Second)
				defer stop()
				want = context.DeadlineExceeded
			}
			done := make(chan error, 1)
			go func() {
				_, err := tr.request(ctx, http.MethodPost, "/events", integrationMedia, []byte("{}"), nil)
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("server never reached body barrier")
			}
			if mode == "cancel" {
				cancel()
			}
			if mode == "stop" {
				tr.close()
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatalf("original context outcome: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("request outlived original context")
			}
		})
	}
}

func TestTransportRefusedHostCannotReachPeer(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	tr := serverTransport(t, &requestHost{before: func(context.Context) error { return host.ErrFenced }}, server)
	if _, err := tr.request(t.Context(), http.MethodGet, "/", "", nil, nil); !errors.Is(err, host.ErrFenced) {
		t.Fatal("host refusal lost")
	}
	if calls.Load() != 0 {
		t.Fatal("peer reached after original host refusal")
	}
}

func TestTransportDetachesOriginalPayloadBeforeHostWait(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var original atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		original.Store(err == nil && string(b) == "original" && r.Header.Get("X-Kudurru-Delivery") == "first")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	h := &requestHost{before: func(ctx context.Context) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	tr := serverTransport(t, h, server)
	body := []byte("original")
	headers := http.Header{"X-Kudurru-Delivery": {"first"}}
	done := make(chan error, 1)
	go func() {
		_, err := tr.request(t.Context(), http.MethodPost, "/events", integrationMedia, body, headers)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("host wait not reached")
	}
	copy(body, "replaced")
	headers.Set("X-Kudurru-Delivery", "second")
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request did not finish")
	}
	if !original.Load() {
		t.Fatal("host wait adopted changed payload or delivery identity")
	}
}

func TestTransportCapturesMTLSMaterial(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "explicit-client"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("client certificate fixture")
	}
	var authenticated atomic.Bool
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authenticated.Store(r.TLS != nil && len(r.TLS.VerifiedChains) == 1 && r.TLS.PeerCertificates[0].Subject.CommonName == "explicit-client" && r.Header.Get("Authorization") == "")
		w.Header().Set("Content-Type", integrationMedia)
		_, _ = io.WriteString(w, "{}")
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	server.StartTLS()
	defer server.Close()
	tr, err := newTransport(&requestHost{}, transportConfig{endpoint: server.URL, certificate: certPEM, key: keyPEM, ca: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.close()
	clear(certPEM)
	clear(keyPEM)
	if _, err := tr.request(t.Context(), http.MethodGet, "/", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if !authenticated.Load() {
		t.Fatal("captured mutual TLS identity was not verified")
	}
}

func TestTransportInputAndResponseBounds(t *testing.T) {
	for _, endpoint := range []string{"http://provider.example", "https://user:pass@provider.example", "https://provider.example?x=1", "https://provider.example/a/../b", "https://provider.example/%2e%2e", "https://provider.example/#fragment"} {
		if _, err := newTransport(&requestHost{}, transportConfig{endpoint: endpoint, bearer: []byte("token")}); !errors.Is(err, errInput) {
			t.Fatal("invalid endpoint accepted")
		}
	}
	for _, token := range []string{"", "line\nbreak", "has space", "=", "a=b", "é"} {
		if _, err := newTransport(&requestHost{}, transportConfig{endpoint: "https://provider.example", bearer: []byte(token)}); !errors.Is(err, errInput) {
			t.Fatal("invalid bearer accepted")
		}
	}
	for _, mode := range []string{"length", "encoding", "duplicate-media"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", integrationMedia)
				switch mode {
				case "length":
					w.Header().Set("Content-Length", "33554433")
				case "encoding":
					w.Header().Set("Content-Encoding", "gzip")
				case "duplicate-media":
					w.Header().Add("Content-Type", externalMedia)
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			tr := serverTransport(t, &requestHost{}, server)
			if _, err := tr.request(t.Context(), http.MethodGet, "/", "", nil, nil); !errors.Is(err, errProtocol) {
				t.Fatalf("invalid response accepted: %v", err)
			}
		})
	}
}
