/*
Copyright 2026 Robert Herbaugh.

SPDX-License-Identifier: LicenseRef-Kudurru-Community
See the LICENSE file in the repository root.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Labels and annotations of AuditLog objects. The reader selects and
// orders batches by them without reading their events.
const (
	// LabelNode is the node that recorded the batch.
	LabelNode = "audit.kudurru.io/node"
	// LabelSource is the audit spool the batch came from.
	LabelSource = "audit.kudurru.io/source"
	// AnnotationFrom and AnnotationUntil are the times of the first and
	// the last event (RFC 3339, microseconds).
	AnnotationFrom  = "audit.kudurru.io/from"
	AnnotationUntil = "audit.kudurru.io/until"
	// AnnotationLastID is the spool id of the batch's last event.
	AnnotationLastID = "audit.kudurru.io/last-id"
	// AnnotationKinds and AnnotationActors list the resource kinds and
	// actors (Actor.String) of the batch's events, comma-separated; absent
	// when the list would be too long, so a filter must read the events.
	AnnotationKinds  = "audit.kudurru.io/kinds"
	AnnotationActors = "audit.kudurru.io/actors"
)

// AuditLog is an immutable batch of audit events recorded by one kudurru
// node: the events of one audit spool (Source) with ids FirstID to LastID,
// hash-chained to the batch before it. kudurru only creates and deletes
// AuditLogs (its RBAC grants no update), the leader deletes them after
// audit.retention, and GET /api/v1/audit reads them on every replica.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories=kudurru-audit
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.node`
// +kubebuilder:printcolumn:name="Events",type=integer,JSONPath=`.spec.count`
// +kubebuilder:printcolumn:name="From",type=string,JSONPath=`.spec.from`
// +kubebuilder:printcolumn:name="Until",type=string,JSONPath=`.spec.until`
type AuditLog struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec AuditLogSpec `json:"spec"`
}

// AuditLogSpec is the batch.
type AuditLogSpec struct {
	// Node is the node that recorded the events.
	// +kubebuilder:validation:MinLength=1
	Node string `json:"node"`
	// Source is the UUID of the node's audit spool; events are keyed by
	// (source, id).
	// +kubebuilder:validation:MinLength=1
	Source string `json:"source"`
	// FirstID and LastID are the spool ids of the first and last event.
	FirstID int64 `json:"firstID"`
	LastID  int64 `json:"lastID"`
	// Count is the number of events.
	Count int `json:"count"`
	// From and Until are the times of the first and last event.
	From  metav1.MicroTime `json:"from"`
	Until metav1.MicroTime `json:"until"`
	// PrevHash is the hash the first event follows (the last event of the
	// spool before this batch); Hash is the last event's hash.
	PrevHash string `json:"prevHash,omitempty"`
	Hash     string `json:"hash"`
	// Seal is an HMAC-SHA256 over the batch keyed from the cluster
	// encryption key, so an edit made without that key is detected.
	Seal string `json:"seal,omitempty"`
	// Stubs counts the denied or failed events stored without documents
	// and request data because the node exceeded its hourly budget for
	// them (successful actions are always stored in full).
	// +optional
	Stubs int `json:"stubs,omitempty"`
	// Events are the events, oldest first.
	// +kubebuilder:validation:MaxItems=1000
	Events []AuditLogEvent `json:"events"`
}

// AuditLogEvent is one audit event (docs/architecture.md §11.4).
type AuditLogEvent struct {
	// ID is the event's id in the node's audit spool.
	ID int64 `json:"id"`
	// Time is when the action happened.
	Time metav1.MicroTime `json:"ts"`
	// Node is set when it differs from spec.node.
	// +optional
	Node string `json:"node,omitempty"`
	// Actor is who acted.
	Actor AuditActor `json:"actor"`
	// Action is "<resource>.<verb>", e.g. "dnszone.update".
	Action string `json:"action"`
	// Resource is the object or virtual resource acted upon.
	Resource AuditResource `json:"resource"`
	// Result is success, denied or error.
	Result string `json:"result"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// Authz is the authorizer path that decided (binding, sar, local).
	// +optional
	Authz string `json:"authz,omitempty"`
	// Manager is the store manager of a store write ("api", "file:...").
	// +optional
	Manager string `json:"manager,omitempty"`
	// +optional
	ResourceVersion string `json:"resourceVersion,omitempty"`
	// Before and After are the redacted states around the action, as JSON
	// text (kept byte for byte: they are hashed).
	// +optional
	Before string `json:"before,omitempty"`
	// +optional
	After string `json:"after,omitempty"`
	// Request is the HTTP context of the action.
	// +optional
	Request AuditRequest `json:"request,omitzero"`
	// Stub is set when the documents, the reason and the request were
	// dropped (hourly budget); Hash then cannot be recomputed and only
	// the link to the previous event is verified.
	// +optional
	Stub bool `json:"stub,omitempty"`
	// PrevHash and Hash chain the node's events.
	PrevHash string `json:"prevHash,omitempty"`
	Hash     string `json:"hash"`
}

// AuditActor is who acted.
type AuditActor struct {
	// +optional
	Kind string `json:"kind,omitempty"`
	// +optional
	ID string `json:"id,omitempty"`
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	Groups []string `json:"groups,omitempty"`
	// Auth is how the actor authenticated (cookie, token, jwt, k8s, unix, internal).
	// +optional
	Auth string `json:"auth,omitempty"`
}

// AuditResource names the object or virtual resource acted upon.
type AuditResource struct {
	Kind string `json:"kind"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	Name string `json:"name,omitempty"`
}

// AuditRequest is the HTTP context of an action.
type AuditRequest struct {
	// +optional
	ID string `json:"id,omitempty"`
	// +optional
	IP string `json:"ip,omitempty"`
	// +optional
	UA string `json:"ua,omitempty"`
	// +optional
	Method string `json:"method,omitempty"`
	// +optional
	Path string `json:"path,omitempty"`
}

// AuditLogList is a list of AuditLog batches.
//
// +kubebuilder:object:root=true
type AuditLogList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AuditLog `json:"items"`
}

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &AuditLog{}, &AuditLogList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
