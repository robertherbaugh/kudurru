package oidc

import (
	"context"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestLegacyProviderNeverDropsExpressionPolicy(t *testing.T) {
	for _, spec := range []public.IdentityProviderSpec{
		{ClaimValidationRules: []public.ClaimValidationRule{{Expression: "false"}}},
		{ClaimMappings: public.ClaimMappings{Username: public.PrefixedClaim{Expression: "claims.sub"}}},
		{ClaimMappings: public.ClaimMappings{Groups: public.PrefixedClaim{Expression: "claims.groups"}}},
		{ClaimMappings: public.ClaimMappings{DisplayName: public.ClaimRef{Expression: "claims.name"}}},
		{ClaimMappings: public.ClaimMappings{Email: public.ClaimRef{Expression: "claims.email"}}},
	} {
		_, err := FromIdentityProvider(t.Context(), &public.IdentityProvider{Spec: spec}, func(context.Context, public.CredentialReference) (string, error) {
			t.Fatal("unsupported expression read a credential")
			return "", nil
		})
		if err == nil {
			t.Fatal("legacy provider ignored expression policy")
		}
	}
}
