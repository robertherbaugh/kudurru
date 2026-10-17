package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ZoneType selects how a zone gets its data.
// +kubebuilder:validation:Enum=primary;forward;secondary;stub;external;catalog;secondaryCatalog
type ZoneType string

// Zone types; the version in the comment is when the type becomes servable.
const (
	ZoneTypePrimary          ZoneType = "primary"
	ZoneTypeForward          ZoneType = "forward"
	ZoneTypeSecondary        ZoneType = "secondary"        // v0.2
	ZoneTypeStub             ZoneType = "stub"             // v0.2
	ZoneTypeExternal         ZoneType = "external"         // v0.2
	ZoneTypeCatalog          ZoneType = "catalog"          // v1.0
	ZoneTypeSecondaryCatalog ZoneType = "secondaryCatalog" // v1.0
)

// AccessMode is Technitium's six-way access vocabulary applied to queries,
// transfers and dynamic updates.
// +kubebuilder:validation:Enum=deny;allow;allowOnlyPrivate;allowOnlyZoneNameServers;useNetworkACL;zoneNameServersAndACL
type AccessMode string

// Access modes.
const (
	AccessDeny                     AccessMode = "deny"
	AccessAllow                    AccessMode = "allow"
	AccessAllowOnlyPrivate         AccessMode = "allowOnlyPrivate"
	AccessAllowOnlyZoneNameServers AccessMode = "allowOnlyZoneNameServers"
	AccessUseNetworkACL            AccessMode = "useNetworkACL"
	AccessZoneNameServersAndACL    AccessMode = "zoneNameServersAndACL"
)

// NotifyMode selects where NOTIFY messages are sent on serial change.
// +kubebuilder:validation:Enum=none;zoneNameServers;specified;both
type NotifyMode string

// Notify modes.
const (
	NotifyNone            NotifyMode = "none"
	NotifyZoneNameServers NotifyMode = "zoneNameServers"
	NotifySpecified       NotifyMode = "specified"
	NotifyBoth            NotifyMode = "both"
)

// SOASpec holds the SOA fields the operator controls. The serial is computed
// by the leader and lives in status (decision D12).
type SOASpec struct {
	// +optional
	MName string `json:"mname,omitempty"`
	// +optional
	RName string `json:"rname,omitempty"`
	// +kubebuilder:default=3600
	// +optional
	Refresh uint32 `json:"refresh,omitempty"`
	// +kubebuilder:default=900
	// +optional
	Retry uint32 `json:"retry,omitempty"`
	// +kubebuilder:default=1209600
	// +optional
	Expire uint32 `json:"expire,omitempty"`
	// +kubebuilder:default=300
	// +optional
	Minimum uint32 `json:"minimum,omitempty"`
}

// ZonePolicy controls who may query, transfer, be notified about and update
// the zone.
type ZonePolicy struct {
	// +kubebuilder:default=allow
	// +optional
	QueryAccess AccessMode `json:"queryAccess,omitempty"`
	// QueryACL entries are CIDRs, optionally negated with a leading "!".
	// Ordered, first match wins, default deny.
	// +optional
	QueryACL []string `json:"queryACL,omitempty"`
	// +kubebuilder:default=deny
	// +optional
	ZoneTransfer AccessMode `json:"zoneTransfer,omitempty"`
	// +optional
	TransferACL []string `json:"transferACL,omitempty"`
	// +optional
	TransferTSIGKeyRefs []string `json:"transferTSIGKeyRefs,omitempty"`
	// +kubebuilder:default=none
	// +optional
	Notify NotifyMode `json:"notify,omitempty"`
	// +optional
	NotifyTargets []string `json:"notifyTargets,omitempty"`
	// Update controls RFC 2136 dynamic updates (v0.2).
	// +kubebuilder:default=deny
	// +optional
	Update AccessMode `json:"update,omitempty"`
	// +optional
	UpdatePolicies []UpdatePolicy `json:"updatePolicies,omitempty"`
}

// UpdatePolicy grants a TSIG key the right to update names and types (v0.2).
type UpdatePolicy struct {
	TSIGKeyRef string `json:"tsigKeyRef"`
	// Name is an owner name or wildcard pattern within the zone.
	Name string `json:"name"`
	// +optional
	Types []string `json:"types,omitempty"`
}

// ForwardSpec configures a forward zone.
type ForwardSpec struct {
	// UpstreamGroup names a group in the Kudurru settings.
	// +kubebuilder:default=default
	// +optional
	UpstreamGroup string `json:"upstreamGroup,omitempty"`
	// +optional
	DNSSECValidation *bool `json:"dnssecValidation,omitempty"`
}

// SecondarySpec configures a secondary zone (v0.2).
type SecondarySpec struct {
	Primaries []string `json:"primaries"`
	// +optional
	TSIGKeyRef string `json:"tsigKeyRef,omitempty"`
	// +kubebuilder:validation:Enum=tcp;tls
	// +kubebuilder:default=tcp
	// +optional
	TransferProtocol string `json:"transferProtocol,omitempty"`
}

// StubSpec configures a stub zone (v0.2).
type StubSpec struct {
	Nameservers []string `json:"nameservers"`
}

// ExternalSpec synthesises records from Kubernetes objects (v0.2).
type ExternalSpec struct {
	// +optional
	Ingress bool `json:"ingress,omitempty"`
	// +optional
	Gateway bool `json:"gateway,omitempty"`
	// +optional
	LoadBalancerServices bool `json:"loadBalancerServices,omitempty"`
}

// DNSSECSpec is reserved for v1.0 signing. The schema exists so that the
// seam is in place (dev-docs/design-record.md §13.1).
type DNSSECSpec struct {
	Enabled bool `json:"enabled"`
	// +kubebuilder:validation:Enum=ECDSAP256SHA256;ED25519
	// +kubebuilder:default=ECDSAP256SHA256
	// +optional
	Algorithm string `json:"algorithm,omitempty"`
	// +kubebuilder:validation:Enum=nsec;nsec3
	// +kubebuilder:default=nsec
	// +optional
	Denial string `json:"denial,omitempty"`
	// +optional
	KeySecretRef *ObjectRef `json:"keySecretRef,omitempty"`
}

// RecordSetSelector restricts which DNSRecordSets may add records to the zone.
type RecordSetSelector struct {
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`
	// +optional
	LabelSelector *metav1.LabelSelector `json:"labelSelector,omitempty"`
}

// PublishRule selects records for an outbound RecordSink integration (v0.2),
// the split-horizon mechanism of decision D29.
type PublishRule struct {
	IntegrationRef ObjectRef `json:"integrationRef"`
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
	// +optional
	Types []string `json:"types,omitempty"`
	// +kubebuilder:validation:Enum=sync;upsert-only;create-only
	// +kubebuilder:default=sync
	// +optional
	Policy string `json:"policy,omitempty"`
}

// DNSZoneSpec defines the desired state of a zone.
type DNSZoneSpec struct {
	// ZoneName is the fully qualified zone name with a trailing dot.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^([a-zA-Z0-9_-]+\.)*$|^\.$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="zoneName is immutable"
	ZoneName string   `json:"zoneName"`
	Type     ZoneType `json:"type"`
	// +optional
	SOA *SOASpec `json:"soa,omitempty"`
	// +optional
	Nameservers []string `json:"nameservers,omitempty"`
	// +kubebuilder:default=300
	// +optional
	DefaultTTL uint32 `json:"defaultTTL,omitempty"`
	// +optional
	Policy ZonePolicy `json:"policy,omitempty"`
	// +optional
	Forward *ForwardSpec `json:"forward,omitempty"`
	// +optional
	Secondary *SecondarySpec `json:"secondary,omitempty"`
	// +optional
	Stub *StubSpec `json:"stub,omitempty"`
	// +optional
	External *ExternalSpec `json:"external,omitempty"`
	// +optional
	DNSSEC *DNSSECSpec `json:"dnssec,omitempty"`
	// +optional
	RecordSetSelector *RecordSetSelector `json:"recordSetSelector,omitempty"`
	// +optional
	Publish []PublishRule `json:"publish,omitempty"`
}

// PublicationStatus reports the state of one outbound sink for the zone.
type PublicationStatus struct {
	Integration string `json:"integration"`
	// +optional
	Managed int `json:"managed,omitempty"`
	// +optional
	Drift int `json:"drift,omitempty"`
	// +optional
	Conflicts int `json:"conflicts,omitempty"`
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`
}

// DNSZoneStatus is the observed state of a zone.
type DNSZoneStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Serial is stamped by the leader as max(prev+1, unix_now) whenever
	// ContentHash changes. Replicas serve this value.
	// +optional
	Serial uint32 `json:"serial,omitempty"`
	// ContentHash is the sha256 of the canonical RRsets.
	// +optional
	ContentHash string `json:"contentHash,omitempty"`
	// +optional
	RecordCount int `json:"recordCount,omitempty"`
	// +optional
	NameserverAddresses []string `json:"nameserverAddresses,omitempty"`
	// +optional
	Published []PublicationStatus `json:"published,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// DNSZone is a recorded grant of authority over a name space: the stone kept
// in the temple.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=zone
// +kubebuilder:printcolumn:name="ZONE",type=string,JSONPath=`.spec.zoneName`
// +kubebuilder:printcolumn:name="TYPE",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="SERIAL",type=integer,JSONPath=`.status.serial`
// +kubebuilder:printcolumn:name="RECORDS",type=integer,JSONPath=`.status.recordCount`
// +kubebuilder:printcolumn:name="READY",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`
type DNSZone struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec DNSZoneSpec `json:"spec"`
	// +optional
	Status DNSZoneStatus `json:"status,omitempty"`
}

// DNSZoneList contains a list of DNSZone.
// +kubebuilder:object:root=true
type DNSZoneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSZone `json:"items"`
}
