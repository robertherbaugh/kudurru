/*
Copyright 2026 The kudurru Authors.

SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0
See the LICENSE file in the repository root.
*/

// Package v1alpha1 holds the internal kudurru.io/v1alpha1 kinds of the
// identity layer (docs/architecture.md §4.1, §8, decision D26): LocalUser,
// APIToken, ServiceAccount, RoleBinding and IdentityProvider.
//
// They share the kudurru.io/v1alpha1 group version with the public kinds so
// that object files and `kudurru apply` read them with the same apiVersion,
// but they are never CRDs: the sqlite driver persists them as JSON rows in
// state.db and the kube driver as Secrets labelled kudurru.io/kind=<Kind> in
// the release namespace. This package therefore carries no +groupName
// marker and is excluded from the CRD generator paths (Makefile
// `manifests`); controller-gen only generates its deepcopy functions.
//
// Secret material never appears in plain text: passwords are Argon2id PHC
// strings, API tokens are SHA-256 hashes, and the reserved TOTP seed is
// sealed with the cluster encryption key before it is stored (v0.2).
//
// +kubebuilder:object:generate=true
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// Kind names of the internal kinds, as store.Kind strings.
const (
	KindLocalUser        = "LocalUser"
	KindAPIToken         = "APIToken"
	KindServiceAccount   = "ServiceAccount"
	KindRoleBinding      = "RoleBinding"
	KindIdentityProvider = "IdentityProvider"
)

// Kinds lists every internal kind, sorted. Store drivers receive it as
// Options.InternalKinds: sqlite skips the CRD validator for them (they call
// Validate instead) and omits them from safe snapshots; kube maps them to
// Secrets.
var Kinds = []string{KindAPIToken, KindIdentityProvider, KindLocalUser, KindRoleBinding, KindServiceAccount}

// Namespaced reports whether an internal kind is namespaced.
// ServiceAccount and RoleBinding are; the others are cluster-scoped.
func Namespaced(kind string) bool {
	return kind == KindServiceAccount || kind == KindRoleBinding
}

// IsInternal reports whether kind is one of Kinds.
func IsInternal(kind string) bool {
	for _, k := range Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

var (
	// SchemeBuilder registers the internal kinds under kudurru.io/v1alpha1.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the internal kinds to a scheme. A store scheme for
	// the identity layer is public.AddToScheme plus this function
	// (NewScheme).
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(public.GroupVersion,
		&LocalUser{}, &LocalUserList{},
		&APIToken{}, &APITokenList{},
		&ServiceAccount{}, &ServiceAccountList{},
		&RoleBinding{}, &RoleBindingList{},
		&IdentityProvider{}, &IdentityProviderList{},
	)
	metav1.AddToGroupVersion(s, public.GroupVersion)
	return nil
}

// NewScheme returns a scheme with the public and the internal kudurru.io
// kinds: the scheme a store driver needs to serve the identity layer.
func NewScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := public.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

// KindLabel marks the Secrets that carry internal kinds in Kubernetes:
// kudurru.io/kind=<Kind>.
const KindLabel = "kudurru.io/kind"

// Validator is implemented by every internal kind. Store drivers call it
// instead of the CRD schema validator, which knows only the public kinds.
//
// +kubebuilder:object:generate=false
type Validator interface {
	Validate() error
}
