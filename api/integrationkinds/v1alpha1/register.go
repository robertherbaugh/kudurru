package v1alpha1

import (
	"slices"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SchemeBuilder registers the private runtime objects and lists.
var SchemeBuilder = runtime.NewSchemeBuilder(func(s *runtime.Scheme) error {
	s.AddKnownTypes(public.GroupVersion, &IntegrationLedger{}, &IntegrationLedgerList{}, &IntegrationOperation{}, &IntegrationOperationList{}, &IntegrationBudget{}, &IntegrationBudgetList{}, &IntegrationDelivery{}, &IntegrationDeliveryList{})
	metav1.AddToGroupVersion(s, public.GroupVersion)
	return nil
})

// AddToScheme adds private Integration runtime types without public CRDs.
var AddToScheme = SchemeBuilder.AddToScheme

// DeepCopy returns isolated ledger state, including the pending reservation.
// These private runtime kinds use hand-maintained copies, like zonekinds;
// no public generator or CRD files are involved.
func (in *IntegrationLedger) DeepCopy() *IntegrationLedger {
	if in == nil {
		return nil
	}
	out := *in
	out.ObjectMeta = *in.ObjectMeta.DeepCopy()
	out.Spec.ProviderIDs = slices.Clone(in.Spec.ProviderIDs)
	if in.Spec.Pending != nil {
		p := *in.Spec.Pending
		out.Spec.Pending = &p
	}
	return &out
}

// DeepCopyObject returns an isolated IntegrationLedger as a runtime object.
func (in *IntegrationLedger) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}

// DeepCopyObject returns an isolated IntegrationLedgerList as a runtime object.
func (in *IntegrationLedgerList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := *in
	in.DeepCopyInto(&out.ListMeta)
	out.Items = make([]IntegrationLedger, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *in.Items[i].DeepCopy()
	}
	return &out
}

// DeepCopy returns an isolated IntegrationOperation.
func (in *IntegrationOperation) DeepCopy() *IntegrationOperation {
	if in == nil {
		return nil
	}
	out := *in
	out.ObjectMeta = *in.ObjectMeta.DeepCopy()
	out.Spec.Before = in.Spec.Before.DeepCopy()
	out.Spec.After = in.Spec.After.DeepCopy()
	out.Spec.ProviderIDs = slices.Clone(in.Spec.ProviderIDs)
	out.Spec.RemoteVersions = slices.Clone(in.Spec.RemoteVersions)
	if in.Spec.Receipt != nil {
		r := *in.Spec.Receipt
		r.ProviderIDs = slices.Clone(r.ProviderIDs)
		r.Targets = slices.Clone(r.Targets)
		out.Spec.Receipt = &r
	}
	return &out
}

// DeepCopyObject returns an isolated IntegrationOperation as a runtime object.
func (in *IntegrationOperation) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}

// DeepCopyObject returns an isolated IntegrationOperationList as a runtime object.
func (in *IntegrationOperationList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := *in
	in.DeepCopyInto(&out.ListMeta)
	out.Items = make([]IntegrationOperation, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *in.Items[i].DeepCopy()
	}
	return &out
}

// DeepCopy returns an isolated IntegrationBudget.
func (in *IntegrationBudget) DeepCopy() *IntegrationBudget {
	if in == nil {
		return nil
	}
	out := *in
	out.ObjectMeta = *in.ObjectMeta.DeepCopy()
	out.Spec.Reservations = slices.Clone(in.Spec.Reservations)
	return &out
}

// DeepCopyObject returns an isolated IntegrationBudget as a runtime object.
func (in *IntegrationBudget) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}

// DeepCopyObject returns an isolated IntegrationBudgetList as a runtime object.
func (in *IntegrationBudgetList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := *in
	in.DeepCopyInto(&out.ListMeta)
	out.Items = make([]IntegrationBudget, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *in.Items[i].DeepCopy()
	}
	return &out
}

// DeepCopy returns an isolated IntegrationDelivery.
func (in *IntegrationDelivery) DeepCopy() *IntegrationDelivery {
	if in == nil {
		return nil
	}
	out := *in
	out.ObjectMeta = *in.ObjectMeta.DeepCopy()
	out.Spec.Payload = slices.Clone(in.Spec.Payload)
	return &out
}

// DeepCopyObject returns an isolated IntegrationDelivery as a runtime object.
func (in *IntegrationDelivery) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}

// DeepCopyObject returns an isolated IntegrationDeliveryList as a runtime object.
func (in *IntegrationDeliveryList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := *in
	in.DeepCopyInto(&out.ListMeta)
	out.Items = make([]IntegrationDelivery, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *in.Items[i].DeepCopy()
	}
	return &out
}
