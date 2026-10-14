package validator

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestPublicIAMAdmission(t *testing.T) {
	v := testValidator(t)
	for _, tc := range []struct {
		name, kind string
		spec       map[string]any
	}{
		{"private authority", "ServiceAccount", map[string]any{"builtinAdmin": true}},
		{"token hash", "ServiceAccount", map[string]any{"secretHash": strings.Repeat("a", 64)}},
		{"binding empty", "RoleBinding", map[string]any{"role": "operator", "subjects": []any{}}},
		{"binding wrong kind", "RoleBinding", map[string]any{"role": "operator", "subjects": []any{map[string]any{"kind": "Robot", "name": "test"}}}},
		{"binding account namespace", "RoleBinding", map[string]any{"role": "operator", "subjects": []any{map[string]any{"kind": "ServiceAccount", "name": "test"}}}},
		{"binding namespace on user", "RoleBinding", map[string]any{"role": "operator", "subjects": []any{map[string]any{"kind": "User", "name": "test", "namespace": "dns"}}}},
		{"cluster scope name", "RoleBinding", map[string]any{"role": "operator", "scope": map[string]any{"kind": "cluster", "name": "home"}, "subjects": []any{map[string]any{"kind": "User", "name": "test"}}}},
		{"zone scope name", "RoleBinding", map[string]any{"role": "operator", "scope": map[string]any{"kind": "zone"}, "subjects": []any{map[string]any{"kind": "User", "name": "test"}}}},
		{"provider path", "IdentityProvider", map[string]any{"issuer": "https://sso.example", "clientID": "test", "clientSecretFile": "/etc/private"}},
		{"provider missing ref", "IdentityProvider", map[string]any{"issuer": "https://sso.example", "clientID": "test"}},
		{"provider ambiguous ref", "IdentityProvider", map[string]any{"issuer": "https://sso.example", "clientID": "test", "clientSecretRef": map[string]any{"file": "/run/secrets/oidc", "env": "TOKEN"}}},
		{"provider namespace ref", "IdentityProvider", map[string]any{"issuer": "https://sso.example", "clientID": "test", "clientSecretRef": map[string]any{"secretRef": map[string]any{"namespace": "other", "name": "oidc", "key": "token"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": v1alpha1.GroupVersion.String(), "kind": tc.kind, "metadata": map[string]any{"name": "test"}, "spec": tc.spec}}
			if tc.kind != "IdentityProvider" {
				u.SetNamespace("dns")
			}
			before, _ := json.Marshal(u.Object)
			if err := v.ValidateUnstructured(t.Context(), u, nil); err == nil {
				t.Fatal("invalid public IAM accepted")
			}
			after, _ := json.Marshal(u.Object)
			if string(before) != string(after) {
				t.Fatal("rejection mutated input")
			}
		})
	}
}
