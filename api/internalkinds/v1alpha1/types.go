/*
Copyright 2026 Robert Herbaugh.

SPDX-License-Identifier: LicenseRef-Kudurru-Community
See the LICENSE file in the repository root.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LocalUserSpec is a local account (§8.7). The object name is the
// username.
type LocalUserSpec struct {
	// DisplayName is shown in the UI.
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	// PasswordHash is an Argon2id PHC string
	// ($argon2id$v=19$m=19456,t=2,p=1$salt$hash); empty means the account
	// cannot log in with a password.
	// +optional
	PasswordHash string `json:"passwordHash,omitempty"`
	// MustChangePassword restricts the account to changing its password
	// (set for the generated first-boot admin and after a reset).
	// +optional
	MustChangePassword bool `json:"mustChangePassword,omitempty"`
	// Disabled refuses every login and every token of the account.
	// +optional
	Disabled bool `json:"disabled,omitempty"`
	// Groups are local group memberships, matched by Group subjects of
	// role bindings. Local group names cannot collide with IdP groups,
	// which always carry an "<idp>:" style prefix.
	// +optional
	Groups []string `json:"groups,omitempty"`
	// BuiltinAdmin grants the admin role implicitly, without a binding
	// (the first-boot admin). The v0.2 IAM write API must refuse to set it.
	// +optional
	BuiltinAdmin bool `json:"builtinAdmin,omitempty"`
	// TOTPSecret is reserved for v0.2 TOTP: the seed sealed with the
	// cluster encryption key, never plain text.
	// +optional
	TOTPSecret string `json:"totpSecret,omitempty"`
}

// LocalUserStatus is maintained by kudurru.
type LocalUserStatus struct {
	// SessionsValidAfter revokes every session issued at or before it
	// (password change, "log out everywhere", disable; §8.2).
	// +optional
	SessionsValidAfter *metav1.Time `json:"sessionsValidAfter,omitempty"`
	// PasswordChangedAt is when PasswordHash was last set.
	// +optional
	PasswordChangedAt *metav1.Time `json:"passwordChangedAt,omitempty"`
	// LastLoginAt is the last successful password login.
	// +optional
	LastLoginAt *metav1.Time `json:"lastLoginAt,omitempty"`
}

// LocalUser is an internal kind: a local account with a password hash.
//
// +kubebuilder:object:root=true
type LocalUser struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec LocalUserSpec `json:"spec"`
	// +optional
	Status LocalUserStatus `json:"status,omitempty"`
}

// LocalUserList contains a list of LocalUser.
//
// +kubebuilder:object:root=true
type LocalUserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LocalUser `json:"items"`
}

// Token owner kinds.
const (
	OwnerUser           = "User"
	OwnerServiceAccount = "ServiceAccount"
)

// TokenOwner names the principal an API token acts as.
type TokenOwner struct {
	// Kind is User (a personal token) or ServiceAccount.
	Kind string `json:"kind"`
	// Name is the username (local users: the LocalUser name; OIDC users:
	// the prefixed username) or the service account name.
	Name string `json:"name"`
	// Namespace is the service account's namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// ID is the owner's principal ID ("local:alice", "oidc:…"), used to
	// list and revoke a user's own tokens.
	// +optional
	ID string `json:"id,omitempty"`
	// Provider is "local" for local users, the IdP id for OIDC users.
	// +optional
	Provider string `json:"provider,omitempty"`
}

// APITokenSpec is one API token (§8.6). The object name is the 12-char
// token id; the secret exists only in the wire form handed out once.
type APITokenSpec struct {
	// Owner is who the token acts as.
	Owner TokenOwner `json:"owner"`
	// Description is a human label.
	// +optional
	Description string `json:"description,omitempty"`
	// SecretHash is hex(sha256(full token)); compared in constant time.
	SecretHash string `json:"secretHash"`
	// ExpiresAt, when set, ends the token (auth.tokens.maxLifetime caps
	// it).
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
	// Grants are frozen at creation for owners whose bindings cannot be
	// re-resolved per request (OIDC users have no stored group list,
	// D48): the creating session's roles snapshot. Empty for local users
	// and service accounts, whose bindings are evaluated live.
	// +optional
	Grants []string `json:"grants,omitempty"`
	// Scopes optionally narrow the token: every request must also be
	// allowed by one of these grants ("role" or "role@kind:name").
	// +optional
	Scopes []string `json:"scopes,omitempty"`
}

// APITokenStatus records use.
type APITokenStatus struct {
	// +optional
	LastUsedAt *metav1.Time `json:"lastUsedAt,omitempty"`
	// +optional
	LastUsedIP string `json:"lastUsedIP,omitempty"`
}

// APIToken is an internal kind: the SHA-256 hash of a kdr_ token.
//
// +kubebuilder:object:root=true
type APIToken struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec APITokenSpec `json:"spec"`
	// +optional
	Status APITokenStatus `json:"status,omitempty"`
}

// APITokenList contains a list of APIToken.
//
// +kubebuilder:object:root=true
type APITokenList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []APIToken `json:"items"`
}

// ServiceAccountTokenSpec asks kudurru to keep a token for the account in a
// Secret (Kubernetes) or file (v0.2).
type ServiceAccountTokenSpec struct {
	// +optional
	SecretName string `json:"secretName,omitempty"`
	// +optional
	ExpiresAfter *metav1.Duration `json:"expiresAfter,omitempty"`
}

// ServiceAccountSpec is a non-human principal owning API tokens (§8.6).
type ServiceAccountSpec struct {
	// +optional
	Description string `json:"description,omitempty"`
	// +optional
	Token *ServiceAccountTokenSpec `json:"token,omitempty"`
	// Disabled refuses every token of the account.
	// +optional
	Disabled bool `json:"disabled,omitempty"`
	// BuiltinAdmin grants the admin role implicitly (the bootstrap
	// service account of KUDURRU_BOOTSTRAP_TOKEN).
	// +optional
	BuiltinAdmin bool `json:"builtinAdmin,omitempty"`
}

// ServiceAccountStatus records use.
type ServiceAccountStatus struct {
	// +optional
	TokenID string `json:"tokenID,omitempty"`
	// +optional
	LastUsed *metav1.Time `json:"lastUsed,omitempty"`
}

// ServiceAccount is a kudurru service account (YAML in v0.1, CRD in v0.2).
//
// +kubebuilder:object:root=true
type ServiceAccount struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ServiceAccountSpec `json:"spec"`
	// +optional
	Status ServiceAccountStatus `json:"status,omitempty"`
}

// ServiceAccountList contains a list of ServiceAccount.
//
// +kubebuilder:object:root=true
type ServiceAccountList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServiceAccount `json:"items"`
}

// BindingScope narrows a role binding (§8.5).
type BindingScope struct {
	// Kind is cluster, zone, zonegroup (v0.2) or clientgroup (v1.0).
	Kind string `json:"kind"`
	// +optional
	Name string `json:"name,omitempty"`
}

// BindingSubject is one party of a binding.
type BindingSubject struct {
	// Kind is User, Group or ServiceAccount.
	Kind string `json:"kind"`
	Name string `json:"name"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// RoleBindingSpec grants Role at Scope to Subjects.
type RoleBindingSpec struct {
	Role string `json:"role"`
	// +optional
	Scope BindingScope `json:"scope,omitempty"`
	// +kubebuilder:validation:MinItems=1
	Subjects []BindingSubject `json:"subjects"`
}

// RoleBinding is a kudurru role binding (YAML in v0.1, CRD in v0.2).
//
// +kubebuilder:object:root=true
type RoleBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec RoleBindingSpec `json:"spec"`
}

// RoleBindingList contains a list of RoleBinding.
//
// +kubebuilder:object:root=true
type RoleBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RoleBinding `json:"items"`
}

// SecretKeyRef names a key of a Secret in the release namespace.
type SecretKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// ClaimValidationRule requires a claim to equal a value (§8.4).
type ClaimValidationRule struct {
	Claim         string `json:"claim"`
	RequiredValue string `json:"requiredValue"`
}

// PrefixedClaim names a claim and the prefix prepended to its values.
type PrefixedClaim struct {
	// +optional
	Claim string `json:"claim,omitempty"`
	// +optional
	Prefix *string `json:"prefix,omitempty"`
}

// ClaimRef names a claim.
type ClaimRef struct {
	// +optional
	Claim string `json:"claim,omitempty"`
}

// ClaimMappings turns claims into a username and groups (§8.4).
type ClaimMappings struct {
	// +optional
	Username PrefixedClaim `json:"username,omitempty"`
	// +optional
	Groups PrefixedClaim `json:"groups,omitempty"`
	// +optional
	DisplayName ClaimRef `json:"displayName,omitempty"`
	// +optional
	Email ClaimRef `json:"email,omitempty"`
}

// Provisioning is the first-login policy (§8.4).
type Provisioning struct {
	// AutoProvision is mapped-only (default), always or never.
	// +optional
	AutoProvision string `json:"autoProvision,omitempty"`
	// +optional
	SyncGroups bool `json:"syncGroups,omitempty"`
	// +optional
	DefaultRole string `json:"defaultRole,omitempty"`
}

// IdentityProviderSpec is one OpenID Provider (§8.2-8.4); the object name
// is the provider id used in URLs.
type IdentityProviderSpec struct {
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	Issuer      string `json:"issuer"`
	ClientID    string `json:"clientID"`
	// ClientSecretRef names a Secret key in the release namespace
	// (Kubernetes).
	// +optional
	ClientSecretRef *SecretKeyRef `json:"clientSecretRef,omitempty"`
	// ClientSecretFile is a file holding the client secret (Docker).
	// +optional
	ClientSecretFile string `json:"clientSecretFile,omitempty"`
	// CLIClientID is the public client of `kudurru login` (v0.2); bearer
	// ID tokens may carry it as their audience.
	// +optional
	CLIClientID string `json:"cliClientID,omitempty"`
	// RedirectURL is the absolute callback URL registered at the IdP.
	// +optional
	RedirectURL string `json:"redirectURL,omitempty"`
	// +optional
	Scopes []string `json:"scopes,omitempty"`
	// Audiences are extra aud values accepted for bearer ID tokens.
	// +optional
	Audiences []string `json:"audiences,omitempty"`
	// +optional
	ClaimValidationRules []ClaimValidationRule `json:"claimValidationRules,omitempty"`
	// +optional
	ClaimMappings ClaimMappings `json:"claimMappings,omitempty"`
	// +optional
	Provisioning Provisioning `json:"provisioning,omitempty"`
	// AllowedDomains, when set, restricts logins to verified emails in
	// these domains (reserved).
	// +optional
	AllowedDomains []string `json:"allowedDomains,omitempty"`
}

// IdentityProviderStatus reports discovery.
type IdentityProviderStatus struct {
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// IdentityProvider configures an OpenID Provider (YAML in v0.1, CRD in
// v0.2).
//
// +kubebuilder:object:root=true
type IdentityProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec IdentityProviderSpec `json:"spec"`
	// +optional
	Status IdentityProviderStatus `json:"status,omitempty"`
}

// IdentityProviderList contains a list of IdentityProvider.
//
// +kubebuilder:object:root=true
type IdentityProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IdentityProvider `json:"items"`
}
