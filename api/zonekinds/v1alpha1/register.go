package v1alpha1

import (
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SchemeBuilder registers private committed-zone kinds without public CRDs.
var SchemeBuilder = runtime.NewSchemeBuilder(func(s *runtime.Scheme) error {
	s.AddKnownTypes(public.GroupVersion, &ZoneState{}, &ZoneStateList{}, &ZoneChunk{}, &ZoneChunkList{}, &ZoneLifecycle{}, &ZoneLifecycleList{})
	metav1.AddToGroupVersion(s, public.GroupVersion)
	return nil
})

// AddToScheme adds private committed-zone kinds to a store's runtime scheme.
var AddToScheme = SchemeBuilder.AddToScheme

// DeepCopy returns a detached copy. Copies are hand-maintained; these kinds have no CRDs or
// generated public schemas. Every mutable metadata/slice field is copied.
func (s *ZoneState) DeepCopy() *ZoneState {
	if s == nil {
		return nil
	}
	out := *s
	out.ObjectMeta = *s.ObjectMeta.DeepCopy()
	return &out
}

// DeepCopyObject implements runtime.Object.
func (s *ZoneState) DeepCopyObject() runtime.Object {
	if s == nil {
		return nil
	}
	return s.DeepCopy()
}

// DeepCopyObject implements runtime.Object.
func (s *ZoneStateList) DeepCopyObject() runtime.Object {
	if s == nil {
		return nil
	}
	out := *s
	s.DeepCopyInto(&out.ListMeta)
	out.Items = make([]ZoneState, len(s.Items))
	for i := range s.Items {
		out.Items[i] = *s.Items[i].DeepCopy()
	}
	return &out
}

// DeepCopy returns a detached copy, including the chunk's bytes.
func (s *ZoneChunk) DeepCopy() *ZoneChunk {
	if s == nil {
		return nil
	}
	out := *s
	out.ObjectMeta = *s.ObjectMeta.DeepCopy()
	out.Spec.Data = append([]byte(nil), s.Spec.Data...)
	return &out
}

// DeepCopyObject implements runtime.Object.
func (s *ZoneChunk) DeepCopyObject() runtime.Object {
	if s == nil {
		return nil
	}
	return s.DeepCopy()
}

// DeepCopyObject implements runtime.Object.
func (s *ZoneChunkList) DeepCopyObject() runtime.Object {
	if s == nil {
		return nil
	}
	out := *s
	s.DeepCopyInto(&out.ListMeta)
	out.Items = make([]ZoneChunk, len(s.Items))
	for i := range s.Items {
		out.Items[i] = *s.Items[i].DeepCopy()
	}
	return &out
}
