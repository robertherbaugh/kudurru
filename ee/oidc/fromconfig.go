package oidc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// SecretResolver reads a key of a Secret in the release namespace (the
// clientSecretRef of an IdentityProvider in Kubernetes).
type SecretResolver func(ctx context.Context, name, key string) (string, error)

// CredentialResolver is an explicitly delegated host capability. Stored objects do
// not acquire filesystem access merely by naming a path.
type CredentialResolver func(ctx context.Context, ref publicv1.CredentialReference) (string, error)

// configEntry is one auth.identityProviders[] entry: the IdentityProvider
// spec plus its id.
type configEntry struct {
	ID               string          `json:"id"`
	ClientSecretFile string          `json:"clientSecretFile,omitempty"`
	ClientSecretRef  json.RawMessage `json:"clientSecretRef,omitempty"`
	internalv1.IdentityProviderSpec
}

// ParseConfigEntry decodes one auth.identityProviders[] map strictly
// (unknown keys are errors, so a typo cannot silently drop a claim rule)
// into an IdentityProvider object named by its id.
func ParseConfigEntry(m map[string]any) (*internalv1.IdentityProvider, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var e configEntry
	if err := dec.Decode(&e); err != nil {
		return nil, err
	}
	idp := &internalv1.IdentityProvider{Spec: e.IdentityProviderSpec}
	idp.Name = e.ID
	if e.ClientSecretFile != "" && len(e.ClientSecretRef) != 0 {
		return nil, errors.New("oidc: configure only one client credential reference")
	}
	if e.ClientSecretFile != "" {
		idp.Spec.ClientSecretRef = &publicv1.CredentialReference{File: e.ClientSecretFile}
	} else if len(e.ClientSecretRef) != 0 {
		var legacy publicv1.SecretKeyRef
		if json.Unmarshal(e.ClientSecretRef, &legacy) == nil && (legacy.Name != "" || legacy.Key != "") {
			dec := json.NewDecoder(bytes.NewReader(e.ClientSecretRef))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&legacy); err != nil {
				return nil, err
			}
			idp.Spec.ClientSecretRef = &publicv1.CredentialReference{SecretRef: &publicv1.CredentialSecretRef{Name: legacy.Name, Key: legacy.Key}}
		} else {
			dec := json.NewDecoder(bytes.NewReader(e.ClientSecretRef))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&idp.Spec.ClientSecretRef); err != nil {
				return nil, err
			}
		}
	}
	if err := idp.Validate(); err != nil {
		return nil, err
	}
	return idp, nil
}

// FromIdentityProvider turns an IdentityProvider (object or config entry)
// into a ProviderConfig, resolving the client secret from
// its delegated clientSecretRef capability. Unset username
// and group prefixes default to "<id>:" (D71), so an IdP account can never
// match a User or Group binding meant for a local account; an explicit ""
// disables the prefix. The result still needs ProviderConfig.Validate
// (NewProvider calls it).
func FromIdentityProvider(ctx context.Context, idp *internalv1.IdentityProvider, resolver CredentialResolver) (ProviderConfig, error) {
	s := idp.Spec
	// This legacy live provider has no captured CEL compiler. Never silently
	// discard a stored authentication restriction while its replacement lands.
	m := s.ClaimMappings
	if m.Username.Expression != "" || m.Groups.Expression != "" || m.DisplayName.Expression != "" || m.Email.Expression != "" {
		return ProviderConfig{}, errors.New("oidc: expression policy requires captured provider authentication")
	}
	for _, rule := range s.ClaimValidationRules {
		if rule.Expression != "" {
			return ProviderConfig{}, errors.New("oidc: expression policy requires captured provider authentication")
		}
	}
	cfg := ProviderConfig{
		ID: idp.Name, DisplayName: s.DisplayName, Issuer: s.Issuer, ClientID: s.ClientID,
		CLIClientID: s.CLIClientID, Audiences: s.Audiences, RedirectURL: s.RedirectURL, Scopes: s.Scopes,
		ClaimMappings: ClaimMappings{
			Username:    PrefixedClaim{Claim: s.ClaimMappings.Username.Claim, Prefix: prefixOr(s.ClaimMappings.Username.Prefix, idp.Name+":")},
			Groups:      PrefixedClaim{Claim: s.ClaimMappings.Groups.Claim, Prefix: prefixOr(s.ClaimMappings.Groups.Prefix, idp.Name+":")},
			DisplayName: ClaimRef{Claim: s.ClaimMappings.DisplayName.Claim},
			Email:       ClaimRef{Claim: s.ClaimMappings.Email.Claim},
		},
		Provisioning: Provisioning{
			AutoProvision: AutoProvision(s.Provisioning.AutoProvision),
			SyncGroups:    s.Provisioning.SyncGroups,
			DefaultRole:   s.Provisioning.DefaultRole,
		},
	}
	for _, r := range s.ClaimValidationRules {
		cfg.ClaimValidationRules = append(cfg.ClaimValidationRules, ClaimValidationRule{Claim: r.Claim, RequiredValue: r.RequiredValue})
	}
	if s.ClientSecretRef == nil || resolver == nil {
		return cfg, fmt.Errorf("oidc provider %q: credential reference requires a delegated resolver", idp.Name)
	}
	v, err := resolver(ctx, *s.ClientSecretRef)
	if err != nil {
		return cfg, fmt.Errorf("oidc provider %q: credential unavailable", idp.Name)
	}
	cfg.ClientSecret = strings.TrimSpace(v)
	return cfg, nil
}

func prefixOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}
