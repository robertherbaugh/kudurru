package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Schedule limits when a policy applies.
type Schedule struct {
	// +optional
	Weekdays []string `json:"weekdays,omitempty"`
	// Start and End are "HH:MM" in TZ.
	// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
	Start string `json:"start"`
	// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
	End string `json:"end"`
	// +kubebuilder:default=UTC
	// +optional
	TZ string `json:"tz,omitempty"`
}

// Rewrite rewrites answers for matching names (v0.2).
type Rewrite struct {
	Name string `json:"name"`
	// +optional
	Type string `json:"type,omitempty"`
	// +optional
	Targets []string `json:"targets,omitempty"`
	// +optional
	CNAME string `json:"cname,omitempty"`
}

// PolicySpec binds client groups to lists and resolver behaviour.
type PolicySpec struct {
	// +optional
	ClientGroupRefs []string `json:"clientGroupRefs,omitempty"`
	// +optional
	BlockListRefs []string `json:"blockListRefs,omitempty"`
	// +optional
	AllowListRefs []string `json:"allowListRefs,omitempty"`
	// AllowOnly answers only names present in the allow lists.
	// +optional
	AllowOnly bool `json:"allowOnly,omitempty"`
	// +optional
	Schedule *Schedule `json:"schedule,omitempty"`
	// +optional
	UpstreamGroup string `json:"upstreamGroup,omitempty"`
	// +optional
	BlockMode BlockResponse `json:"blockMode,omitempty"`
	// +kubebuilder:default="10m"
	// +optional
	BlockTTL metav1.Duration `json:"blockTTL,omitempty,omitzero"`
	// QueryLog false excludes matching clients from the query log.
	// +optional
	QueryLog *bool `json:"queryLog,omitempty"`
	// Priority breaks ties when several policies match; highest wins.
	// +optional
	Priority int `json:"priority,omitempty"`
	// +optional
	Rewrites []Rewrite `json:"rewrites,omitempty"`
	// BlockedServices names services of the blocked-services catalogue
	// (GET /api/v1/catalogue/services: youtube, tiktok, ...) whose domains
	// this policy blocks like a block list; the query log names the
	// service as the list, "service:<id>". IDs are checked against the
	// catalogue in service, which can change at runtime, so an unknown ID
	// is reported as Degraded{UnknownService} rather than refused.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`
	BlockedServices []string `json:"blockedServices,omitempty"`
	// SafeSearch true answers the search engines' and YouTube's host names
	// with their vendor's safe-search target (a CNAME to
	// forcesafesearch.google.com, strict.bing.com, ...; see the catalogue).
	// +optional
	SafeSearch *bool `json:"safeSearch,omitempty"`
	// SafeSearchMode is the level for engines that offer two: strict (the
	// default) or moderate. Only YouTube has two levels (Strict or
	// Moderate Restricted Mode); every other engine is always strict.
	// +kubebuilder:validation:Enum=strict;moderate
	// +optional
	SafeSearchMode string `json:"safeSearchMode,omitempty"`
}

// PolicyStatus is the observed state of a policy.
type PolicyStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Policy applies lists, schedules and resolver settings to client groups.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=pol
// +kubebuilder:printcolumn:name="GROUPS",type=string,JSONPath=`.spec.clientGroupRefs`
// +kubebuilder:printcolumn:name="PRIORITY",type=integer,JSONPath=`.spec.priority`
// +kubebuilder:printcolumn:name="READY",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type Policy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PolicySpec `json:"spec"`
	// +optional
	Status PolicyStatus `json:"status,omitempty"`
}

// PolicyList contains a list of Policy.
// +kubebuilder:object:root=true
type PolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Policy `json:"items"`
}
