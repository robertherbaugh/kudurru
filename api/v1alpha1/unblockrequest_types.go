package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KindUnblockRequest is the kind name of UnblockRequest.
const KindUnblockRequest = "UnblockRequest"

// UnblockRequestState is the lifecycle state of an UnblockRequest.
// +kubebuilder:validation:Enum=Pending;Approved;Denied;Expired
type UnblockRequestState string

// UnblockRequest states. An empty status.state is Pending.
const (
	UnblockPending  UnblockRequestState = "Pending"
	UnblockApproved UnblockRequestState = "Approved"
	UnblockDenied   UnblockRequestState = "Denied"
	// UnblockExpired: a pending request nobody decided in time, or a
	// time-limited approval whose allow entry has been removed.
	UnblockExpired UnblockRequestState = "Expired"
)

// UnblockClientPrivacy says how spec.client was recorded under
// querylog.privacy.
// +kubebuilder:validation:Enum=full;prefix;hidden
type UnblockClientPrivacy string

// Client recording modes.
const (
	// UnblockClientFull: spec.client is the client address.
	UnblockClientFull UnblockClientPrivacy = "full"
	// UnblockClientPrefix: spec.client is the client's /24 or /48.
	UnblockClientPrefix UnblockClientPrivacy = "prefix"
	// UnblockClientHidden: no client is recorded (pseudonym and stricter
	// privacy modes).
	UnblockClientHidden UnblockClientPrivacy = "hidden"
)

// UnblockBlock is why the name was blocked when the request was made, as
// the block page found it.
type UnblockBlock struct {
	// Kind is list, service, safesearch, policy (allow-only) or cname.
	// +optional
	// +kubebuilder:validation:MaxLength=32
	Kind string `json:"kind,omitempty"`
	// List is the deciding list ("ns/name", "service:<id>",
	// "safesearch:<engine>").
	// +optional
	// +kubebuilder:validation:MaxLength=300
	List string `json:"list,omitempty"`
	// Rule is the matching rule text.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Rule string `json:"rule,omitempty"`
	// Policy is the policy that applied to the client, "ns/name".
	// +optional
	// +kubebuilder:validation:MaxLength=300
	Policy string `json:"policy,omitempty"`
}

// UnblockRequestSpec is what a user asked for on the block page. The block
// page writes it; administrators decide through the API (status).
type UnblockRequestSpec struct {
	// Domain is the blocked name, from the Host header or TLS server name
	// of the block page request (never from the form).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Domain string `json:"domain"`
	// Client is the requester as querylog.privacy allows recording it:
	// the address (full), its /24 or /48 (prefix), or empty (hidden).
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Client string `json:"client,omitempty"`
	// ClientPrivacy says how Client was recorded.
	// +optional
	ClientPrivacy UnblockClientPrivacy `json:"clientPrivacy,omitempty"`
	// Groups are the client's ClientGroups ("ns/name") at request time.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=300
	Groups []string `json:"groups,omitempty"`
	// Policies are the policies that applied to the client, highest
	// priority first.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=300
	Policies []string `json:"policies,omitempty"`
	// Node is the node whose block page took the request.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Node string `json:"node,omitempty"`
	// RequesterName and Contact are what the user typed (plain text).
	// +optional
	// +kubebuilder:validation:MaxLength=100
	RequesterName string `json:"requesterName,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=254
	Contact string `json:"contact,omitempty"`
	// Reason is the user's explanation (plain text).
	// +optional
	// +kubebuilder:validation:MaxLength=500
	Reason string `json:"reason,omitempty"`
	// Block is why the name was blocked; nil when the block page did not
	// know.
	// +optional
	Block *UnblockBlock `json:"block,omitempty"`
	// RequestedAt is the latest submission (the object's creation is the
	// first); Submissions counts them.
	// +optional
	RequestedAt *metav1.Time `json:"requestedAt,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	Submissions int `json:"submissions,omitempty"`
}

// UnblockAllow is the allow entry an approval created.
type UnblockAllow struct {
	// Scope is everyone (the ui-allow list) or group (ui-allow-<group>).
	// +kubebuilder:validation:Enum=everyone;group
	Scope string `json:"scope"`
	// Match is exact (the name only) or subdomains (the name and below).
	// +kubebuilder:validation:Enum=exact;subdomains
	Match string `json:"match"`
	// Group is the ClientGroup of a group approval, "ns/name".
	// +optional
	// +kubebuilder:validation:MaxLength=300
	Group string `json:"group,omitempty"`
	// List is the allow BlockList, "ns/name", and Entry the line in its
	// inline "ui" source.
	// +kubebuilder:validation:MaxLength=300
	List string `json:"list"`
	// +kubebuilder:validation:MaxLength=300
	Entry string `json:"entry"`
	// Added is true when this approval added Entry (it was not on the
	// list before); only then does expiry remove it.
	// +optional
	Added bool `json:"added,omitempty"`
	// Policies are the policies the list was attached to by this approval.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	Policies []string `json:"policies,omitempty"`
	// ExpiresAt ends a time-limited approval; nil is permanent.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
	// Removed is when the expired entry was taken off the list.
	// +optional
	Removed *metav1.Time `json:"removed,omitempty"`
}

// UnblockDecision is one past decision of a request that was asked again.
type UnblockDecision struct {
	State UnblockRequestState `json:"state"`
	// +optional
	// +kubebuilder:validation:MaxLength=300
	DecidedBy string `json:"decidedBy,omitempty"`
	// +optional
	DecidedAt *metav1.Time `json:"decidedAt,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=500
	Note string `json:"note,omitempty"`
	// +optional
	Allow *UnblockAllow `json:"allow,omitempty"`
}

// UnblockRequestStatus is the decision.
type UnblockRequestStatus struct {
	// State is Pending (also when empty), Approved, Denied or Expired.
	// +optional
	State UnblockRequestState `json:"state,omitempty"`
	// DecidedBy is the administrator ("user:alice") or "system" for
	// expiry; DecidedAt when.
	// +optional
	// +kubebuilder:validation:MaxLength=300
	DecidedBy string `json:"decidedBy,omitempty"`
	// +optional
	DecidedAt *metav1.Time `json:"decidedAt,omitempty"`
	// Note is the administrator's note, shown to the user on the block
	// page.
	// +optional
	// +kubebuilder:validation:MaxLength=500
	Note string `json:"note,omitempty"`
	// Allow is the allow entry of an approval.
	// +optional
	Allow *UnblockAllow `json:"allow,omitempty"`
	// History holds the earlier decisions when the user asked again,
	// oldest first.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	History []UnblockDecision `json:"history,omitempty"`
}

// UnblockRequest is a user's request, made on the block page, to allow a
// blocked name. Administrators approve (which adds an allow entry) or
// deny it; the leader expires and prunes old requests
// (docs/deploy/blocking-page.md, "Unblock requests").
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ubr
// +kubebuilder:printcolumn:name="DOMAIN",type=string,JSONPath=`.spec.domain`
// +kubebuilder:printcolumn:name="CLIENT",type=string,JSONPath=`.spec.client`
// +kubebuilder:printcolumn:name="STATE",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="AGE",type=date,JSONPath=`.metadata.creationTimestamp`
type UnblockRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec UnblockRequestSpec `json:"spec"`
	// +optional
	Status UnblockRequestStatus `json:"status,omitempty"`
}

// EffectiveState is status.state with empty read as Pending.
func (r *UnblockRequest) EffectiveState() UnblockRequestState {
	if r.Status.State == "" {
		return UnblockPending
	}
	return r.Status.State
}

// UnblockRequestList contains a list of UnblockRequest.
// +kubebuilder:object:root=true
type UnblockRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UnblockRequest `json:"items"`
}
