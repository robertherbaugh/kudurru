package v1alpha1

import (
	"slices"

	"k8s.io/apimachinery/pkg/runtime"
)

// DeepCopy returns an isolated private carrier.
func (in *IntegrationSourceHead) DeepCopy() *IntegrationSourceHead {
	if in == nil {
		return nil
	}
	out := *in
	out.ObjectMeta = *in.ObjectMeta.DeepCopy()
	out.Spec.Chunks = slices.Clone(in.Spec.Chunks)
	return &out
}

// DeepCopyObject implements runtime.Object.
func (in *IntegrationSourceHead) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}

// DeepCopyObject implements runtime.Object for the private carrier list.
func (in *IntegrationSourceHeadList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := *in
	in.DeepCopyInto(&out.ListMeta)
	out.Items = make([]IntegrationSourceHead, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *in.Items[i].DeepCopy()
	}
	return &out
}

// DeepCopy returns an isolated private carrier.
func (in *IntegrationSourceChunk) DeepCopy() *IntegrationSourceChunk {
	if in == nil {
		return nil
	}
	out := *in
	out.ObjectMeta = *in.ObjectMeta.DeepCopy()
	out.Spec.Data = slices.Clone(in.Spec.Data)
	return &out
}

// DeepCopyObject implements runtime.Object.
func (in *IntegrationSourceChunk) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}

// DeepCopyObject implements runtime.Object for the private carrier list.
func (in *IntegrationSourceChunkList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := *in
	in.DeepCopyInto(&out.ListMeta)
	out.Items = make([]IntegrationSourceChunk, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *in.Items[i].DeepCopy()
	}
	return &out
}

// DeepCopy returns an isolated private carrier.
func (in *IntegrationNameAllocation) DeepCopy() *IntegrationNameAllocation {
	if in == nil {
		return nil
	}
	out := *in
	out.ObjectMeta = *in.ObjectMeta.DeepCopy()
	return &out
}

// DeepCopyObject implements runtime.Object.
func (in *IntegrationNameAllocation) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}

// DeepCopyObject implements runtime.Object for the private carrier list.
func (in *IntegrationNameAllocationList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := *in
	in.DeepCopyInto(&out.ListMeta)
	out.Items = make([]IntegrationNameAllocation, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *in.Items[i].DeepCopy()
	}
	return &out
}
