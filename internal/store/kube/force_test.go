package kube

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// managers returns the main-resource field managers of a zone.
func managers(t *testing.T, name string) map[string]metav1.ManagedFieldsOperationType {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("DNSZone"))
	if err := shared.admin.Get(context.Background(), client.ObjectKey{Name: name}, u); err != nil {
		t.Fatal(err)
	}
	out := map[string]metav1.ManagedFieldsOperationType{}
	for _, e := range u.GetManagedFields() {
		if e.Subresource == "" {
			out[e.Manager] = e.Operation
		}
	}
	return out
}

// TestForceIsWholeObjectTakeover: after a forced apply the new manager owns
// the whole object, as in the reference driver. Fields that only the
// previous owners set (a file manager's defaultTTL and nameservers, a
// kubectl manager's label) and that the new manager omits are removed,
// instead of lingering under their old SSA owners.
func TestForceIsWholeObjectTakeover(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := newTestStore(t, Options{})
	ctx := context.Background()

	byFile := zone("home", "home.example.")
	byFile.Spec.DefaultTTL = 600
	byFile.Spec.Nameservers = []string{"ns1.home.example."}
	if _, err := s.Apply(ctx, byFile, store.ApplyOptions{Manager: store.ManagerFile("zones.yaml")}); err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(), "kind": "DNSZone",
		"metadata": map[string]any{"name": "home", "labels": map[string]any{"owner": "ops"}},
	}}
	if err := shared.admin.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner("kubectl")); err != nil {
		t.Fatal(err)
	}

	out, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	z := out.(*v1alpha1.DNSZone)
	if z.Spec.DefaultTTL != 300 || len(z.Spec.Nameservers) != 0 || z.Labels["owner"] != "" {
		t.Fatalf("fields of the previous owners survived Force: defaultTTL=%d (CRD default 300) nameservers=%v labels=%v",
			z.Spec.DefaultTTL, z.Spec.Nameservers, z.Labels)
	}
	if store.ManagedBy(z) != store.ManagerAPI || z.Spec.ZoneName != "home.example." {
		t.Fatalf("managedBy=%q zoneName=%q", store.ManagedBy(z), z.Spec.ZoneName)
	}
	if m := managers(t, "home"); len(m) != 1 || m[store.ManagerAPI] != metav1.ManagedFieldsOperationApply {
		t.Fatalf("main-resource managers after Force: %v, want only api/Apply", m)
	}
	// The new owner can now shrink the object further without Force.
	if _, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	// Force by the current sole owner is a plain forced apply.
	if _, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI, Force: true}); err != nil {
		t.Fatal(err)
	}
}
