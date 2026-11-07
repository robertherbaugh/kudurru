package unifi

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
)

const fixtureKey = "fixture-api-key"

type loanHost struct {
	owner   host.InstanceID
	loans   atomic.Int32
	request func(context.Context, func(context.Context) error) error
}

func (h *loanHost) IntegrationOwner() host.InstanceID { return h.owner }
func (*loanHost) Logger() *slog.Logger                { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func (*loanHost) Metrics() prometheus.Registerer      { return nil }
func (*loanHost) Resolve(context.Context, dns.Question) (*dns.Msg, error) {
	return nil, host.ErrUnsupported
}
func (*loanHost) SetCondition(context.Context, string, string, string, bool) error { return nil }
func (h *loanHost) Request(ctx context.Context, provider, account string, mutating bool, fn func(context.Context) error) error {
	if provider != "unifi" || len(account) != 64 || mutating {
		return host.ErrInvalid
	}
	h.loans.Add(1)
	if h.request != nil {
		return h.request(ctx, fn)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(ctx)
}

type fixtureSecrets struct{}

func (fixtureSecrets) Resolve(_ context.Context, name string) (string, error) {
	if name != "apiKey" {
		return "", host.ErrCredentials
	}
	return fixtureKey, nil
}

func newTLSFixture(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewUnstartedServer(handler)
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}
func provisionFixture(t *testing.T, s *httptest.Server, c public.UniFiIntegration, h *loanHost) *Provider {
	t.Helper()
	c.Endpoint = s.URL
	p := New()
	p.transport = s.Client().Transport.(*http.Transport)
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Provision(context.Background(), h, raw, fixtureSecrets{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return p
}
func fixtureLoan() *loanHost { return &loanHost{owner: translationFixture().Owner} }

func TestStrictEndpointAndOfflineConfiguration(t *testing.T) {
	for _, raw := range []string{"http://controller.example", "https://user:pass@controller.example", "https://controller.example?key=x", "https://controller.example/#x", "https://controller.example/v1", "https://controller.example/proxy/network/integration/v2", "https://controller.example/%70roxy/network/integration", "https://controller.example/proxy/network/integration/../integration", " https://controller.example", "https://controller.example//"} {
		if _, _, err := endpoint(raw); err == nil {
			t.Errorf("accepted unsupported endpoint %q", raw)
		}
	}
	for _, raw := range []string{"https://controller.example", "https://controller.example/", "https://controller.example/proxy/network/integration/v1", "https://api.ui.com/v1/connector/consoles/id:one/proxy/network/integration/v1"} {
		if _, _, err := endpoint(raw); err != nil {
			t.Errorf("rejected supported endpoint %q: %v", raw, err)
		}
	}
	var calls atomic.Int32
	s := newTLSFixture(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	p := provisionFixture(t, s, public.UniFiIntegration{}, fixtureLoan())
	if err := p.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("offline provision/validate made network calls")
	}
	raw, _ := json.Marshal(public.UniFiIntegration{Endpoint: s.URL, Push: public.UniFiPush{DHCPDNSServers: true}})
	if err := New().Provision(context.Background(), fixtureLoan(), raw, fixtureSecrets{}); !errors.Is(err, host.ErrUnsupported) {
		t.Fatal("unsupported push did not fail visibly", err)
	}
}

func TestTransportOriginalContextCancellationAndNoHiddenRetries(t *testing.T) {
	type contextKey struct{}
	for _, mode := range []string{"success", "deadline", "stop", "fence", "budget"} {
		t.Run(mode, func(t *testing.T) {
			entered := make(chan struct{})
			var calls atomic.Int32
			s := newTLSFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.Header.Get("X-API-Key") != fixtureKey {
					t.Error("wrong authentication/method")
				}
				close(entered)
				if mode == "deadline" || mode == "stop" {
					<-r.Context().Done()
					return
				}
				_, _ = io.WriteString(w, `{"ok":true}`)
			})
			h := fixtureLoan()
			h.request = func(ctx context.Context, fn func(context.Context) error) error {
				if ctx.Value(contextKey{}) != "original" {
					t.Error("scope rebound")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Error("original deadline lost")
				}
				if mode == "fence" {
					return host.ErrFenced
				}
				if mode == "budget" {
					return ledger.ErrBudget
				}
				return fn(ctx)
			}
			p := provisionFixture(t, s, public.UniFiIntegration{}, h)
			ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), contextKey{}, "original"), 100*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := p.get(ctx, &pollBudget{}, "/info"); done <- err }()
			if mode == "stop" {
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal("request not started")
				}
				if err := p.Stop(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			select {
			case err = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("request failed to quiesce")
			}
			switch mode {
			case "success":
				if err != nil {
					t.Fatal(err)
				}
			case "deadline":
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			case "stop":
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case "fence":
				if !errors.Is(err, host.ErrFenced) {
					t.Fatal(err)
				}
			case "budget":
				var f *host.Failure
				if !errors.As(err, &f) || f.Kind != host.FailureRateLimited {
					t.Fatal(err)
				}
			}
			want := int32(1)
			if mode == "fence" || mode == "budget" {
				want = 0
			}
			if calls.Load() != want || h.loans.Load() != 1 {
				t.Fatal("attempt bypassed host or retried", calls.Load(), h.loans.Load())
			}
		})
	}
}

func TestTransportBoundsCompressionRedirectAndErrors(t *testing.T) {
	for _, mode := range []string{"oversized", "gzip_bomb", "gzip_invalid", "gzip_valid", "unsupported_encoding", "redirect", "401", "403", "429", "503"} {
		t.Run(mode, func(t *testing.T) {
			var escaped atomic.Int32
			target := newTLSFixture(t, func(http.ResponseWriter, *http.Request) { escaped.Add(1) })
			s := newTLSFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				switch mode {
				case "oversized":
					_, _ = io.WriteString(w, strings.Repeat("x", maxResponse+1))
				case "gzip_bomb", "gzip_valid":
					w.Header().Set("Content-Encoding", "gzip")
					g := gzip.NewWriter(w)
					body := `{"ok":true}`
					if mode == "gzip_bomb" {
						body = strings.Repeat("x", maxResponse+1)
					}
					_, _ = io.WriteString(g, body)
					_ = g.Close()
				case "gzip_invalid":
					w.Header().Set("Content-Encoding", "gzip")
					_, _ = io.WriteString(w, "invalid")
				case "unsupported_encoding":
					w.Header().Set("Content-Encoding", "br")
					_, _ = io.WriteString(w, "x")
				case "redirect":
					w.Header().Set("Location", target.URL)
					w.WriteHeader(302)
				case "401":
					w.WriteHeader(401)
				case "403":
					w.WriteHeader(403)
				case "429":
					w.Header().Set("Retry-After", "999999")
					w.WriteHeader(429)
				case "503":
					w.WriteHeader(503)
				}
			})
			p := provisionFixture(t, s, public.UniFiIntegration{}, fixtureLoan())
			body, err := p.get(context.Background(), &pollBudget{}, "/info")
			switch mode {
			case "gzip_valid":
				if err != nil || !bytes.Equal(body, []byte(`{"ok":true}`)) {
					t.Fatal(err)
				}
			case "oversized", "gzip_bomb":
				if !errors.Is(err, ErrCapacity) {
					t.Fatal(err)
				}
			case "gzip_invalid", "unsupported_encoding":
				if !errors.Is(err, ErrInvalid) {
					t.Fatal(err)
				}
			default:
				var f *host.Failure
				if !errors.As(err, &f) {
					t.Fatal(err)
				}
				want := host.FailurePermanent
				if mode == "429" {
					want = host.FailureRateLimited
					if f.RetryAfter != 15*time.Minute {
						t.Fatal("unbounded retry", f)
					}
				}
				if mode == "503" {
					want = host.FailureTemporary
				}
				if f.Kind != want {
					t.Fatal(f)
				}
			}
			if escaped.Load() != 0 {
				t.Fatal("credential origin escaped via redirect")
			}
		})
	}
	p := New()
	p.host = fixtureLoan()
	p.client = &http.Client{}
	p.bucket = strings.Repeat("a", 64)
	if _, err := p.get(context.Background(), &pollBudget{attempts: maxAttempts}, "/info"); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	if after := statusFailure(429, time.Now().Add(24*time.Hour).UTC().Format(http.TimeFormat), time.Now()).RetryAfter; after > 15*time.Minute {
		t.Fatal(after)
	}
}

func TestTLSNormalTrustAndPinStillRequireHostnameAndTime(t *testing.T) {
	s := newTLSFixture(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) })
	sum := sha256.Sum256(s.Certificate().Raw)
	pin := "sha256:" + hex.EncodeToString(sum[:])
	for _, mode := range []string{"trusted_ca", "untrusted_ca", "valid_pin", "wrong_pin", "wrong_hostname", "expired", "not_yet_valid"} {
		t.Run(mode, func(t *testing.T) {
			p := New()
			c := public.UniFiIntegration{Endpoint: s.URL}
			if mode == "trusted_ca" {
				p.transport = s.Client().Transport.(*http.Transport)
			}
			if mode != "trusted_ca" && mode != "untrusted_ca" {
				c.TLSFingerprint = pin
			}
			if mode == "wrong_pin" {
				c.TLSFingerprint = "sha256:" + strings.Repeat("0", 64)
			}
			if mode == "wrong_hostname" {
				p.transport = s.Client().Transport.(*http.Transport).Clone()
				p.transport.TLSClientConfig = &tls.Config{ServerName: "wrong.example", MinVersion: tls.VersionTLS12}
				c.Endpoint = strings.Replace(s.URL, "127.0.0.1", "localhost", 1)
			}
			// httptest's certificate contains example.com and 127.0.0.1, but not localhost.
			if mode == "expired" {
				p.now = func() time.Time { return s.Certificate().NotAfter }
			}
			if mode == "not_yet_valid" {
				p.now = func() time.Time { return s.Certificate().NotBefore.Add(-time.Second) }
			}
			raw, _ := json.Marshal(c)
			if err := p.Provision(context.Background(), fixtureLoan(), raw, fixtureSecrets{}); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = p.Stop(context.Background()) }()
			_, err := p.get(context.Background(), &pollBudget{}, "/info")
			good := mode == "trusted_ca" || mode == "valid_pin"
			if good && err != nil || !good && err == nil {
				t.Fatal("TLS contract violated", err)
			}
		})
	}
}

// controllerFixture implements only the first-party10.6.106 read contract.
// It is never a production admission path.
type controllerFixture struct {
	mu      sync.Mutex
	clients []clientRow
	version string
	calls   map[string]int
	hook    func(http.ResponseWriter, *http.Request, int) bool
}

func controller(t *testing.T) (*httptest.Server, *controllerFixture) {
	t.Helper()
	at := translationFixture().Observation.ObservedAt.Add(-time.Hour)
	f := &controllerFixture{version: "10.6.106", clients: []clientRow{{ID: clientID, Name: "Laptop", Type: "WIRED", MACAddress: "02:00:00:00:00:01", IPAddress: "192.0.2.3", ConnectedAt: &at}}, calls: map[string]int{}}
	return newTLSFixture(t, f.serve), f
}
func (f *controllerFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[r.URL.Path]++
	call := f.calls[r.URL.Path]
	if r.Method != "GET" || r.Header.Get("X-API-Key") != fixtureKey {
		w.WriteHeader(403)
		return
	}
	if f.hook != nil && f.hook(w, r, call) {
		return
	}
	base := "/proxy/network/integration/v1"
	switch r.URL.Path {
	case base + "/info":
		_ = json.NewEncoder(w).Encode(map[string]any{"applicationVersion": f.version})
	case base + "/sites":
		fixturePage(w, r, []map[string]string{{"id": siteID, "internalReference": "default", "name": "Display name"}})
	case base + "/sites/" + siteID + "/clients":
		fixturePage(w, r, f.clients)
	case base + "/sites/" + siteID + "/networks":
		fixturePage(w, r, []map[string]any{{"id": networkID, "name": "LAN", "vlanId": 10, "enabled": true, "management": "GATEWAY"}})
	case base + "/sites/" + siteID + "/networks/" + networkID:
		_ = json.NewEncoder(w).Encode(map[string]any{"id": networkID, "name": "LAN", "vlanId": 10, "enabled": true, "management": "GATEWAY", "ipv4Configuration": map[string]any{"hostIpAddress": "192.0.2.1", "prefixLength": 24, "autoScaleEnabled": false, "dhcpConfiguration": map[string]string{"mode": "SERVER", "domainName": "lan.example"}}})
	default:
		w.WriteHeader(404)
	}
}
func fixturePage[T any](w http.ResponseWriter, r *http.Request, rows []T) {
	if rows == nil {
		rows = []T{}
	}
	var offset int
	_, _ = fmt.Sscan(r.URL.Query().Get("offset"), &offset)
	limit := pageSize
	if offset < 0 || offset > len(rows) {
		w.WriteHeader(400)
		return
	}
	end := min(len(rows), offset+limit)
	_ = json.NewEncoder(w).Encode(map[string]any{"offset": offset, "limit": limit, "count": end - offset, "totalCount": len(rows), "data": rows[offset:end]})
}

func TestEndpointNormalizationKeepsOneBudgetIdentity(t *testing.T) {
	for _, raw := range []string{"https://CONTROLLER.EXAMPLE.:443/", "https://controller.example:0443/proxy/network/integration/v1", "https://controller.example"} {
		origin, prefix, err := endpoint(raw)
		if err != nil || origin != "https://controller.example" || prefix != "/proxy/network/integration" {
			t.Fatal("equivalent endpoints split budget identity", raw, origin, prefix, err)
		}
	}
	for _, raw := range []string{"https://controller.example:0", "https://controller.example:65536", "https://controller.example:", "https://[fe80::1%25eth0]", "https://controller_bad.example"} {
		if _, _, err := endpoint(raw); err == nil {
			t.Fatal("invalid HTTPS origin accepted", raw)
		}
	}
	origin, _, err := endpoint("https://[2001:db8::1]:443")
	if err != nil || origin != "https://[2001:db8::1]" {
		t.Fatal(origin, err)
	}
}
