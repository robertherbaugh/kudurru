package v1alpha1

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EnabledAnnotation switches a BlockList, ClientGroup or Policy off without
// deleting it: "false" disables the object, any other value or no
// annotation leaves it enabled. The policy index and the list resolver
// treat a disabled object as absent; the controller reports it with a
// Disabled condition. An annotation rather than a spec field so that the
// v1alpha1 schema stays stable and kubectl annotate works (decision in
// docs/m5/burndown-server.md); the dashboard writes the same annotation.
const EnabledAnnotation = "kudurru.io/enabled"

// CommentAnnotation is a free-text note the dashboard shows next to the
// object.
const CommentAnnotation = "kudurru.io/comment"

// ConditionDisabled is True while EnabledAnnotation is "false".
const ConditionDisabled = "Disabled"

// ReasonDisabled is the reason of the Disabled condition and of the False
// Ready condition of a disabled object.
const ReasonDisabled = "Disabled"

// IsDisabled reports whether obj carries kudurru.io/enabled: "false"
// (case-insensitive, surrounding blanks ignored).
func IsDisabled(obj metav1.Object) bool {
	if obj == nil {
		return false
	}
	v, ok := obj.GetAnnotations()[EnabledAnnotation]
	return ok && strings.EqualFold(strings.TrimSpace(v), "false")
}
