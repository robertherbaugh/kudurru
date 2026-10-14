package v1alpha1

import (
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

var SchemeBuilder = runtime.NewSchemeBuilder(func(s *runtime.Scheme) error {
	s.AddKnownTypes(public.GroupVersion, &ZoneState{}, &ZoneStateList{}, &ZoneChunk{}, &ZoneChunkList{}, &ZoneLifecycle{}, &ZoneLifecycleList{})
	metav1.AddToGroupVersion(s, public.GroupVersion)
	return nil
})
var AddToScheme = SchemeBuilder.AddToScheme

// Deep copies are deliberately hand-maintained; these kinds have no CRDs or
// generated public schemas. Every mutable metadata/slice field is copied.
func (s *ZoneState) DeepCopy() *ZoneState {
	if s == nil {
		return nil
	}
	out := *s
	out.ObjectMeta = *s.ObjectMeta.DeepCopy()
	return &out
}
func (s *ZoneState) DeepCopyObject() runtime.Object {
	if s == nil {
		return nil
	}
	return s.DeepCopy()
}
func (s *ZoneStateList) DeepCopyObject() runtime.Object {
	if s == nil {
		return nil
	}
	out := *s
	s.ListMeta.DeepCopyInto(&out.ListMeta)
	out.Items = make([]ZoneState, len(s.Items))
	for i := range s.Items {
		out.Items[i] = *s.Items[i].DeepCopy()
	}
	return &out
}
func (s *ZoneChunk) DeepCopy() *ZoneChunk {
	if s == nil {
		return nil
	}
	out := *s
	out.ObjectMeta = *s.ObjectMeta.DeepCopy()
	out.Spec.Data = append([]byte(nil), s.Spec.Data...)
	return &out
}
func (s *ZoneChunk) DeepCopyObject() runtime.Object {
	if s == nil {
		return nil
	}
	return s.DeepCopy()
}
func (s *ZoneChunkList) DeepCopyObject() runtime.Object {
	if s == nil {
		return nil
	}
	out := *s
	s.ListMeta.DeepCopyInto(&out.ListMeta)
	out.Items = make([]ZoneChunk, len(s.Items))
	for i := range s.Items {
		out.Items[i] = *s.Items[i].DeepCopy()
	}
	return &out
}
