package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Public IAM kind names.
const (
	KindServiceAccount   = "ServiceAccount"
	KindRoleBinding      = "RoleBinding"
	KindIdentityProvider = "IdentityProvider"
)

// ServiceAccountTokenSpec asks kudurru to keep a token for the account in a
// Secret in the operator credential namespace.
type ServiceAccountTokenSpec struct {
	// +optional
	// +kubebuilder:validation:MaxLength=253
	SecretName string `json:"secretName,omitempty"`
	// +optional
	ExpiresAfter *metav1.Duration `json:"expiresAfter,omitempty"`
}

// ServiceAccountSpec is a non-human principal owning API tokens (§8.6).
type ServiceAccountSpec struct {
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	Description string `json:"description,omitempty"`
	// +optional
	Token *ServiceAccountTokenSpec `json:"token,omitempty"`
	// Disabled refuses every token of the account.
	// +optional
	Disabled bool `json:"disabled,omitempty"`
}

// ServiceAccountStatus records use.
type ServiceAccountStatus struct {
	// +optional
	TokenID string `json:"tokenID,omitempty"`
	// +optional
	LastUsed *metav1.Time `json:"lastUsed,omitempty"`
}

// ServiceAccount is a public non-human principal. Credentials stay private.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ksa
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
// +kubebuilder:validation:XValidation:rule="!has(self.kind) || self.kind == 'cluster' ? !has(self.name) || self.name.size() == 0 : has(self.name) && self.name.size() > 0",message="cluster scope has no name; other scopes require a name"
type BindingScope struct {
	// Kind is cluster, zone, zonegroup (v0.2) or clientgroup (reserved).
	// +kubebuilder:validation:Enum=cluster;zone;zonegroup;clientgroup
	// +optional
	Kind string `json:"kind,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name,omitempty"`
}

// BindingSubject is one party of a binding.
// +kubebuilder:validation:XValidation:rule="self.kind == 'ServiceAccount' ? has(self.namespace) && self.namespace.size() > 0 : !has(self.namespace) || self.namespace.size() == 0",message="only ServiceAccount subjects require a namespace"
type BindingSubject struct {
	// Kind is User, Group or ServiceAccount.
	// +kubebuilder:validation:Enum=User;Group;ServiceAccount
	Kind string `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace,omitempty"`
}

// RoleBindingSpec grants Role at Scope to Subjects.
type RoleBindingSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Role string `json:"role"`
	// +optional
	Scope BindingScope `json:"scope,omitempty"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	Subjects []BindingSubject `json:"subjects"`
}

// RoleBinding grants a named role to the selected subjects and scope.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=krb
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
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	Key  string `json:"key"`
}

// ClaimValidationRule requires scalar equality or a boolean CEL expression.
// +kubebuilder:validation:XValidation:rule="has(self.claim) != has(self.expression)",message="exactly one claim or expression is required"
// +kubebuilder:validation:XValidation:rule="!has(self.expression) || !has(self.requiredValue) || self.requiredValue.size() == 0",message="requiredValue belongs only to a claim rule"
type ClaimValidationRule struct {
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Claim string `json:"claim,omitempty"`
	// RequiredValue defaults to the empty string for scalar equality.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	RequiredValue string `json:"requiredValue,omitempty"`
	// Expression evaluates signed claims and must produce a boolean.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	Expression string `json:"expression,omitempty"`
}

// PrefixedClaim maps a claim or expression and prepends a prefix. An empty
// mapping selects the provider's default claim behavior.
// +kubebuilder:validation:XValidation:rule="!(has(self.claim) && has(self.expression))",message="claim and expression are mutually exclusive"
type PrefixedClaim struct {
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Claim string `json:"claim,omitempty"`
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	Expression string `json:"expression,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Prefix *string `json:"prefix,omitempty"`
}

// ClaimRef maps a claim or string expression; empty selects the default claim.
// +kubebuilder:validation:XValidation:rule="!(has(self.claim) && has(self.expression))",message="claim and expression are mutually exclusive"
type ClaimRef struct {
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Claim string `json:"claim,omitempty"`
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	Expression string `json:"expression,omitempty"`
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
	// +kubebuilder:validation:Enum=mapped-only;always;never
	AutoProvision string `json:"autoProvision,omitempty"`
	// +optional
	SyncGroups bool `json:"syncGroups,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=63
	DefaultRole string `json:"defaultRole,omitempty"`
}

// IdentityProviderSpec is one OpenID Provider (§8.2-8.4); the object name
// is the provider id used in URLs.
type IdentityProviderSpec struct {
	// +optional
	// +kubebuilder:validation:MaxLength=256
	DisplayName string `json:"displayName,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	Issuer string `json:"issuer"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	ClientID string `json:"clientID"`
	// ClientSecretRef selects an operator-granted credential capability.
	ClientSecretRef *CredentialReference `json:"clientSecretRef"`
	// CLIClientID is the public client of `kudurru login` (v0.2); bearer
	// ID tokens may carry it as their audience.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	CLIClientID string `json:"cliClientID,omitempty"`
	// RedirectURL is the absolute callback URL registered at the IdP.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	RedirectURL string `json:"redirectURL,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=256
	Scopes []string `json:"scopes,omitempty"`
	// Audiences are extra aud values accepted for bearer ID tokens.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=256
	Audiences []string `json:"audiences,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=64
	ClaimValidationRules []ClaimValidationRule `json:"claimValidationRules,omitempty"`
	// +optional
	ClaimMappings ClaimMappings `json:"claimMappings,omitempty"`
	// +optional
	Provisioning Provisioning `json:"provisioning,omitempty"`
	// AllowedDomains, when set, restricts logins to verified emails in
	// these domains (reserved).
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=256
	AllowedDomains []string `json:"allowedDomains,omitempty"`
}

// IdentityProviderStatus reports discovery.
type IdentityProviderStatus struct {
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=16
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// IdentityProvider configures an OpenID Provider without storing credentials.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=idp
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
