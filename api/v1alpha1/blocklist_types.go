package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ListFormat is the syntax of a blocklist source.
// +kubebuilder:validation:Enum=auto;hosts;domains;adblock;regex;rpz
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

// EntryType is how a structured list entry matches.
// +kubebuilder:validation:Enum=exact;wildcard;regex
type EntryType string

// Entry types.
const (
	// EntryExact matches the name only.
	EntryExact EntryType = "exact"
	// EntryWildcard ("*.example.com") matches the name and every name
	// below it, like the adblock rule ||example.com^.
	EntryWildcard EntryType = "wildcard"
	// EntryRegex matches every name the RE2 expression matches.
	EntryRegex EntryType = "regex"
)

// EntryOrigin records how a structured entry was added.
// +kubebuilder:validation:Enum=manual;querylog;unblock;api;migrated
type EntryOrigin string

// Entry origins.
const (
	OriginManual   EntryOrigin = "manual"
	OriginQueryLog EntryOrigin = "querylog"
	OriginUnblock  EntryOrigin = "unblock"
	OriginAPI      EntryOrigin = "api"
	OriginMigrated EntryOrigin = "migrated"
)

// ListEntry is one structured entry of a list: the dashboard's Allowlist
// and Denylist keep their domains as entries of the lists ui-allow,
// ui-deny, ui-allow-<group> and ui-deny-<group>, each with its own switch,
// comment and provenance (Pi-hole's Domains page).
type ListEntry struct {
	// Value is the canonical form for Type: "ads.example.com" (exact),
	// "*.example.com" (wildcard) or the RE2 expression without slashes
	// (regex).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Value string `json:"value"`
	// +kubebuilder:default=exact
	// +optional
	Type EntryType `json:"type,omitempty"`
	// Enabled false keeps the entry without applying it. Omitted means
	// true.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// +kubebuilder:validation:MaxLength=500
	// +optional
	Comment string `json:"comment,omitempty"`
	// +optional
	Created *metav1.Time `json:"created,omitempty"`
	// CreatedBy is the actor that added the entry ("user:alice").
	// +kubebuilder:validation:MaxLength=300
	// +optional
	CreatedBy string `json:"createdBy,omitempty"`
	// +optional
	Modified *metav1.Time `json:"modified,omitempty"`
	// +optional
	Origin EntryOrigin `json:"origin,omitempty"`
	// Request is the UnblockRequest ("namespace/name") whose approval
	// added the entry.
	// +kubebuilder:validation:MaxLength=300
	// +optional
	Request string `json:"request,omitempty"`
}

// IsEnabled reports whether the entry is switched on (Enabled unset or
// true).
func (e *ListEntry) IsEnabled() bool { return e.Enabled == nil || *e.Enabled }

// EntryRule renders a structured entry as the list rule it compiles to:
// "example.com", "*.example.com" or "/expression/".
func EntryRule(t EntryType, value string) string {
	switch t {
	case EntryRegex:
		return "/" + value + "/"
	case EntryWildcard:
		if len(value) > 2 && value[:2] == "*." {
			return value
		}
		return "*." + value
	}
	return value
}

// RPZSource acquires an external response policy zone independently on each
// node. It is policy data, not a locally served authoritative DNSZone.
type RPZSource struct {
	// ZoneName is the non-root absolute policy-zone origin.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)*[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.?$`
	ZoneName string `json:"zoneName"`
	// Primaries are ordered transfer endpoints (host or host:port). IPv6
	// endpoints with a port use brackets. TLS verifies the endpoint hostname.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=300
	Primaries []string `json:"primaries"`
	// +kubebuilder:default=tcp
	// +kubebuilder:validation:Enum=tcp;tls
	// +optional
	TransferProtocol string `json:"transferProtocol,omitempty"`
	// TSIGKeyRef names an operator-managed key, never secret material.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	TSIGKeyRef string `json:"tsigKeyRef,omitempty"`
}

// ListSource is one place a BlockList reads entries from. Exactly one of
// URL, Inline, ConfigMapRef, Entries or RPZ is set.
// +kubebuilder:validation:XValidation:rule="[has(self.url), has(self.inline), has(self.configMapRef), has(self.entries), has(self.rpz)].filter(x, x).size() == 1",message="exactly one of url, inline, configMapRef, entries or rpz must be set"
// +kubebuilder:validation:XValidation:rule="has(self.rpz) == (has(self.format) && self.format == 'rpz')",message="RPZ transfer configuration requires format rpz, and format rpz requires RPZ configuration"
// +kubebuilder:validation:XValidation:rule="!has(self.rpz) || !has(self.subdomains) || !self.subdomains",message="RPZ wildcard semantics cannot be overridden with subdomains"
type ListSource struct {
	// +optional
	URL string `json:"url,omitempty"`
	// +optional
	Inline []string `json:"inline,omitempty"`
	// +optional
	ConfigMapRef *ObjectRef `json:"configMapRef,omitempty"`
	// RPZ transfers a complete policy zone using AXFR/IXFR. Missing or expired
	// enabled data fails closed for policies selecting this required source.
	// +optional
	RPZ *RPZSource `json:"rpz,omitempty"`
	// Entries are structured entries (value, type, switch, comment,
	// provenance), as the dashboard's Allowlist and Denylist write them.
	// They compile as adblock rules; format and subdomains do not apply.
	// +kubebuilder:validation:MaxItems=10000
	// +optional
	Entries []ListEntry `json:"entries,omitempty"`
	// Exclude switches off single entries of this source without editing
	// it, also for URL and ConfigMap sources: each value is a rule as the
	// source's format normalises it ("ads.example.com" for a hosts line
	// "0.0.0.0 ads.example.com", "||ads.example.com^", "/regex/"), as
	// GET /blocklists/{namespace}/{name}/sources/{source}/entries lists
	// them. Excluded rules are not compiled; the exclusions survive
	// refreshes, and values that match nothing in the current body are
	// reported in status.sources[].staleExcludes.
	// +kubebuilder:validation:MaxItems=10000
	// +listType=set
	// +optional
	Exclude []string `json:"exclude,omitempty"`
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
	// Enabled false switches this one source off without removing it, like
	// Pi-hole's per-adlist switch: it is not fetched and not compiled, its
	// cached body and status digest are kept, and status.sources[] reports
	// it with enabled: false. Omitted means true. The kudurru.io/enabled
	// annotation switches the whole list off.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
}

// IsEnabled reports whether the source is switched on (Enabled unset or
// true).
func (s *ListSource) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// CustomIP is the answer for BlockResponse customIP.
type CustomIP struct {
	// +optional
	V4 string `json:"v4,omitempty"`
	// +optional
	V6 string `json:"v6,omitempty"`
}

// BlockListSpec defines a set of sources compiled into one matcher chain.
// +kubebuilder:validation:XValidation:rule="!has(self.response) || self.response != 'customIP' || has(self.customIP)",message="response customIP requires the customIP block"
// +kubebuilder:validation:XValidation:rule="!has(self.mode) || self.mode != 'allow' || self.sources.all(s, !has(s.rpz))",message="RPZ sources require block mode"
type BlockListSpec struct {
	// +kubebuilder:default=block
	// +optional
	Mode ListMode `json:"mode,omitempty"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	Sources []ListSource `json:"sources"`
	// RefreshInterval overrides blocking.refreshInterval for this list. When
	// omitted, the current cluster default applies, including hot reloads.
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
	// Enabled is false for a source switched off with
	// spec.sources[].enabled and absent otherwise. A disabled source keeps
	// the digest, etag, lastFetched and counts of its last fetch and
	// reports no error; status.entries leaves it out.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
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
	// Excluded counts the entries of this source that are switched off
	// (spec.sources[].exclude hits, or entries with enabled: false); they
	// are not in Entries.
	// +optional
	Excluded int `json:"excluded,omitempty"`
	// StaleExcludes are spec.sources[].exclude values that match no rule
	// of the current body (at most 50): the list dropped the entry, or the
	// value is not written the way the source's format normalises it.
	// +optional
	StaleExcludes []string `json:"staleExcludes,omitempty"`
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
	// apply, including hosts rewrites and RPZ triggers requiring resolver NS
	// provenance. Supported modifiers are not counted as unsupported.
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
