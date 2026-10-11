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
)
