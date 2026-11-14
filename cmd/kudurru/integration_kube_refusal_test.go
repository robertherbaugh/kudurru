package main

import (
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A terminating UniFi instance that a field this root does not run refuses
// is still cleaned up when it is no publisher: its cleanup is the local
// purge and the finalizer removal (D384). A publisher stays refused, and a
// live instance is refused whatever the reason.
func TestKubeRefusalAdmitsCleanupOfNonPublishers(t *testing.T) {
	now := metav1.Now()
	unifi := func(terminating bool, edit func(*public.UniFiIntegration)) *public.Integration {
		d := &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "kudurru-system"}, Spec: public.IntegrationSpec{Type: "unifi", DeletionPolicy: public.IntegrationDeletionDelete, UniFi: &public.UniFiIntegration{Endpoint: "https://controller.invalid"}}}
		if terminating {
			d.DeletionTimestamp = &now
		}
		edit(d.Spec.UniFi)
		return d
	}
	for _, tc := range []struct {
		name    string
		edit    func(*public.UniFiIntegration)
		reason  string
		cleanup bool
	}{
		{"inventory", func(u *public.UniFiIntegration) { u.Inventory = &public.UniFiInventory{} }, reasonRecordsUnsupported, true},
		{"dhcpDNSServers", func(u *public.UniFiIntegration) {
			u.Push.DHCPDNSServers, u.Push.DNSServers, u.Push.Networks = true, []string{"192.0.2.53"}, []string{"LAN"}
		}, reasonPushUnsupported, true},
		{"clientActions", func(u *public.UniFiIntegration) { u.Push.ClientActions = true }, reasonPushUnsupported, true},
		// The record mirror makes the instance a publisher: its cleanup needs
		// the provider this root does not run for it.
		{"mirrorRecords", func(u *public.UniFiIntegration) { u.Push.MirrorRecords.Enabled = true }, reasonPushUnsupported, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live, terminating := unifi(false, tc.edit), unifi(true, tc.edit)
			if got := kubeIntegrationRefusal("kudurru-system", terminating); got != tc.reason {
				t.Fatalf("refusal %q, want %q", got, tc.reason)
			}
			if kubeRefusalAdmitsCleanup(tc.reason, live) {
				t.Fatal("a live instance's refusal admitted a cleanup")
			}
			if got := kubeRefusalAdmitsCleanup(tc.reason, terminating); got != tc.cleanup {
				t.Fatalf("cleanup admitted %t, want %t", got, tc.cleanup)
			}
			// An admitted cleanup keeps its credential references; a refused
			// instance resolves none.
			if got := kubeIntegrationCredentialless("kudurru-system", terminating); got == tc.cleanup {
				t.Fatalf("credentialless %t with cleanup %t", got, tc.cleanup)
			}
			c := &kubeIntegrationCut{}
			r := &kubeIntegrationRuntime{opts: kubeIntegrationOptions{Namespace: "kudurru-system"}}
			if got := r.refusedFor(c, ih.GuardCleanup, terminating); got == tc.cleanup {
				t.Fatalf("cleanup refused %t with admission %t", got, tc.cleanup)
			}
			if !r.refusedFor(c, ih.GuardNormal, live) || r.refusedFor(c, ih.GuardRelease, terminating) {
				t.Fatal("a normal run was admitted or a release refused")
			}
			if msg := kubeRefusalMessage(tc.reason, "kudurru-system"); msg == kubeRefusalMessage(reasonUnsupportedOnKubernetes, "") {
				t.Fatalf("%s has no message of its own: %q", tc.reason, msg)
			}
		})
	}
}
