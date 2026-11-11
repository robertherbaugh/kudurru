package v1alpha1

import (
	"fmt"
	"maps"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// KindIntegrationEventCursor is the private event production cursor of one
// hook instance (D379).
const KindIntegrationEventCursor = "IntegrationEventCursor"

// MaxCursorPositions bounds the positions of one cursor.
const MaxCursorPositions = 16

// IntegrationEventCursorSpec records how far root's event production for
// one Integration incarnation has read. Positions are opaque to the host:
// root names them (a store change-feed cursor per watched kind, and the
// level digests it compares to detect a transition). The cursor grants no
// authority and carries no event content.
type IntegrationEventCursorSpec struct {
	FormatVersion int               `json:"formatVersion"`
	Owner         Owner             `json:"owner"`
	Positions     map[string]string `json:"positions"`
	UpdatedAt     time.Time         `json:"updatedAt"`
}

// IntegrationEventCursor is one durable cursor per hook instance.
type IntegrationEventCursor struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              IntegrationEventCursorSpec `json:"spec"`
}

// IntegrationEventCursorList is a private cursor carrier list.
type IntegrationEventCursorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IntegrationEventCursor `json:"items"`
}

// EventCursorName binds the one cursor of an Integration incarnation.
func EventCursorName(o Owner) string { return objectName("iec-", o) }

// Validate rejects malformed or unbounded IntegrationEventCursor state.
func (in *IntegrationEventCursor) Validate() error {
	if in == nil {
		return fmt.Errorf("nil integration event cursor")
	}
	p := in.Spec
	if err := objectValid(in, p.FormatVersion, in.Name, in.Namespace, EventCursorName(p.Owner)); err != nil {
		return err
	}
	if err := p.Owner.Validate(); err != nil {
		return err
	}
	if len(p.Positions) > MaxCursorPositions || p.UpdatedAt.IsZero() {
		return fmt.Errorf("invalid integration event cursor")
	}
	for k, v := range p.Positions {
		if !bounded(k, 64) || len(v) > 256 {
			return fmt.Errorf("invalid integration event cursor position")
		}
	}
	return nil
}

// DeepCopy returns an isolated IntegrationEventCursor.
func (in *IntegrationEventCursor) DeepCopy() *IntegrationEventCursor {
	if in == nil {
		return nil
	}
	out := *in
	out.ObjectMeta = *in.ObjectMeta.DeepCopy()
	out.Spec.Positions = maps.Clone(in.Spec.Positions)
	return &out
}

// DeepCopyObject returns an isolated IntegrationEventCursor as a runtime object.
func (in *IntegrationEventCursor) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}

// DeepCopyObject returns an isolated IntegrationEventCursorList.
func (in *IntegrationEventCursorList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := *in
	in.DeepCopyInto(&out.ListMeta)
	out.Items = make([]IntegrationEventCursor, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *in.Items[i].DeepCopy()
	}
	return &out
}

func addCursorToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(public.GroupVersion, &IntegrationEventCursor{}, &IntegrationEventCursorList{})
	return nil
}
