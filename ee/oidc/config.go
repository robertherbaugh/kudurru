// Package oidc is kudurru's OpenID Connect relying party for the web UI: the
// backend-for-frontend authorization code + PKCE login of
// dev-docs/design-record.md §8.2, the claim pipeline of §8.4
// (claimValidationRules → claimMappings → provisioning → bindings) and the
// identity key of §8.1, built on github.com/coreos/go-oidc/v3 and
// golang.org/x/oauth2 (§2).
//
// Tokens never reach the browser: the callback exchanges the code as a
// confidential client, verifies the ID token, resolves role bindings and
// issues a sealed session cookie (package session). The refresh token is
// discarded (D23: no refresh token in the cookie).
package oidc

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/rbac"
)

// AutoProvision is the provisioning policy for a first login (§8.4).
type AutoProvision string

// Provisioning policies.
//
//   - mapped-only (default): the login must match at least one binding.
//   - always: any identity the IdP vouches for may log in; one with no
//     binding receives Provisioning.DefaultRole when one is configured.
//   - never: only identities already known to kudurru may log in
//     (Options.IdentityExists decides); DefaultRole applies as for always.
//
// In every mode a login that ends with zero grants is refused with a clear
// message; kudurru never silently grants admin.
const (
	AutoProvisionMappedOnly AutoProvision = "mapped-only"
	AutoProvisionAlways     AutoProvision = "always"
	AutoProvisionNever      AutoProvision = "never"
)

// ProviderConfig is one entry of auth.identityProviders[] (or an
// IdentityProvider object). JSON names match the config file of §6.2.
// Secrets arrive resolved: clientSecretFile / secretRef handling belongs to
// internal/config.
type ProviderConfig struct {
	// ID is the provider slug used in URLs, cookie names and the identity
	// key: lowercase letters, digits and dashes.
	ID string `json:"id"`
	// DisplayName is shown on the login page; defaults to ID.
	DisplayName string `json:"displayName,omitempty"`
	// Issuer is the exact issuer URL; discovery reads
	// <issuer>/.well-known/openid-configuration. https is required except
	// for loopback hosts (local IdPs in development and tests).
	Issuer string `json:"issuer"`
	// ClientID is the confidential web client registered at the IdP.
	ClientID string `json:"clientID"`
	// ClientSecret is the resolved client secret.
	ClientSecret string `json:"clientSecret,omitempty"`
	// RedirectURL is the absolute callback URL registered at the IdP, e.g.
	// https://dns.example/api/v1/auth/oidc/corp/callback.
	RedirectURL string `json:"redirectURL"`
	// CLIClientID is the public client of `kudurru login` (v0.2); bearer
	// ID tokens minted for it are accepted (§8.3).
	CLIClientID string `json:"cliClientID,omitempty"`
	// Audiences are extra aud values accepted for bearer ID tokens.
	Audiences []string `json:"audiences,omitempty"`
	// Scopes requested; default openid, profile, email, groups. openid is
	// always added.
	Scopes []string `json:"scopes,omitempty"`
	// ClaimValidationRules must all hold or the login is refused.
	ClaimValidationRules []ClaimValidationRule `json:"claimValidationRules,omitempty"`
	// ClaimMappings turns claims into a username and groups.
	ClaimMappings ClaimMappings `json:"claimMappings"`
	// Provisioning is the first-login policy.
	Provisioning Provisioning `json:"provisioning"`
}

// ClaimValidationRule is the simple form of the Kubernetes rule: the claim
// must be present and equal RequiredValue (booleans and numbers compare by
// their JSON text, so email_verified: "true" works). CEL expressions are
// v0.2.
type ClaimValidationRule struct {
	Claim         string `json:"claim"`
	RequiredValue string `json:"requiredValue"`
}

// PrefixedClaim names a claim and the prefix prepended to its values.
type PrefixedClaim struct {
	Claim  string `json:"claim,omitempty"`
	Prefix string `json:"prefix,omitempty"`
}

// ClaimRef names a claim.
type ClaimRef struct {
	Claim string `json:"claim,omitempty"`
}

// ClaimMappings follows §8.4.
type ClaimMappings struct {
	// Username: the configured claim is tried first, then the fallback
	// chain preferred_username → upn → email (only when email_verified is
	// true) → sub. Prefix is prepended to the result.
	Username PrefixedClaim `json:"username"`
	// Groups: the configured claim (default "groups") and the "roles"
	// claim are both read; each may be a string or an array of strings.
	// Prefix is prepended to every value, so bindings name e.g.
	// "corp:/dns-admins".
	Groups PrefixedClaim `json:"groups"`
	// DisplayName claim, default "name".
	DisplayName ClaimRef `json:"displayName"`
	// Email claim, default "email"; kept only when email_verified is true.
	Email ClaimRef `json:"email"`
}

// Provisioning follows §8.4.
type Provisioning struct {
	AutoProvision AutoProvision `json:"autoProvision,omitempty"`
	// SyncGroups is accepted for config compatibility. With sealed
	// sessions (v0.1) group membership is re-read at every login anyway;
	// persisting IdP-managed groups needs the user store.
	SyncGroups bool `json:"syncGroups,omitempty"`
	// DefaultRole is granted cluster-wide to a login with no binding when
	// AutoProvision is "always" or "never" (it contradicts mapped-only). It
	// may not be "admin".
	DefaultRole string `json:"defaultRole,omitempty"`
}

// DefaultScopes are requested when ProviderConfig.Scopes is empty.
var DefaultScopes = []string{"openid", "profile", "email", "groups"}

var idPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Validate checks the configuration and fills defaults. perms, when non-nil,
// is used to check that DefaultRole exists and is not a scoped role.
func (c *ProviderConfig) Validate(perms *rbac.Permissions) error {
	var errs []error
	if !idPattern.MatchString(c.ID) {
		errs = append(errs, fmt.Errorf("id %q must be a lowercase DNS label", c.ID))
	}
	if c.DisplayName == "" {
		c.DisplayName = c.ID
	}
	if err := checkURL("issuer", c.Issuer); err != nil {
		errs = append(errs, err)
	}
	if err := checkURL("redirectURL", c.RedirectURL); err != nil {
		errs = append(errs, err)
	}
	if c.ClientID == "" {
		errs = append(errs, errors.New("clientID is required"))
	}
	if c.ClientSecret == "" {
		errs = append(errs, errors.New("clientSecret is required (kudurru is a confidential client)"))
	}
	if len(c.Scopes) == 0 {
		c.Scopes = slices.Clone(DefaultScopes)
	}
	if !slices.Contains(c.Scopes, "openid") {
		c.Scopes = append([]string{"openid"}, c.Scopes...)
	}
	for i, r := range c.ClaimValidationRules {
		if r.Claim == "" {
			errs = append(errs, fmt.Errorf("claimValidationRules[%d]: claim is required", i))
		}
	}
	if c.ClaimMappings.Groups.Claim == "" {
		c.ClaimMappings.Groups.Claim = "groups"
	}
	if c.ClaimMappings.DisplayName.Claim == "" {
		c.ClaimMappings.DisplayName.Claim = "name"
	}
	if c.ClaimMappings.Email.Claim == "" {
		c.ClaimMappings.Email.Claim = "email"
	}
	switch c.Provisioning.AutoProvision {
	case "":
		c.Provisioning.AutoProvision = AutoProvisionMappedOnly
	case AutoProvisionMappedOnly, AutoProvisionAlways, AutoProvisionNever:
	default:
		errs = append(errs, fmt.Errorf("provisioning.autoProvision %q is not always|mapped-only|never", c.Provisioning.AutoProvision))
	}
	if r := c.Provisioning.DefaultRole; r != "" {
		switch {
		case r == "admin":
			errs = append(errs, errors.New("provisioning.defaultRole may not be admin; bind admins explicitly"))
		case perms != nil && perms.Roles[r] == nil:
			errs = append(errs, fmt.Errorf("provisioning.defaultRole %q is not a known role", r))
		case perms != nil && slices.Contains(perms.Scoped, r):
			errs = append(errs, fmt.Errorf("provisioning.defaultRole %q is scoped and cannot be granted cluster-wide", r))
		}
		if c.Provisioning.AutoProvision == AutoProvisionMappedOnly {
			errs = append(errs, errors.New("provisioning.defaultRole contradicts autoProvision: mapped-only"))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("oidc provider %q: %w", c.ID, errors.Join(errs...))
	}
	return nil
}

func checkURL(field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("%s %q must be an absolute http(s) URL", field, raw)
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return fmt.Errorf("%s %q must use https (http is allowed only for loopback hosts)", field, raw)
	}
	if u.Fragment != "" {
		return fmt.Errorf("%s %q must not have a fragment", field, raw)
	}
	return nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
