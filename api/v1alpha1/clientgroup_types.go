package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClientMatcher identifies clients. Exactly one of the selector fields is set.
// +kubebuilder:validation:XValidation:rule="[has(self.ip), has(self.cidr), has(self.mac), has(self.clientID), has(self.hostname), has(self.nameGlob)].filter(x, x).size() == 1",message="exactly one of ip, cidr, mac, clientID, hostname or nameGlob must be set"
type ClientMatcher struct {
	// +optional
	IP string `json:"ip,omitempty"`
	// +optional
	CIDR string `json:"cidr,omitempty"`
	// MAC matches through an inventory source such as UniFi (v0.2).
	// +optional
	MAC string `json:"mac,omitempty"`
	// ClientID matches DoT SNI / DoH path client identifiers (v0.2).
	// +optional
	ClientID string `json:"clientID,omitempty"`
	// +optional
	Hostname string `json:"hostname,omitempty"`
	// +optional
	NameGlob string `json:"nameGlob,omitempty"`
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	Comment string `json:"comment,omitempty"`
}

// ClientGroupSources lets an integration add members (v0.2).
type ClientGroupSources struct {
	// +optional
	IntegrationRef *ObjectRef `json:"integrationRef,omitempty"`
}

// ClientGroupSpec defines which clients belong to the group.
type ClientGroupSpec struct {
	// RateLimitExempt skips UDP per-prefix rate limiting for clients selected
	// into this group by the winning identification tier. A true value in any
	// tied group wins; groups in lower tiers do not contribute. This does not
	// grant authentication or bypass DNS policy, ACLs or message validation.
	// +optional
	RateLimitExempt bool `json:"rateLimitExempt,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxItems=1000
	Clients []ClientMatcher `json:"clients,omitempty"`
	// +optional
	Sources *ClientGroupSources `json:"sources,omitempty"`
}

// ClientGroupStatus is the observed state of a client group.
type ClientGroupStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Members int `json:"members,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ClientGroup names a set of clients so that Policies can target them.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cg
// +kubebuilder:printcolumn:name="MEMBERS",type=integer,JSONPath=`.status.members`
// +kubebuilder:printcolumn:name="READY",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type ClientGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ClientGroupSpec `json:"spec"`
	// +optional
	Status ClientGroupStatus `json:"status,omitempty"`
}

// ClientGroupList contains a list of ClientGroup.
// +kubebuilder:object:root=true
type ClientGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClientGroup `json:"items"`
}
