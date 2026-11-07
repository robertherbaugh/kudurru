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

func TestProviderExpressionAdmission(t *testing.T) {
	v := testValidator(t)
	for _, tc := range []struct {
		name  string
		field string
		value any
		valid bool
	}{
		{"scalar", "claimValidationRules", []any{map[string]any{"claim": "hd", "requiredValue": "example.org"}}, true},
		{"empty scalar", "claimValidationRules", []any{map[string]any{"claim": "optional"}}, true},
		{"boolean expression", "claimValidationRules", []any{map[string]any{"expression": "claims.email_verified == true"}}, true},
		{"empty rule", "claimValidationRules", []any{map[string]any{}}, false},
		{"ambiguous rule", "claimValidationRules", []any{map[string]any{"claim": "hd", "expression": "true"}}, false},
		{"expression with scalar", "claimValidationRules", []any{map[string]any{"expression": "true", "requiredValue": "true"}}, false},
		{"default mapping", "claimMappings", map[string]any{"username": map[string]any{"prefix": "corp:"}}, true},
		{"expression mapping", "claimMappings", map[string]any{"groups": map[string]any{"expression": "claims.groups"}}, true},
		{"ambiguous mapping", "claimMappings", map[string]any{"email": map[string]any{"claim": "email", "expression": "claims.email"}}, false},
		{"expression cap", "claimMappings", map[string]any{"displayName": map[string]any{"expression": strings.Repeat("x", 4097)}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := map[string]any{"issuer": "https://sso.example", "clientID": "test", "clientSecretRef": map[string]any{"env": "OIDC_SECRET"}, tc.field: tc.value}
			u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": v1alpha1.GroupVersion.String(), "kind": "IdentityProvider", "metadata": map[string]any{"name": "test"}, "spec": spec}}
			if err := v.ValidateUnstructured(t.Context(), u, nil); (err == nil) != tc.valid {
				t.Fatalf("valid=%v, got %v", tc.valid, err)
			}
		})
	}
}
