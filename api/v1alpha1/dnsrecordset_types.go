package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProviderSpecific carries a per-sink override, e.g. cloudflare/proxied.
// The shape matches external-dns so endpoints round-trip unchanged.
type ProviderSpecific struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// EndpointExt is the kudurru extension block on an Endpoint (decision D28).
type EndpointExt struct {
	// +optional
	Comment string `json:"comment,omitempty"`
	// Disabled records are kept but never served.
	// +optional
	Disabled bool `json:"disabled,omitempty"`
	// ExpiresAt removes the record from service after this time.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
	// View is "public" (published to sinks, never served) or a ClientGroup
	// name (served only to that group). v0.2.
	// +optional
	View string `json:"view,omitempty"`
}

// Endpoint is a superset of the external-dns Endpoint: one owner name and
// record type with one or more targets in presentation format.
type Endpoint struct {
	DNSName string `json:"dnsName"`
	// +kubebuilder:validation:Pattern=`^([A-Z0-9]+|TYPE[0-9]+)$`
	RecordType string `json:"recordType"`
	// +kubebuilder:validation:MinItems=1
	Targets []string `json:"targets"`
	// +optional
	RecordTTL int64 `json:"recordTTL,omitempty"`
	// +optional
	SetIdentifier string `json:"setIdentifier,omitempty"`
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// +optional
	ProviderSpecific []ProviderSpecific `json:"providerSpecific,omitempty"`
	// +optional
	Kudurru *EndpointExt `json:"kudurru,omitempty"`
}

// DNSRecordSetSpec defines records added to a zone. App teams own these:
// the clay copies of authority delegated from a DNSZone.
type DNSRecordSetSpec struct {
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="zoneRef is immutable"
	ZoneRef ObjectRef `json:"zoneRef"`
	// +kubebuilder:validation:MaxItems=1000
	Endpoints []Endpoint `json:"endpoints"`
}

// PublicationState reports whether one sink has programmed the record set.
type PublicationState struct {
	Integration string `json:"integration"`
	// +kubebuilder:validation:Enum=Accepted;Programmed;Failed
	State string `json:"state"`
	// +optional
	Message string `json:"message,omitempty"`
}

// DNSRecordSetStatus is the observed state of a record set.
type DNSRecordSetStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Names int `json:"names,omitempty"`
	// +optional
	Publications []PublicationState `json:"publications,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// DNSRecordSet is a set of records that belong to a DNSZone.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=rrset
// +kubebuilder:printcolumn:name="ZONE",type=string,JSONPath=`.spec.zoneRef.name`
// +kubebuilder:printcolumn:name="NAMES",type=integer,JSONPath=`.status.names`
// +kubebuilder:printcolumn:name="READY",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`
type DNSRecordSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec DNSRecordSetSpec `json:"spec"`
	// +optional
	Status DNSRecordSetStatus `json:"status,omitempty"`
}

// DNSRecordSetList contains a list of DNSRecordSet.
// +kubebuilder:object:root=true
type DNSRecordSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSRecordSet `json:"items"`
}
