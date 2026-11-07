/*
Copyright 2026 Robert Herbaugh.

SPDX-License-Identifier: LicenseRef-Kudurru-Community
See the LICENSE file in the repository root.
*/

package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// PrincipalStateSpec contains private authority bound to one account UID.
// Public ServiceAccount fields and annotations can never grant this authority.
type PrincipalStateSpec struct {
	Owner TokenOwner `json:"owner"`
	// +optional
	BuiltinAdmin bool `json:"builtinAdmin,omitempty"`
}

// PrincipalState is private and is never accepted by public object routes.
//
// +kubebuilder:object:root=true
type PrincipalState struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PrincipalStateSpec `json:"spec"`
}

// PrincipalStateList contains private account authority records.
//
// +kubebuilder:object:root=true
type PrincipalStateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PrincipalState `json:"items"`
}

// PrincipalStateName includes the account UID, so name reuse cannot inherit
// private authority. The namespace and name remain explicit in Spec for checks.
func PrincipalStateName(owner TokenOwner) string {
	sum := sha256.Sum256([]byte(owner.Kind + "\x00" + owner.Namespace + "\x00" + owner.Name + "\x00" + owner.UID))
	hexDigest := hex.EncodeToString(sum[:])
	return "ps-" + hexDigest[:32] + "." + hexDigest[32:]
}

// Validate implements Validator for the private identity record.
func (p *PrincipalState) Validate() error {
	errs := validateMeta(KindPrincipalState, p.Namespace, p.Name)
	o := p.Spec.Owner
	if o.Kind != OwnerServiceAccount || o.UID == "" || len(o.UID) > 512 || o.ID != "sa:"+o.Namespace+"/"+o.Name || o.Provider != "kudurru" || strings.ContainsRune(o.UID, 0) {
		errs.add("spec.owner", "a complete service account identity is required")
	}
	if len(validation.IsDNS1123Subdomain(o.Name)) != 0 || len(validation.IsDNS1123Label(o.Namespace)) != 0 {
		errs.add("spec.owner", "valid account name and namespace are required")
	}
	if p.Name != PrincipalStateName(o) {
		errs.add("metadata.name", "must match the account identity")
	}
	return errs.err(KindPrincipalState, p.Namespace, p.Name)
}
