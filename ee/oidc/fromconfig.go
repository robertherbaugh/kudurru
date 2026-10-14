package oidc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
)

// SecretResolver reads a key of a Secret in the release namespace (the
// clientSecretRef of an IdentityProvider in Kubernetes).
type SecretResolver func(ctx context.Context, name, key string) (string, error)

// FileResolver is an explicitly delegated host capability. Stored objects do
// not acquire filesystem access merely by naming a path.
type FileResolver func(ctx context.Context, path string) (string, error)

// configEntry is one auth.identityProviders[] entry: the IdentityProvider
// spec plus its id.
type configEntry struct {
	ID string `json:"id"`
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
	if err := idp.Validate(); err != nil {
		return nil, err
	}
	return idp, nil
}

// FromIdentityProvider turns an IdentityProvider (object or config entry)
// into a ProviderConfig, resolving the client secret from
// clientSecretFile or, through secrets, clientSecretRef. Unset username
// and group prefixes default to "<id>:" (D71), so an IdP account can never
// match a User or Group binding meant for a local account; an explicit ""
// disables the prefix. The result still needs ProviderConfig.Validate
// (NewProvider calls it).
func FromIdentityProvider(ctx context.Context, idp *internalv1.IdentityProvider, secrets SecretResolver, files ...FileResolver) (ProviderConfig, error) {
	s := idp.Spec
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
	switch {
	case s.ClientSecretFile != "":
		if len(files) != 1 || files[0] == nil {
			return cfg, fmt.Errorf("oidc provider %q: file credential requires a host grant", idp.Name)
		}
		value, err := files[0](ctx, s.ClientSecretFile)
		if err != nil {
			return cfg, fmt.Errorf("oidc provider %q: file credential unavailable", idp.Name)
		}
		cfg.ClientSecret = strings.TrimSpace(value)
	case s.ClientSecretRef != nil:
		if secrets == nil {
			return cfg, fmt.Errorf("oidc provider %q: clientSecretRef needs the Kubernetes store driver; use clientSecretFile", idp.Name)
		}
		v, err := secrets(ctx, s.ClientSecretRef.Name, s.ClientSecretRef.Key)
		if err != nil {
			return cfg, fmt.Errorf("oidc provider %q: Secret credential unavailable", idp.Name)
		}
		cfg.ClientSecret = strings.TrimSpace(v)
	default:
		return cfg, errors.New("oidc provider " + idp.Name + ": clientSecretFile or clientSecretRef is required (kudurru is a confidential client)")
	}
	return cfg, nil
}

func prefixOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}
