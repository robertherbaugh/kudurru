package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// CredentialSecretRef names a key in the operator's credential namespace.
// Namespace selection belongs to host configuration, never to this reference.
type CredentialSecretRef struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key"`
}

// CredentialReference selects one secret capability. File and Env are usable
// only when explicitly allowlisted by the node operator; a public object cannot
// acquire arbitrary filesystem or environment access. Values never enter spec.
// +kubebuilder:validation:XValidation:rule="[has(self.secretRef), has(self.file), has(self.env)].filter(x, x).size() == 1",message="exactly one credential reference is required"
type CredentialReference struct {
	// +optional
	SecretRef *CredentialSecretRef `json:"secretRef,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	// +optional
	File string `json:"file,omitempty"`
	// +kubebuilder:validation:Pattern=`^[A-Za-z_][A-Za-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=128
	// +optional
	Env string `json:"env,omitempty"`
}

// PublicationSelection reads original desired records, including public-only
// endpoints withheld from the internal DNS view.
type PublicationSelection struct {
	// +kubebuilder:validation:MaxProperties=64
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	// +optional
	Types []string `json:"types,omitempty"`
	// Exclude contains exact names or leading *. suffix patterns.
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=253
	// +optional
	Exclude []string `json:"exclude,omitempty"`
}

// CloudflareDefaults supplies provider values absent from an endpoint.
type CloudflareDefaults struct {
	// +optional
	Proxied bool `json:"proxied,omitempty"`
	// TTL 1 means provider automatic TTL; proxied records always use it.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	// +optional
	TTL int64 `json:"ttl,omitempty"`
	// +kubebuilder:validation:MaxLength=500
	// +optional
	Comment string `json:"comment,omitempty"`
}

// PublicationAdoption requires an explicit external-dns ownership identity.
type PublicationAdoption struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ExternalDNSOwner string `json:"externalDNSOwner"`
}

// CloudflareZone configures one destination and its ownership policy.
type CloudflareZone struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Zone string `json:"zone"`
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{32}$`
	// +optional
	ZoneID string `json:"zoneID,omitempty"`
	// +optional
	Select PublicationSelection `json:"select,omitempty"`
	// +optional
	Defaults CloudflareDefaults `json:"defaults,omitempty"`
	// +kubebuilder:validation:Enum=sync;upsert-only;create-only
	// +kubebuilder:default=sync
	// +optional
	Policy string `json:"policy,omitempty"`
	// +optional
	AdoptFrom *PublicationAdoption `json:"adoptFrom,omitempty"`
	// AdoptUnowned never permits overwriting a foreign ownership marker.
	// +optional
	AdoptUnowned bool `json:"adoptUnowned,omitempty"`
	// Translate maps equal-sized IP prefixes before publication.
	// +kubebuilder:validation:MaxProperties=64
	// +optional
	Translate map[string]string `json:"translate,omitempty"`
}

// CloudflareIntegration selects destinations for owned public records.
type CloudflareIntegration struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +listType=map
	// +listMapKey=zone
	Zones []CloudflareZone `json:"zones"`
}

// UniFiInventory controls generated records independently of query-log traffic.
type UniFiInventory struct {
	ZoneRef ObjectRef `json:"zoneRef"`
	// +kubebuilder:validation:Enum=auto;disabled
	// +kubebuilder:default=auto
	// +optional
	ReverseZones string `json:"reverseZones,omitempty"`
	// NameTemplate uses the bounded inventory name formatter, never arbitrary
	// Go template execution or filesystem/network functions.
	// +kubebuilder:validation:MaxLength=500
	// +optional
	NameTemplate string `json:"nameTemplate,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	// +kubebuilder:default=120
	// +optional
	TTL int64 `json:"ttl,omitempty"`
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=253
	// +listType=set
	// +optional
	IncludeNetworks []string `json:"includeNetworks,omitempty"`
}

// UniFiGroups selects the provider fields used for client group membership.
type UniFiGroups struct {
	// ByNetwork maps provider network names to desired ClientGroup names.
	// +kubebuilder:validation:MaxProperties=256
	// +optional
	ByNetwork map[string]string `json:"byNetwork,omitempty"`
	// +kubebuilder:validation:MaxLength=128
	// +optional
	ByNote string `json:"byNote,omitempty"`
}

// UniFiRecordMirror controls optional record publication to the gateway.
type UniFiRecordMirror struct {
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// +kubebuilder:validation:MaxItems=2
	// +kubebuilder:validation:items:Enum=A;CNAME
	// +listType=set
	// +optional
	Types []string `json:"types,omitempty"`
}

// UniFiPush defaults every external write to disabled.
type UniFiPush struct {
	// +optional
	DHCPDNSServers bool `json:"dhcpDNSServers,omitempty"`
	// +optional
	MirrorRecords UniFiRecordMirror `json:"mirrorRecords,omitempty"`
}

// UniFiIntegration configures inventory polling and explicitly enabled writes.
type UniFiIntegration struct {
	// +kubebuilder:validation:Pattern=`^https://[^/]+.*$`
	// +kubebuilder:validation:MaxLength=2048
	Endpoint string `json:"endpoint"`
	// TLSFingerprint pins the provider certificate for a private CA deployment.
	// +kubebuilder:validation:Pattern=`^sha256:[a-fA-F0-9]{64}$`
	// +optional
	TLSFingerprint string `json:"tlsFingerprint,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:default=default
	// +optional
	Site string `json:"site,omitempty"`
	// +kubebuilder:default="60s"
	// +optional
	Poll metav1.Duration `json:"poll,omitempty,omitzero"`
	// Legacy enables version-detected supplementary inventory endpoints and
	// explicitly authorized legacy actions; it never enables them by default.
	// +optional
	Legacy bool `json:"legacy,omitempty"`
	// +optional
	Inventory *UniFiInventory `json:"inventory,omitempty"`
	// +optional
	Groups UniFiGroups `json:"groups,omitempty"`
	// +optional
	Push UniFiPush `json:"push,omitempty"`
}

// WebhookIntegration connects an independently deployed Tier-B provider.
type WebhookIntegration struct {
	// +kubebuilder:validation:Pattern=`^https?://[^/]+.*$`
	// +kubebuilder:validation:MaxLength=2048
	Endpoint string `json:"endpoint"`
	// +kubebuilder:default="10s"
	// +optional
	Timeout metav1.Duration `json:"timeout,omitempty,omitzero"`
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=253
	// +optional
	Domains []string `json:"domains,omitempty"`
	// +kubebuilder:validation:Enum=sync;upsert-only;create-only
	// +kubebuilder:default=sync
	// +optional
	Policy string `json:"policy,omitempty"`
}

// IntegrationSpec is one independently elected background integration.
// +kubebuilder:validation:XValidation:rule="has(self.cloudflare) == (self.type == 'cloudflare')",message="cloudflare settings must match type"
// +kubebuilder:validation:XValidation:rule="has(self.unifi) == (self.type == 'unifi')",message="unifi settings must match type"
// +kubebuilder:validation:XValidation:rule="has(self.webhook) == (self.type == 'webhook')",message="webhook settings must match type"
// +kubebuilder:validation:XValidation:rule="self.type != 'cloudflare' || (has(self.credentials) && 'apiToken' in self.credentials)",message="cloudflare requires an apiToken credential reference"
// +kubebuilder:validation:XValidation:rule="self.type != 'unifi' || (has(self.credentials) && 'apiKey' in self.credentials)",message="unifi requires an apiKey credential reference"
type IntegrationSpec struct {
	// +kubebuilder:validation:Enum=cloudflare;unifi;webhook
	Type string `json:"type"`
	// +kubebuilder:validation:MaxProperties=16
	// +optional
	Credentials map[string]CredentialReference `json:"credentials,omitempty"`
	// +optional
	Cloudflare *CloudflareIntegration `json:"cloudflare,omitempty"`
	// +optional
	UniFi *UniFiIntegration `json:"unifi,omitempty"`
	// +optional
	Webhook *WebhookIntegration `json:"webhook,omitempty"`
	// +kubebuilder:default="10m"
	// +optional
	Resync metav1.Duration `json:"resync,omitempty,omitzero"`
	// +optional
	Suspend bool `json:"suspend,omitempty"`
	// DeletionPolicy applies only to records proven owned by this instance.
	// +kubebuilder:validation:Enum=Retain;Delete
	// +kubebuilder:default=Retain
	// +optional
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

// IntegrationRecordCounts describes the last publication plan.
type IntegrationRecordCounts struct {
	// +kubebuilder:validation:Minimum=0
	Managed int64 `json:"managed"`
	// +kubebuilder:validation:Minimum=0
	Drift int64 `json:"drift"`
	// +kubebuilder:validation:Minimum=0
	Conflicts int64 `json:"conflicts"`
}

// IntegrationStatus reports observed execution without credential values.
type IntegrationStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`
	// +optional
	Records IntegrationRecordCounts `json:"records,omitempty"`
	// +optional
	Clients int64 `json:"clients,omitempty"`
	// CredentialFingerprints identify rotated secrets without revealing them.
	// +kubebuilder:validation:MaxProperties=16
	// +optional
	CredentialFingerprints map[string]string `json:"credentialFingerprints,omitempty"`
}

// Integration configures background publication, inventory, or a webhook.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=intg
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspend`
type Integration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              IntegrationSpec `json:"spec"`
	// +optional
	Status IntegrationStatus `json:"status,omitempty"`
}

// IntegrationList contains a list of Integration.
// +kubebuilder:object:root=true
type IntegrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Integration `json:"items"`
}
