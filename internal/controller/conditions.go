package controller

import (
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// Condition reasons the controller uses besides the generic ones; they
// live in api/v1alpha1 and are aliased here for the existing callers.
const (
	// ReasonAsExpected is the reason of a False Degraded condition.
	ReasonAsExpected = v1alpha1.ReasonAsExpected
	// ReasonZoneNotFound: a DNSRecordSet's zoneRef names no DNSZone.
	ReasonZoneNotFound = v1alpha1.ReasonZoneNotFound
	// ReasonNotSelected: the zone's recordSetSelector does not admit the
	// record set.
	ReasonNotSelected = v1alpha1.ReasonNotSelected
	// ReasonMissingReference: a Policy names a ClientGroup or BlockList
	// that does not exist.
	ReasonMissingReference = v1alpha1.ReasonMissingReference
	// ReasonUnsupported: the zone type is not served by this version.
	ReasonUnsupported = v1alpha1.ReasonUnsupported
)

// verdict is the desired Ready and Degraded pair for one object.
type verdict struct {
	Ready         bool
	ReadyReason   string
	ReadyMessage  string
	Degraded      bool
	DegradedRsn   string
	DegradedMsg   string
	observedGen   int64
	transitionNow time.Time
}

// apply sets Ready and Degraded on conds. meta.SetStatusCondition keeps
// lastTransitionTime when the status does not change, so re-applying the
// same verdict leaves conds deep-equal (status write suppression relies on
// this).
func (v verdict) apply(conds *[]metav1.Condition) {
	now := metav1.NewTime(v.transitionNow.Truncate(time.Second))
	ready := metav1.Condition{
		Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse,
		Reason: v.ReadyReason, Message: v.ReadyMessage,
		ObservedGeneration: v.observedGen, LastTransitionTime: now,
	}
	if v.Ready {
		ready.Status = metav1.ConditionTrue
	}
	degraded := metav1.Condition{
		Type: v1alpha1.ConditionDegraded, Status: metav1.ConditionFalse,
		Reason: ReasonAsExpected, Message: "",
		ObservedGeneration: v.observedGen, LastTransitionTime: now,
	}
	if v.Degraded {
		degraded.Status = metav1.ConditionTrue
		degraded.Reason = v.DegradedRsn
		degraded.Message = v.DegradedMsg
	}
	meta.SetStatusCondition(conds, ready)
	meta.SetStatusCondition(conds, degraded)
}

// serving is the verdict of a healthy object.
func serving(msg string) verdict {
	return verdict{Ready: true, ReadyReason: v1alpha1.ReasonServing, ReadyMessage: msg}
}

// condition returns the condition of type t, or nil.
func condition(conds []metav1.Condition, t string) *metav1.Condition {
	return meta.FindStatusCondition(conds, t)
}
