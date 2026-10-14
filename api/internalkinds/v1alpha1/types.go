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
	// UID binds local users and service accounts to one object incarnation.
	// Legacy tokens without a verified binding cannot authenticate.
	// +optional
	UID string `json:"uid,omitempty"`
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
