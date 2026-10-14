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

// Rewrite replaces forwarded answers for an exact name or descendant-only
// *.suffix. Type is a DNS RR type; omitted, address targets infer A or AAAA.
// CNAME is an alternative to Type CNAME with one target. CNAME responses
// return an alias that the client follows through the ordinary query path.
// +kubebuilder:validation:XValidation:rule="has(self.cname) && size(self.cname) > 0 ? (!has(self.targets) || size(self.targets) == 0) && (!has(self.type) || size(self.type) == 0 || self.type == 'CNAME') : has(self.targets) && size(self.targets) > 0",message="provide either a CNAME or targets, not both"
type Rewrite struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:Enum="";A;AAAA;CNAME;PTR;TXT;MX;SRV;HTTPS;SVCB;CAA;NS;NAPTR
	// +optional
	Type string `json:"type,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=4096
	// +optional
	Targets []string `json:"targets,omitempty"`
	// +optional
	CNAME string `json:"cname,omitempty"`
}

// PolicyResponse overrides selected global response processing for clients.
// Each field uses the highest-priority policy that explicitly sets it.
type PolicyResponse struct {
	// DNS64Prefix overrides response.dns64.prefix. An explicit empty string
	// disables inherited DNS64; omitted inherits the next policy or global value.
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="size(self) == 0 || (isCIDR(self) && cidr(self).ip().family() == 6 && cidr(self).ip().isGlobalUnicast() && cidr(self).prefixLength() in [32, 40, 48, 56, 64, 96] && cidr(self) == cidr(self).masked())",message="must be empty or a canonical IPv6 /32, /40, /48, /56, /64 or /96 prefix"
	// +optional
	DNS64Prefix *string `json:"dns64Prefix,omitempty"`
	// FilterAAAA overrides response.filterAAAA. False explicitly disables it.
	// +optional
	FilterAAAA *bool `json:"filterAAAA,omitempty"`
}

// PolicySpec binds client groups to lists and resolver behavior.
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
	// +kubebuilder:validation:MaxItems=128
	// +optional
	Rewrites []Rewrite `json:"rewrites,omitempty"`
	// +optional
	Response *PolicyResponse `json:"response,omitempty"`
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
	// BlockedCategories names categories of the catalogue
	// (GET /api/v1/catalogue/categories: ai, social, gaming, ...); every
	// service in them is blocked as if listed in blockedServices, including
	// services a later catalogue adds to the category. Unknown IDs are
	// reported as Degraded{UnknownCategory}.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`
	BlockedCategories []string `json:"blockedCategories,omitempty"`
	// AllowedServices are exceptions to blockedCategories: services of a
	// blocked category this policy does not block. They do not cancel an
	// ID listed in blockedServices, nor another policy's blocks; allow
	// lists still win over every service.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`
	AllowedServices []string `json:"allowedServices,omitempty"`
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
