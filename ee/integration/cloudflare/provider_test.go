package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
)

func TestSDKCheckTokenStatusAndSanitizedFailuresNoRetries(t *testing.T) {
	t.Setenv("CLOUDFLARE_API_TOKEN", "unwanted-env-token")
	t.Setenv("CLOUDFLARE_API_KEY", "unwanted-env-key")
	t.Setenv("CLOUDFLARE_EMAIL", "unwanted-env-email")
	for _, mode := range []string{"active", "revoked", "401", "403", "429", "500", "false-envelope", "missing-envelope"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.mu.Lock()
			f.tokenStatus = mode
			start := f.requests
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasSuffix(r.URL.Path, "/verify") {
					return false
				}
				switch mode {
				case "401":
					w.WriteHeader(401)
				case "403":
					w.WriteHeader(403)
				case "429":
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(429)
				case "500":
					w.WriteHeader(500)
				case "false-envelope":
					writeJSON(w, map[string]any{"success": false, "errors": []string{testToken}, "result": map[string]any{"status": "active"}})
					return true
				case "missing-envelope":
					writeJSON(w, map[string]any{"result": map[string]any{"status": "active"}})
					return true
				default:
					return false
				}
				_, _ = w.Write([]byte(testToken))
				return true
			}
			f.mu.Unlock()
			status, err := f.p.Check(f.ctx)
			if mode == "active" {
				if err != nil || !status.Authenticated || !status.Reachable {
					t.Fatal(status, err)
				}
			} else {
				if err == nil || status.Authenticated || strings.Contains(err.Error(), testToken) {
					t.Fatal("unsafe status", status, err)
				}
				var failure *host.Failure
				if !errors.As(err, &failure) {
					t.Fatal("unsanitized", err)
				}
				if mode == "429" && (failure.Kind != host.FailureRateLimited || failure.RetryAfter.Seconds() != 60) {
					t.Fatal(failure)
				}
			}
			f.mu.Lock()
			requests := f.requests - start
			f.mu.Unlock()
			if mode != "active" && requests != 1 {
				t.Fatal("SDK auto retry", requests)
			}
			f.h.mu.Lock()
			calls := f.h.calls
			f.h.mu.Unlock()
			f.mu.Lock()
			total := f.requests
			f.mu.Unlock()
			if calls != total {
				t.Fatal("network attempt bypassed host", calls, total)
			}
		})
	}
}
func TestOfflineStrictConfigAndCapabilityChecks(t *testing.T) {
	base := newFixture(t)
	for _, raw := range []string{`{}`, `null`, `{"zones":[]}`, `{"zones":[{"zone":"example.com","unexpected":1}]}`, `{"zones":[{"zone":"example.com"}]} {}`, `{"zones":[{"zone":"example.com"},{"zone":"EXAMPLE.COM."}]}`, `{"zones":[{"zone":"example.com","zoneID":"bad"}]}`, `{"zones":[{"zone":"example.com","policy":"delete-all"}]}`, `{"zones":[{"zone":"example.com","defaults":{"ttl":2}}]}`, `{"zones":[{"zone":"example.com","defaults":{"comment":"kudurru:fake"}}]}`, `{"zones":[{"zone":"example.com","adoptFrom":{"externalDNSOwner":""}}]}`, `{"zones":[{"zone":"example.com","select":{"types":["ANY"]}}]}`, `{"zones":[{"zone":"example.com","select":{"exclude":["bad*pattern"]}}]}`, `{"zones":[{"zone":"example.com","translate":{"192.0.2.0/24":"198.51.100.0/25"}}]}`} {
		p := New()
		err := p.Provision(base.ctx, base.h, json.RawMessage(raw), secretFunc(func(context.Context, string) (string, error) { return testToken, nil }))
		if err == nil {
			err = p.Validate(base.ctx)
		}
		if err == nil {
			t.Fatal("invalid config accepted", raw)
		}
		_ = p.Stop(context.Background())
	}
	p := New()
	if err := p.Provision(base.ctx, struct{ integration.Host }{base.h}, json.RawMessage(`{"zones":[{"zone":"example.com"}]}`), secretFunc(func(context.Context, string) (string, error) { return testToken, nil })); !errors.Is(err, host.ErrUnsupported) {
		t.Fatal("missing OwnerHost accepted", err)
	}
	for _, token := range []string{"", " contains-space", "contains\nnewline"} {
		p := New()
		if err := p.Provision(base.ctx, base.h, json.RawMessage(`{"zones":[{"zone":"example.com"}]}`), secretFunc(func(context.Context, string) (string, error) { return token, nil })); !errors.Is(err, host.ErrCredentials) {
			t.Fatal("invalid named token", err)
		}
	}
	base.mu.Lock()
	defer base.mu.Unlock()
	if base.requests != 1 {
		t.Fatal("offline validation performed network I/O", base.requests)
	}
}
func TestSDKZoneResolutionAndImmutableSelections(t *testing.T) {
	z := public.CloudflareZone{Zone: "EXAMPLE.COM", Select: public.PublicationSelection{Labels: map[string]string{"publish": "yes"}, Types: []string{"A"}, Exclude: []string{"*.private.example.com"}}}
	f := newFixture(t, z)
	if f.pub.Destination.Zone != zoneID || f.pub.Destination.Account != accountID || f.pub.Domains[0] != "example.com." {
		t.Fatal(f.pub)
	}
	f.pub.Selection.Labels["publish"] = "changed"
	f.pub.Selection.Types[0] = "AAAA"
	f.pub.Selection.Exclude[0] = "*"
	again, err := f.p.Destinations(f.ctx)
	if err != nil || again[0].Selection.Labels["publish"] != "yes" || again[0].Selection.Types[0] != "A" {
		t.Fatal("borrowed configuration escaped", again, err)
	}
	f.mu.Lock()
	f.zoneName = "other.example.com"
	f.mu.Unlock()
	if _, err = f.p.Destinations(f.ctx); err == nil {
		t.Fatal("zone name mismatch accepted")
	}
}
func TestSDKRefusesRedirectAndUntrustedTLS(t *testing.T) {
	for _, mode := range []string{"redirect", "tls"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			var hits atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { hits.Add(1); t.Error("credential redirect escaped") }))
			defer target.Close()
			if mode == "redirect" {
				f.mu.Lock()
				f.hook = func(w http.ResponseWriter, r *http.Request) bool {
					http.Redirect(w, r, target.URL, http.StatusFound)
					return true
				}
				f.mu.Unlock()
				if _, err := f.p.Check(f.ctx); err == nil {
					t.Fatal("redirect accepted")
				}
			} else {
				tls := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { t.Error("untrusted TLS reached handler") }))
				defer tls.Close()
				p := New()
				p.apiURL = tls.URL + "/"
				if err := p.Provision(f.ctx, f.h, json.RawMessage(`{"zones":[{"zone":"example.com"}]}`), secretFunc(func(context.Context, string) (string, error) { return testToken, nil })); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = p.Stop(context.Background()) }()
				if _, err := p.Check(f.ctx); err == nil {
					t.Fatal("untrusted TLS accepted")
				}
			}
			if hits.Load() != 0 {
				t.Fatal("followed redirect")
			}
		})
	}
}
