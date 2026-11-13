package zone

import (
	"strings"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// External zones (D402). A DNSZone of type external is built like a primary
// zone, but only from the record sets the Kubernetes source adapter
// (internal/sources/k8s) derives from Ingresses, Gateway API routes and
// LoadBalancer Services. The adapter writes each record set with the
// manager ExternalSourceManager(uid) of its source object and records the
// source identity in the annotations below. Any other record set that
// names an external zone (an API, file or kubectl write) is not selected:
// the zone serves what its sources say and nothing else.
const (
	// ExternalSourceManagerPrefix starts the metadata.managedBy value of
	// every record set the source adapter maintains.
	ExternalSourceManagerPrefix = "kubernetes-source:"
	// ExternalSourceUIDAnnotation is the UID of the source object.
	ExternalSourceUIDAnnotation = "kudurru.io/external-source-uid"
	// ExternalSourceNameAnnotation is the name of the source object.
	ExternalSourceNameAnnotation = "kudurru.io/external-source-name"
	// ExternalSourceVersionAnnotation is the resourceVersion of the source
	// object the record set was derived from.
	ExternalSourceVersionAnnotation = "kudurru.io/external-source-version"
	// ExternalSourceLabel marks the derived record sets with the source kind
	// (ingress, httproute, grpcroute, tlsroute, service), so the adapter can
	// list them by label.
	ExternalSourceLabel = "kudurru.io/external-source"
)

// ExternalSourceManager is the manager of the record sets derived from the
// source object with uid.
func ExternalSourceManager(uid string) string { return ExternalSourceManagerPrefix + uid }

// ExternalSourceOwned reports whether rs carries the complete identity of a
// record set the source adapter maintains: the source label, a source UID
// and name, the manager of that UID and exactly one owner reference to that
// object. The managedBy annotation is server-owned on every API path (the
// API strips it from documents), so an API or file write never selects
// itself into an external zone.
func ExternalSourceOwned(rs *v1alpha1.DNSRecordSet) bool {
	if rs == nil {
		return false
	}
	uid, name := rs.Annotations[ExternalSourceUIDAnnotation], rs.Annotations[ExternalSourceNameAnnotation]
	if uid == "" || name == "" || rs.Labels[ExternalSourceLabel] == "" || rs.Annotations[v1alpha1.ManagedByAnnotation] != ExternalSourceManager(uid) {
		return false
	}
	if len(rs.OwnerReferences) != 1 {
		return false
	}
	owner := rs.OwnerReferences[0]
	return string(owner.UID) == uid && owner.Name == name && strings.EqualFold(owner.Kind, externalKindOf(rs.Labels[ExternalSourceLabel]))
}

// externalKindOf maps the source label value to the owner reference kind.
func externalKindOf(label string) string {
	switch label {
	case "ingress":
		return "Ingress"
	case "httproute":
		return "HTTPRoute"
	case "grpcroute":
		return "GRPCRoute"
	case "tlsroute":
		return "TLSRoute"
	case "service":
		return "Service"
	}
	return "\x00"
}

// externalSelected applies the external-zone rule on top of the zone's
// recordSetSelector: only the adapter's record sets build an external zone.
func externalSelected(dz *v1alpha1.DNSZone, rs *v1alpha1.DNSRecordSet) bool {
	return dz.Spec.Type != v1alpha1.ZoneTypeExternal || ExternalSourceOwned(rs)
}
