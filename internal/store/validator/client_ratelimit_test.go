package validator

import (
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestClientGroupRateLimitExemptSchemaRoundTrip(t *testing.T) {
	v := testValidator(t)
	g := &public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "busy", Namespace: "default"}, Spec: public.ClientGroupSpec{RateLimitExempt: true, Clients: []public.ClientMatcher{{IP: "192.0.2.1"}}}}
	if err := v.Validate(t.Context(), g, nil); err != nil || !g.Spec.RateLimitExempt {
		t.Fatal("embedded CRD pruned or refused group exemption", err)
	}
	if !g.DeepCopy().Spec.RateLimitExempt {
		t.Fatal("deepcopy lost scalar exemption")
	}
	g.Spec.RateLimitExempt = false
	if err := v.Validate(t.Context(), g, nil); err != nil || g.Spec.RateLimitExempt {
		t.Fatal("absent exemption failed default false", err)
	}
	for _, bad := range []any{"true", float64(1), []any{true}} {
		u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": public.GroupVersion.String(), "kind": "ClientGroup", "metadata": map[string]any{"name": "bad", "namespace": "default"}, "spec": map[string]any{"rateLimitExempt": bad}}}
		if err := v.ValidateUnstructured(t.Context(), u, nil); err == nil {
			t.Fatal("CRD accepted nonboolean exemption")
		}
	}
}
