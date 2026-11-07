package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KudurruSpec owns cluster configuration. The generator replaces Settings'
// generic Go carrier with the strict nested schema from config.Schema.
type KudurruSpec struct {
	// Settings contains only public cluster keys. Node, host-only and secret
	// settings are excluded from the generated admission schema.
	// +optional
	Settings map[string]apiextensionsv1.JSON `json:"settings,omitempty"`
}

// BlockingStatus is replicated absolute-time state, never a node-local timer.
// +kubebuilder:validation:XValidation:rule="!has(self.disabledUntil) || self.disabled",message="disabledUntil requires disabled"
type BlockingStatus struct {
	Disabled bool `json:"disabled"`
	// DisabledUntil is absent for an indefinite disable. Expiry enables
	// blocking without waiting for another write or renewing on restart.
	// +optional
	DisabledUntil *metav1.Time `json:"disabledUntil,omitempty"`
}

// KudurruStatus records runtime state and observed cluster configuration.
type KudurruStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	Blocking BlockingStatus `json:"blocking,omitempty"`
}

// Kudurru is the cluster settings singleton named default.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default'",message="the cluster settings object must be named default"
type Kudurru struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              KudurruSpec `json:"spec"`
	// +optional
	Status KudurruStatus `json:"status,omitempty"`
}

// KudurruList contains the cluster singleton.
// +kubebuilder:object:root=true
type KudurruList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Kudurru `json:"items"`
}
