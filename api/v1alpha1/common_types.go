package v1alpha1

// ObjectRef points at another kudurru object. Namespace is omitted for
// cluster-scoped kinds and for same-namespace references.
type ObjectRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// ConditionType values shared by every kind. Kinds add their own.
const (
	ConditionReady    = "Ready"
	ConditionDegraded = "Degraded"
)

// Condition reasons used by the reconcilers.
const (
	ReasonServing      = "Serving"
	ReasonSyncing      = "Syncing"
	ReasonInvalid      = "Invalid"
	ReasonOutsideZone  = "OutsideZone"
	ReasonDuplicated   = "Duplicated"
	ReasonFetchFailed  = "ListFetchFailed"
	ReasonFromSnapshot = "ServingFromSnapshot"
	// ReasonAsExpected is the reason of a False Degraded condition.
	ReasonAsExpected = "AsExpected"
	// ReasonZoneNotFound: a DNSRecordSet's zoneRef names no DNSZone.
	ReasonZoneNotFound = "ZoneNotFound"
	// ReasonNotSelected: the zone's recordSetSelector does not admit the
	// record set.
	ReasonNotSelected = "NotSelected"
	// ReasonMissingReference: a Policy names a ClientGroup or BlockList
	// that does not exist.
	ReasonMissingReference = "MissingReference"
	// ReasonUnknownService: a Policy's blockedServices names a service the
	// blocked-services catalogue in service does not have.
	ReasonUnknownService = "UnknownService"
	// ReasonUnknownCategory: a Policy's blockedCategories names a category
	// the catalogue in service does not have.
	ReasonUnknownCategory = "UnknownCategory"
	// ReasonUnsupported: the zone type is not served by this version.
	ReasonUnsupported = "Unsupported"
	// ReasonOccluded: records below a DNAME owner, which no lookup can
	// reach (RFC 6672 §2.4), were left out of the zone.
	ReasonOccluded = "Occluded"
)
