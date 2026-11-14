package v1alpha1

import public "github.com/robertherbaugh/kudurru/api/v1alpha1"

// Compatibility aliases keep Go callers on the single public IAM object model.
// These kinds are registered only by public.AddToScheme and are no longer Secrets.

// ServiceAccountTokenSpec aliases the public IAM type.
// +kubebuilder:object:generate=false
type ServiceAccountTokenSpec = public.ServiceAccountTokenSpec

// ServiceAccountSpec aliases the public IAM type.
// +kubebuilder:object:generate=false
type ServiceAccountSpec = public.ServiceAccountSpec

// ServiceAccountStatus aliases the public IAM type.
// +kubebuilder:object:generate=false
type ServiceAccountStatus = public.ServiceAccountStatus

// ServiceAccount aliases the public IAM type.
// +kubebuilder:object:generate=false
type ServiceAccount = public.ServiceAccount

// ServiceAccountList aliases the public IAM type.
// +kubebuilder:object:generate=false
type ServiceAccountList = public.ServiceAccountList

// BindingScope aliases the public IAM type.
// +kubebuilder:object:generate=false
type BindingScope = public.BindingScope

// BindingSubject aliases the public IAM type.
// +kubebuilder:object:generate=false
type BindingSubject = public.BindingSubject

// RoleBindingSpec aliases the public IAM type.
// +kubebuilder:object:generate=false
type RoleBindingSpec = public.RoleBindingSpec

// RoleBinding aliases the public IAM type.
// +kubebuilder:object:generate=false
type RoleBinding = public.RoleBinding

// RoleBindingList aliases the public IAM type.
// +kubebuilder:object:generate=false
type RoleBindingList = public.RoleBindingList

// SecretKeyRef aliases the public IAM type.
// +kubebuilder:object:generate=false
type SecretKeyRef = public.SecretKeyRef

// ClaimValidationRule aliases the public IAM type.
// +kubebuilder:object:generate=false
type ClaimValidationRule = public.ClaimValidationRule

// PrefixedClaim aliases the public IAM type.
// +kubebuilder:object:generate=false
type PrefixedClaim = public.PrefixedClaim

// ClaimRef aliases the public IAM type.
// +kubebuilder:object:generate=false
type ClaimRef = public.ClaimRef

// ClaimMappings aliases the public IAM type.
// +kubebuilder:object:generate=false
type ClaimMappings = public.ClaimMappings

// Provisioning aliases the public IAM type.
// +kubebuilder:object:generate=false
type Provisioning = public.Provisioning

// IdentityProviderSpec aliases the public IAM type.
// +kubebuilder:object:generate=false
type IdentityProviderSpec = public.IdentityProviderSpec

// IdentityProviderStatus aliases the public IAM type.
// +kubebuilder:object:generate=false
type IdentityProviderStatus = public.IdentityProviderStatus

// IdentityProvider aliases the public IAM type.
// +kubebuilder:object:generate=false
type IdentityProvider = public.IdentityProvider

// IdentityProviderList aliases the public IAM type.
// +kubebuilder:object:generate=false
type IdentityProviderList = public.IdentityProviderList
