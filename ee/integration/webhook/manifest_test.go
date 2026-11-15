package webhook

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/credentials"
)

func TestPublicConstructorAdvertisesOnlyMountedCapabilities(t *testing.T) {
	manifest, constructor, ok := integration.Lookup("webhook")
	// Record publication is mounted with the TXT registry (D380).
	if !ok || constructor == nil || !slices.Contains(manifest.Capabilities, integration.CapHook) || !slices.Contains(manifest.Capabilities, integration.CapRecordSink) || !slices.Contains(manifest.Capabilities, integration.CapActions) || !slices.Contains(manifest.Capabilities, integration.CapClientSource) {
		t.Fatal("registry hid a mounted capability")
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(manifest.ConfigSchema, &schema) != nil || schema.Properties["domains"] == nil || schema.Properties["policy"] == nil {
		t.Fatal("config schema does not name the publication fields")
	}
	if !slices.ContainsFunc(manifest.SecretKeys, func(k integration.SecretKey) bool { return k.Path == "hmacKey" }) {
		t.Fatal("registry does not name the event signing key")
	}
	p := constructor()
	defer func() { _ = p.Stop(t.Context()) }()
	if !slices.Equal(p.Manifest().Capabilities, manifest.Capabilities) {
		t.Fatal("constructor disagrees with registry")
	}
}

// Hooks are mounted (D379): a hooks peer with the separate signing key is
// accepted and gets exactly the produced topics; without the key it refuses,
// and a records-only peer refuses while no domains are configured (D380).
func TestPublicConstructorPeerCapabilities(t *testing.T) {
	for _, tc := range []struct {
		capability string
		hmac       bool
		accepted   bool
	}{
		{capability: "hooks", hmac: true, accepted: true},
		{capability: "hooks"},
		{capability: "records", hmac: true},
	} {
		t.Run(fmt.Sprintf("%s/hmac=%t", tc.capability, tc.hmac), func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", integrationMedia)
				d := actionDeclaration()
				d.Capabilities = []string{tc.capability}
				d.Manifest.Capabilities = []integration.Capability{integration.CapHook}
				if tc.capability == "records" {
					d.Manifest.Capabilities = []integration.Capability{integration.CapRecordSink}
				}
				_ = json.NewEncoder(w).Encode(d)
			}))
			defer server.Close()
			p := New()
			defer func() { _ = p.Stop(t.Context()) }()
			values := map[string]string{"bearerToken": "explicit-test-token", "ca": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))}
			if tc.hmac {
				values["hmacKey"] = strings.Repeat("k", 32)
			}
			raw, _ := json.Marshal(map[string]string{"endpoint": server.URL})
			if err := p.Provision(t.Context(), &lifecycleHost{}, raw, namedResolver(func(_ context.Context, name string) (string, error) {
				if v, ok := values[name]; ok {
					return v, nil
				}
				return "", credentials.ErrDenied
			})); err != nil {
				t.Fatal(err)
			}
			_, err := p.Check(t.Context())
			if !tc.accepted && tc.capability == "records" {
				// A records-only peer without domains passes Check, so a Delete
				// cleanup can still remove what it owns, and is refused by
				// Roles, which every ordinary lease asks before it schedules.
				if err != nil {
					t.Fatal("records-only peer failed the identity check", err)
				}
				if _, err := p.(*provider).Roles(); !errors.Is(err, host.ErrUnsupported) {
					t.Fatal("records-only peer without domains got roles", err)
				}
				return
			}
			if !tc.accepted {
				if !errors.Is(err, host.ErrUnsupported) {
					t.Fatal("unmounted peer capability did not refuse", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			hooks := p.(*provider)
			if !slices.Equal(hooks.Topics(), []string{"zone.changed", "record.changed", "blocking.toggled", "integration.error", "cluster.leader_changed"}) {
				t.Fatal("advertised topics root does not produce", hooks.Topics())
			}
			if roles, err := hooks.Roles(); err != nil || roles != (host.Roles{Hook: true}) {
				t.Fatal("hooks peer roles", roles, err)
			}
		})
	}
}

func TestProvisionRefusesHMACKeyOutsideBounds(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	for _, n := range []int{31, maxHMACKey + 1} {
		p := New()
		values := map[string]string{"bearerToken": "explicit-test-token", "hmacKey": strings.Repeat("k", n)}
		raw, _ := json.Marshal(map[string]string{"endpoint": server.URL})
		err := p.Provision(t.Context(), &lifecycleHost{}, raw, namedResolver(func(_ context.Context, name string) (string, error) {
			if v, ok := values[name]; ok {
				return v, nil
			}
			return "", credentials.ErrDenied
		}))
		_ = p.Stop(t.Context())
		if !errors.Is(err, host.ErrCredentials) {
			t.Fatal("key length accepted", n, err)
		}
	}
}
