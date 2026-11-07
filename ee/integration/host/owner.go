package host

// OwnerHost supplies the immutable ownership identity of a provisioned job.
// It is metadata, never an alternate credential or live object lookup.
type OwnerHost interface {
	RequestHost
	IntegrationOwner() InstanceID
}

// IntegrationOwner retains the original incarnation even after cancellation or
// replacement under the same name. Guard.Owner is fixed for the job's lifetime;
// refresh changes only its RV, and lease reacquisition preserves that Owner.
func (p providerHost) IntegrationOwner() InstanceID {
	p.h.mu.RLock()
	defer p.h.mu.RUnlock()
	return p.j.guard.Owner
}
