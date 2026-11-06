package webhook

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/credentials"
)

func TestPublicConstructorAdvertisesOnlyMountedCapabilities(t *testing.T) {
	manifest, constructor, ok := integration.Lookup("webhook")
	if !ok || constructor == nil || slices.Contains(manifest.Capabilities, integration.CapHook) || slices.Contains(manifest.Capabilities, integration.CapRecordSink) || !slices.Contains(manifest.Capabilities, integration.CapActions) || !slices.Contains(manifest.Capabilities, integration.CapClientSource) {
		t.Fatal("registry advertised unmounted capabilities")
	}
	p := constructor()
	defer func() { _ = p.Stop(t.Context()) }()
	if !slices.Equal(p.Manifest().Capabilities, manifest.Capabilities) {
		t.Fatal("constructor disagrees with registry")
	}
}

func TestPublicConstructorRefusesUnmountedPeerCapabilities(t *testing.T) {
	for _, capability := range []string{"hooks", "records"} {
		t.Run(capability, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", integrationMedia)
				d := actionDeclaration()
				d.Capabilities = []string{capability}
				d.Manifest.Capabilities = []integration.Capability{integration.CapHook}
				if capability == "records" {
					d.Manifest.Capabilities = []integration.Capability{integration.CapRecordSink}
				}
				_ = json.NewEncoder(w).Encode(d)
			}))
			defer server.Close()
			p := New()
			defer func() { _ = p.Stop(t.Context()) }()
			values := map[string]string{"bearerToken": "explicit-test-token", "ca": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))}
			raw, _ := json.Marshal(map[string]string{"endpoint": server.URL})
			err := p.Provision(t.Context(), &lifecycleHost{}, raw, namedResolver(func(_ context.Context, name string) (string, error) {
				if name == "hmacKey" {
					t.Error("public constructor acquired unused delivery credential")
				}
				if v, ok := values[name]; ok {
					return v, nil
				}
				return "", credentials.ErrDenied
			}))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = p.Check(t.Context()); !errors.Is(err, host.ErrUnsupported) {
				t.Fatal("unmounted peer capability did not refuse", err)
			}
		})
	}
}
