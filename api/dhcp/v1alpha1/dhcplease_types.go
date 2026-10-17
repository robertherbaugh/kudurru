/*
Copyright 2026 Robert Herbaugh.

SPDX-License-Identifier: LicenseRef-Kudurru-Community
See the LICENSE file in the repository root.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// LabelScope is the DHCPScope a lease belongs to.
const LabelScope = "dhcp.kudurru.io/scope"

// DHCPLeaseIdentity is how the client identified itself: its MAC address,
// or the opaque client identifier (option 61) as base64.
type DHCPLeaseIdentity struct {
	// +kubebuilder:validation:Enum=mac;client-id
	Kind string `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2800
	Value string `json:"value"`
}

// DHCPLeaseClaim is the rank of a lease's local DNS name: the earliest live
// claimant owns a name. Ticket zero means no claim.
type DHCPLeaseClaim struct {
	// +kubebuilder:validation:MaxLength=254
	Name string `json:"name"`
	// +kubebuilder:validation:Minimum=0
	Epoch int64 `json:"epoch"`
	// +kubebuilder:validation:Minimum=1
	Ticket int64 `json:"ticket"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	Ordinal int64 `json:"ordinal,omitempty"`
}

// DHCPLeaseSpec is one address held by a client: offered (a one-minute
// hold), bound, or declined (a ten-minute quarantine after a conflict).
type DHCPLeaseSpec struct {
	// Scope is the DHCPScope name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Scope string `json:"scope"`
	// +kubebuilder:validation:MaxLength=15
	Address  string            `json:"address"`
	Identity DHCPLeaseIdentity `json:"identity"`
	// +kubebuilder:validation:MaxLength=17
	MAC string `json:"mac"`
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Hostname string `json:"hostname,omitempty"`
	// ExpiresAt is when the address becomes free again.
	ExpiresAt metav1.Time `json:"expiresAt"`
	// +kubebuilder:validation:Enum=offered;bound;declined
	State string `json:"state"`
	// +optional
	DNSClaim *DHCPLeaseClaim `json:"dnsClaim,omitempty"`
}

// DHCPLease is one DHCPv4 lease of a DHCPScope. kudurru writes these; the
// object name is "<scope>.<address>", so Kubernetes itself refuses a
// second owner for an address. Expired leases are deleted by the replica
// that serves DHCP.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=dhcplease,categories=kudurru-dhcp
// +kubebuilder:printcolumn:name="SCOPE",type=string,JSONPath=`.spec.scope`
// +kubebuilder:printcolumn:name="ADDRESS",type=string,JSONPath=`.spec.address`
// +kubebuilder:printcolumn:name="MAC",type=string,JSONPath=`.spec.mac`
// +kubebuilder:printcolumn:name="HOSTNAME",type=string,JSONPath=`.spec.hostname`
// +kubebuilder:printcolumn:name="STATE",type=string,JSONPath=`.spec.state`
// +kubebuilder:printcolumn:name="EXPIRES",type=date,JSONPath=`.spec.expiresAt`
type DHCPLease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec DHCPLeaseSpec `json:"spec"`
}

// DHCPLeaseList is a list of DHCPLease.
//
// +kubebuilder:object:root=true
type DHCPLeaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DHCPLease `json:"items"`
}

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &DHCPLease{}, &DHCPLeaseList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
