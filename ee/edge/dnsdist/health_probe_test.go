package dnsdist

import (
	"context"
	"errors"
	"testing"

	"github.com/robertherbaugh/kudurru/ee/edge"
)

func TestHealthCapabilityStrictOwnershipAndCompatibility(t *testing.T) {
	credentials, err := edge.NewCredentials()
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte(credentials.Password)
	supervisor := &Supervisor{cfg: Config{Options: edge.Options{ProxyProtocolSecret: string(secret)}}}
	current := startLabBackend(t, "same-owner", 1, secret)
	legacy := startLabBackend(t, "same-owner", 2, secret, "legacy")
	wildcard := startLabBackend(t, "same-owner", 3, secret, "wildcard")
	for _, tc := range []struct {
		name    string
		backend *labBackend
		want    healthCapability
	}{
		{"versioned", current, healthTargetV1}, {"legacy new address same owner", legacy, healthLegacy}, {"generic wildcard is not target proof", wildcard, healthLegacy}, {"current requalified after legacy address", current, healthTargetV1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := edge.Backend{Name: tc.backend.endpoint.Name, Address: tc.backend.endpoint.Address}
			got, err := supervisor.classifyHealth(t.Context(), be)
			if err != nil || got != tc.want {
				t.Fatalf("capability=%d error=%v", got, err)
			}
		})
	}
	malformed := startLabBackend(t, "same-owner", 4, secret, "invalid-target")
	if _, err := supervisor.classifyHealth(t.Context(), edge.Backend{Name: "same-owner", Address: malformed.endpoint.Address}); !errors.Is(err, errHealthProof) {
		t.Fatal("wrong targeted owner was silently downgraded to legacy")
	}
	wrong := edge.Backend{Name: "wrong-source-owner", Address: current.endpoint.Address}
	if _, err := supervisor.classifyHealth(t.Context(), wrong); !errors.Is(err, errHealthProof) {
		t.Fatal("generic wrong owner was downgraded to legacy")
	}
	// Authentication failures must happen before the mounted ingress handler.
	before := current.checks.Load()
	for _, credential := range []string{"", "incorrect-proxy-secret-for-test-only"} {
		caller := &Supervisor{cfg: Config{Options: edge.Options{ProxyProtocolSecret: credential}}}
		_, _, err := caller.healthProbe(context.Background(), edge.Backend{Address: current.endpoint.Address}, "health.kudurru.internal.")
		if !errors.Is(err, errHealthProof) {
			t.Fatal("missing/wrong original PROXY authentication accepted")
		}
	}
	if current.checks.Load() != before {
		t.Fatal("unauthenticated health query reached captured owner handler")
	}
}
