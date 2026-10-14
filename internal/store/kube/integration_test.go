package kube

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestIntegrationCRDAdmissionAndStatus(t *testing.T) {
	requireEnv(t)
	s := newTestStore(t, Options{})
	i := &v1alpha1.Integration{
		ObjectMeta: metav1.ObjectMeta{Name: "inventory-admission", Namespace: "default"},
		Spec: v1alpha1.IntegrationSpec{Type: "unifi",
			Credentials: map[string]v1alpha1.CredentialReference{"apiKey": {SecretRef: &v1alpha1.CredentialSecretRef{Name: "unifi", Key: "token"}}},
			UniFi:       &v1alpha1.UniFiIntegration{Endpoint: "https://network.example"}},
	}
	if _, err := s.Apply(t.Context(), i, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	var saved v1alpha1.Integration
	key := store.Key{Kind: "Integration", Namespace: i.Namespace, Name: i.Name}
	if err := s.Get(t.Context(), key, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Spec.DeletionPolicy != "Retain" || saved.Spec.UniFi.Push.DHCPDNSServers || saved.Spec.UniFi.Push.MirrorRecords.Enabled {
		t.Fatal("unsafe integration defaults")
	}
	invalid := saved.DeepCopy()
	invalid.Spec.Type = "cloudflare"
	if _, err := s.Apply(t.Context(), invalid, store.ApplyOptions{Manager: store.ManagerAPI}); err == nil {
		t.Fatal("apiserver accepted mismatched provider or missing credential")
	}
	saved.Status.Clients = 7
	if err := s.UpdateStatus(t.Context(), &saved); err != nil {
		t.Fatal(err)
	}
	if err := s.Get(t.Context(), key, &saved); err != nil || saved.Status.Clients != 7 {
		t.Fatalf("status was not persisted: %v", err)
	}
}
