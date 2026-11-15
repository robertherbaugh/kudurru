package kube

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// TestClientGroupRateLimitExemptAdmission exercises the generated CRD in a
// real apiserver: unknown-field pruning must not silently drop the exemption.
func TestClientGroupRateLimitExemptAdmission(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := newTestStore(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	group := &v1alpha1.ClientGroup{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "ClientGroup"},
		ObjectMeta: metav1.ObjectMeta{Name: "rate-exempt", Namespace: "apps"},
		Spec:       v1alpha1.ClientGroupSpec{RateLimitExempt: true},
	}
	created, err := s.Apply(ctx, group, store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(group)
	read := func(want bool) {
		t.Helper()
		var got v1alpha1.ClientGroup
		if err := shared.admin.Get(ctx, key, &got); err != nil {
			t.Fatal(err)
		}
		if got.Spec.RateLimitExempt != want {
			t.Fatalf("persisted exemption = %v, want %v", got.Spec.RateLimitExempt, want)
		}
	}
	read(true)
	group.Spec.RateLimitExempt = false
	if _, err := s.Apply(ctx, group, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: created.GetResourceVersion()}); err != nil {
		t.Fatal(err)
	}
	read(false)

	// Bypass typed decoding and the embedded validator to challenge actual
	// Kubernetes admission. Rejected input must leave the stored value intact.
	invalid := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(), "kind": "ClientGroup",
		"metadata": map[string]any{"name": key.Name, "namespace": key.Namespace},
	}}
	if err := shared.admin.Get(ctx, key, invalid); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(invalid.Object, "true", "spec", "rateLimitExempt"); err != nil {
		t.Fatal(err)
	}
	err = shared.admin.Update(ctx, invalid)
	if !apierrors.IsInvalid(err) && !apierrors.IsBadRequest(err) {
		t.Fatalf("nonboolean exemption must fail schema admission, got %v", err)
	}
	read(false)
}
