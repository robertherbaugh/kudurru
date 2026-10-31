package providerresources

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/browseroidc"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
)

func TestLogoutDiscoveryUsesOriginalEndpointTrust(t *testing.T) {
	f := newBrowserFixture(t)
	s := f.source(t, servingauth.ProviderObject)
	h := f.host(t, s)
	for _, endpoint := range []any{"https://ungranted.example/end", "http://169.254.169.254/latest", f.server.URL + "/end#fragment", "https://user@127.0.0.1/end", 123} {
		var m map[string]any
		_ = json.Unmarshal(f.discovery, &m)
		m["end_session_endpoint"] = endpoint
		raw, _ := json.Marshal(m)
		if _, err := parseMetadata(raw, s.Issuer, h.network); err == nil {
			t.Fatal("untrusted end-session discovery accepted")
		}
	}
	var m map[string]any
	_ = json.Unmarshal(f.discovery, &m)
	m["end_session_endpoint"] = f.server.URL + "/end"
	raw, _ := json.Marshal(m)
	parsed, err := parseMetadata(raw, s.Issuer, h.network)
	if err != nil || parsed.endSession != f.server.URL+"/end" {
		t.Fatal("trusted end-session discovery lost")
	}
	f.mu.Lock()
	f.discovery = raw
	f.mu.Unlock()
	owner, desired, lease, b := f.ready(t, servingauth.ProviderObject)
	defer lease.Release()
	if b.Info().EndSessionURL != parsed.endSession {
		t.Fatal("exact acquired endpoint not exposed")
	}
	f.mu.Lock()
	m["end_session_endpoint"] = f.server.URL + "/replacement"
	f.discovery, _ = json.Marshal(m)
	f.mu.Unlock()
	changed := owner.acquire(t.Context(), desired, desired.Sources[0], entry{})
	if changed.reason != "" || changed.binding.MetadataBinding == desired.Authorities[0].Binding.MetadataBinding {
		t.Fatal("replacement logout endpoint did not change original metadata trust binding")
	}

}

func TestLogoutRedirectOriginalCredentialTimeAndCapability(t *testing.T) {
	for _, mode := range []string{"current", "credential", "host", "resource", "released", "expired", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := newBrowserFixture(t)
			f.mu.Lock()
			var metadata map[string]any
			_ = json.Unmarshal(f.discovery, &metadata)
			metadata["end_session_endpoint"] = f.server.URL + "/end"
			f.discovery, _ = json.Marshal(metadata)
			f.mu.Unlock()
			owner, desired, lease, b := f.ready(t, servingauth.ProviderConfig)
			i := b.Info()
			hint, err := browseroidc.NewLogoutHint(browseroidc.LogoutBinding{Provider: i.Provider.ID, ProviderUID: i.Provider.UID, Origin: string(i.Provider.Origin), SpecIdentity: i.SpecIdentity, ResourceIdentity: i.ResourceIdentity, Issuer: i.Issuer, ClientID: i.ClientID, RedirectURI: i.RedirectURI}, strings.Repeat("A", 43), "private-hint", f.now(), f.now().Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			next, err := hint.Continue(f.now())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mode {
			case "credential":
				f.secret.Store("changed-test-only")
			case "host":
				var current atomic.Bool
				current.Store(true)
				b, _ = b.BindCurrent(current.Load)
				current.Store(false)
			case "resource":
				desired.RegistryDigest = hash("replacement-registry")
				if err := owner.SetDesired(t.Context(), desired); err != nil {
					t.Fatal(err)
				}
			case "released":
				lease.Release()
			case "expired":
				f.clock.Add(int64(time.Minute))
			case "cancel":
				cancel()
			}
			target, err := b.LogoutRedirect(ctx, next)
			if mode == "current" {
				if err != nil || !strings.HasPrefix(target, f.server.URL+"/end?") {
					t.Fatal("exact current redirect refused")
				}
			} else if err == nil || target != "" {
				t.Fatal("original credential/time/host refusal bypassed")
			}
			if f.requests.Load() != 0 {
				t.Fatal("logout performed a token request")
			}
		})
	}
}

func TestLogoutAbsentEndpointPreservesOriginalMetadataReceipt(t *testing.T) {
	m := metadata{issuer: "https://idp.example", authorization: "https://idp.example/authorize", token: "https://idp.example/token", jwksURL: "https://idp.example/keys", algorithms: []string{"EdDSA"}, browser: true} //nolint:gosec // G101: token is a public endpoint URL, not a credential.
	// Golden digest of the previously shipped v2 metadata receipt.
	const original = "1e682860a035046f88cab43e616bbaaeeb0ef07e27c16b4e94789c18f0c64c71"
	if metadataBinding(m) != original {
		t.Fatal("provider without logout support unnecessarily changed trust authority")
	}
	m.endSession = "https://idp.example/end"
	withLogout := metadataBinding(m)
	if withLogout == original {
		t.Fatal("new logout endpoint was not bound")
	}
	m.endSession = "https://idp.example/new-end"
	if metadataBinding(m) == withLogout {
		t.Fatal("changed logout endpoint retained old authority")
	}
}
