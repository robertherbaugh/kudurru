package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ListFormat is the syntax of a blocklist source.
// +kubebuilder:validation:Enum=auto;hosts;domains;adblock;regex
type ListFormat string

// ListMode says whether entries block or allow.
// +kubebuilder:validation:Enum=block;allow
type ListMode string

// BlockResponse is how a blocked query is answered: nxdomain, zeroIP
// (0.0.0.0 / ::), refused, customIP (the customIP addresses) or blockPage
// (the addresses of the block page listener, blockPage.ipv4/ipv6, so
// browsers show why the name is blocked).
// +kubebuilder:validation:Enum=nxdomain;zeroIP;refused;customIP;blockPage
type BlockResponse string

// ListSource is one place a BlockList reads entries from. Exactly one of
// URL, Inline or ConfigMapRef is set.
// +kubebuilder:validation:XValidation:rule="[has(self.url), has(self.inline), has(self.configMapRef)].filter(x, x).size() == 1",message="exactly one of url, inline or configMapRef must be set"
type ListSource struct {
	// +optional
	URL string `json:"url,omitempty"`
	// +optional
	Inline []string `json:"inline,omitempty"`
	// +optional
	ConfigMapRef *ObjectRef `json:"configMapRef,omitempty"`
	// +kubebuilder:default=auto
	// +optional
	Format ListFormat `json:"format,omitempty"`
	// +optional
	Name string `json:"name,omitempty"`
	// Subdomains makes the hosts and plain-domain entries of this source
	// match their subdomains too, for lists published as "domains with
	// subdomains". Adblock and wildcard rules are not affected.
	// +optional
	Subdomains bool `json:"subdomains,omitempty"`
}

// CustomIP is the answer for BlockResponse customIP.
type CustomIP struct {
	// +optional
	V4 string `json:"v4,omitempty"`
	// +optional
	V6 string `json:"v6,omitempty"`
}

// BlockListSpec defines a set of sources compiled into one matcher chain.
// +kubebuilder:validation:XValidation:rule="!has(self.response) || self.response != 'customIP' || has(self.customIP)",message="response customIP requires the customIP block"
type BlockListSpec struct {
	// +kubebuilder:default=block
	// +optional
	Mode ListMode `json:"mode,omitempty"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	Sources []ListSource `json:"sources"`
	// +kubebuilder:default="24h"
	// +optional
	RefreshInterval metav1.Duration `json:"refreshInterval,omitempty,omitzero"`
	// +optional
	Response BlockResponse `json:"response,omitempty"`
	// +optional
	CustomIP *CustomIP `json:"customIP,omitempty"`
}

// SourceStatus reports the last fetch of one source.
type SourceStatus struct {
	Name string `json:"name"`
	// Digest is the sha256 of the fetched body; replicas pull by digest
	// from the leader (decision D15).
	// +optional
	Digest string `json:"digest,omitempty"`
	// +optional
	ETag string `json:"etag,omitempty"`
	// +optional
	Entries int `json:"entries,omitempty"`
	// +optional
	Invalid int `json:"invalid,omitempty"`
	// +optional
	LastFetched *metav1.Time `json:"lastFetched,omitempty"`
	// +optional
	Error string `json:"error,omitempty"`
}

// BlockListStatus is the observed state of a block list.
type BlockListStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Entries int `json:"entries,omitempty"`
	// +optional
	RuleCount int `json:"ruleCount,omitempty"`
	// +optional
	Invalid int `json:"invalid,omitempty"`
	// Unsupported counts valid rules this version parses but does not
	// apply: modifiers such as $important and $badfilter, mid-pattern
	// wildcards, hosts rewrites.
	// +optional
	Unsupported int `json:"unsupported,omitempty"`
	// +optional
	LastFetched *metav1.Time `json:"lastFetched,omitempty"`
	// +optional
	Sources []SourceStatus `json:"sources,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// BlockList is a named, refreshable set of block or allow entries.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=blocklist
// +kubebuilder:printcolumn:name="MODE",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="ENTRIES",type=integer,JSONPath=`.status.entries`
// +kubebuilder:printcolumn:name="LAST-FETCH",type=date,JSONPath=`.status.lastFetched`
// +kubebuilder:printcolumn:name="READY",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type BlockList struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec BlockListSpec `json:"spec"`
	// +optional
	Status BlockListStatus `json:"status,omitempty"`
}

// BlockListList contains a list of BlockList.
// +kubebuilder:object:root=true
type BlockListList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BlockList `json:"items"`
}
