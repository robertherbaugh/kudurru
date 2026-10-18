package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KindDHCPScope is the kind name of DHCPScope.
const KindDHCPScope = "DHCPScope"

// DHCPScope conditions and reasons.
const (
	// ReasonLeaseConflict: the scope applies, but some live leases are no
	// longer allowed by it; they are not renewed and end at expiry.
	ReasonLeaseConflict = "LeaseConflict"
	// ReasonNotServing: no replica holds the DHCP allocator term.
	ReasonNotServing = "NotServing"
)

// DHCPTrustedRelay authorizes one DHCP relay: the exact source address and
// gateway address (giaddr) of its packets for this subnet.
type DHCPTrustedRelay struct {
	// SourceIP is the IPv4 source address of the relay's packets as kudurru
	// receives them (the LoadBalancer Service must preserve it).
	// +kubebuilder:validation:MaxLength=15
	SourceIP string `json:"sourceIP"`
	// GatewayIP is the giaddr the relay writes for this subnet; it is never
	// allocated.
	// +kubebuilder:validation:MaxLength=15
	GatewayIP string `json:"gatewayIP"`
	// SourcePort is the relay's UDP source port; zero means 67. An
	// alternate port requires the RFC 8357 relay port suboption.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	SourcePort int32 `json:"sourcePort,omitempty"`
	// LinkSelection is the exact RFC 3527 link selection address the relay
	// sends, inside this subnet; empty refuses the extension.
	// +optional
	// +kubebuilder:validation:MaxLength=15
	LinkSelection string `json:"linkSelection,omitempty"`
	// ServerIdentifier is the exact RFC 5107 server identifier override the
	// relay sends; empty refuses the extension.
	// +optional
	// +kubebuilder:validation:MaxLength=15
	ServerIdentifier string `json:"serverIdentifier,omitempty"`
}

// DHCPReservation assigns an address, and optionally a hostname, to one
// Ethernet MAC address.
type DHCPReservation struct {
	// +kubebuilder:validation:MaxLength=17
	MAC string `json:"mac"`
	// +kubebuilder:validation:MaxLength=15
	IP string `json:"ip"`
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Hostname string `json:"hostname,omitempty"`
}

// DHCPClasslessRoute is one RFC 3442 classless static route (option 121).
type DHCPClasslessRoute struct {
	// +kubebuilder:validation:MaxLength=18
	Destination string `json:"destination"`
	// +kubebuilder:validation:MaxLength=15
	Router string `json:"router"`
}

// DHCPBootProfile selects boot information by client architecture
// (option 93).
type DHCPBootProfile struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	Architecture int32 `json:"architecture"`
	// +optional
	// +kubebuilder:validation:MaxLength=15
	BootServer string `json:"bootServer,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=64
	BootServerName string `json:"bootServerName,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=128
	BootFile string `json:"bootFile,omitempty"`
}

// DHCPScopeSpec is one DHCPv4 address pool and the options its clients
// receive. Clients reach kudurru through the trusted relays; the server
// address clients see is a node setting (dhcp.serverIP), shared by every
// scope.
type DHCPScopeSpec struct {
	// Subnet is the client IPv4 subnet in canonical CIDR form; it must not
	// overlap another scope.
	// +kubebuilder:validation:MaxLength=18
	Subnet string `json:"subnet"`
	// RangeStart and RangeEnd bound the dynamic pool, inclusive.
	// +kubebuilder:validation:MaxLength=15
	RangeStart string `json:"rangeStart"`
	// +kubebuilder:validation:MaxLength=15
	RangeEnd string `json:"rangeEnd"`
	// Router is the default gateway advertised to clients.
	// +kubebuilder:validation:MaxLength=15
	Router string `json:"router"`
	// DNSServers are the IPv4 DNS servers advertised to clients.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=63
	// +kubebuilder:validation:items:MaxLength=15
	DNSServers []string `json:"dnsServers"`
	// DomainName is the client domain, also the domain of local DNS records
	// when registerDNS is set.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	DomainName string `json:"domainName,omitempty"`
	// LeaseDuration is in whole seconds, from one minute to 30 days.
	// +kubebuilder:default="12h"
	// +optional
	LeaseDuration metav1.Duration `json:"leaseDuration,omitempty,omitzero"`
	// RegisterDNS publishes A and PTR records for bound leases on every
	// replica.
	// +optional
	RegisterDNS bool `json:"registerDNS,omitempty"`
	// DNSTTL is the TTL of those records, from 1s to 1h, clamped to the
	// remaining lease time.
	// +kubebuilder:default="30s"
	// +optional
	DNSTTL metav1.Duration `json:"dnsTTL,omitempty,omitzero"`
	// TrustedRelays are the relays allowed to forward this subnet's
	// clients. At least one is required.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	TrustedRelays []DHCPTrustedRelay `json:"trustedRelays"`
	// ConflictDetection probes a new address before offering it: icmp
	// (default; needs NET_RAW) or disabled.
	// +kubebuilder:validation:Enum=auto;icmp;disabled
	// +optional
	ConflictDetection string `json:"conflictDetection,omitempty"`
	// ProbeTimeout bounds the ICMP probe; default 200ms.
	// +optional
	ProbeTimeout metav1.Duration `json:"probeTimeout,omitempty,omitzero"`
	// +optional
	// +kubebuilder:validation:MaxItems=63
	// +kubebuilder:validation:items:MaxLength=15
	NTPServers []string `json:"ntpServers,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=253
	DomainSearch []string `json:"domainSearch,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=32
	ClasslessRoutes []DHCPClasslessRoute `json:"classlessRoutes,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=15
	BootServer string `json:"bootServer,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=64
	BootServerName string `json:"bootServerName,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=128
	BootFile string `json:"bootFile,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=32
	BootProfiles []DHCPBootProfile `json:"bootProfiles,omitempty"`
	// Reservations assign fixed addresses to MAC addresses.
	// +optional
	// +kubebuilder:validation:MaxItems=4096
	Reservations []DHCPReservation `json:"reservations,omitempty"`
}

// DHCPScopeStatus is written by the replica that serves DHCP.
type DHCPScopeStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ServingPod is the replica that holds the DHCP allocator term.
	// +optional
	ServingPod string `json:"servingPod,omitempty"`
	// ActiveLeases counts bound leases.
	// +optional
	ActiveLeases int `json:"activeLeases,omitempty"`
	// ConflictingLeases counts live leases this spec no longer allows; they
	// are not renewed and end at expiry.
	// +optional
	ConflictingLeases int `json:"conflictingLeases,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// DHCPScope is a DHCPv4 address pool served by kudurru in Kubernetes. Leases
// are DHCPLease objects (dhcp.kudurru.io); one replica allocates at a time.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=dhcpscope
// +kubebuilder:printcolumn:name="SUBNET",type=string,JSONPath=`.spec.subnet`
// +kubebuilder:printcolumn:name="LEASES",type=integer,JSONPath=`.status.activeLeases`
// +kubebuilder:printcolumn:name="SERVING",type=string,JSONPath=`.status.servingPod`
// +kubebuilder:printcolumn:name="READY",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`
type DHCPScope struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec DHCPScopeSpec `json:"spec"`
	// +optional
	Status DHCPScopeStatus `json:"status,omitempty"`
}

// DHCPScopeList contains a list of DHCPScope.
// +kubebuilder:object:root=true
type DHCPScopeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DHCPScope `json:"items"`
}
