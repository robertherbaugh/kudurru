package servingbuild

import (
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
)

// TSIGManagement borrows immutable original provisioning inputs from one complete
// capture. Its lifetime excludes intentional stored-ring replacement only for
// postcommit observation. It grants no current command or transaction authority.
// The containing serving lease must remain retained through every method call.
type TSIGManagement struct {
	prepared *tsigkeys.Prepared
	sealer   *tsigkeys.Sealer
	current  func() bool
}

// NewTSIGManagement binds the exact original Prepared and encryption cut. The
// supplied predicate is memory-only original host/encryption lifetime, not live
// Store acquisition and not the old ring's installation-currency predicate.
func NewTSIGManagement(prepared *tsigkeys.Prepared, sealer *tsigkeys.Sealer, current func() bool) (*TSIGManagement, error) {
	if prepared == nil || sealer == nil || current == nil || !current() || prepared.SourceIdentity() == "" || prepared.EncryptionIdentity() != sealer.Identity() {
		return nil, serving.ErrDependency
	}
	return &TSIGManagement{prepared: prepared, sealer: sealer, current: current}, nil
}

// BindLifetime returns a distinct immutable binding to the same original input.
// Root adds the original retained provider capture; it must not capture a newer
// provider or replace the complete dependency's ordinary Current predicate.
func (m *TSIGManagement) BindLifetime(current func() bool) *TSIGManagement {
	if m == nil || m.current == nil || current == nil {
		return nil
	}
	original := m.current
	return &TSIGManagement{prepared: m.prepared, sealer: m.sealer, current: func() bool { return original() && current() }}
}

// TSIGMetadata returns detached metadata from the original complete cut only.
func (r *RuntimeOwner) TSIGMetadata() ([]tsigkeys.Metadata, error) {
	if r == nil || r.tsig == nil || r.tsig.prepared == nil || r.current == nil || !r.current() || !r.TSIGOriginalCurrent() {
		return nil, serving.ErrDependency
	}
	return r.tsig.prepared.Metadata()
}

// PlanTSIGChange plans against original immutable inputs before command gates.
// The caller still must reauthorize raw proof and use the actual CommandTx.
func (r *RuntimeOwner) PlanTSIGChange(input tsigkeys.MutationInput) (*tsigkeys.Mutation, error) {
	if r == nil || r.tsig == nil || r.tsig.prepared == nil || r.current == nil || !r.current() || !r.TSIGOriginalCurrent() {
		return nil, serving.ErrDependency
	}
	return r.tsig.prepared.Plan(r.tsig.sealer, input)
}

// TSIGOriginalCurrent checks retained host/encryption/provider lifetime only.
// Use after confirmed commit to observe the predetermined target; it cannot
// admit an old source to a command, DNS effect or acquisition persistence.
func (r *RuntimeOwner) TSIGOriginalCurrent() bool {
	return r != nil && r.tsig != nil && r.tsig.current != nil && r.tsig.current()
}

// TSIGEncryptionIdentity is internal target correlation, never API metadata.
func (r *RuntimeOwner) TSIGEncryptionIdentity() string {
	if r == nil || r.tsig == nil || r.tsig.prepared == nil {
		return ""
	}
	return r.tsig.prepared.EncryptionIdentity()
}

// MatchesTSIGTarget compares a complete observed owner's exact durable source
// and encryption identity. Publisher.CheckWriterLease remains mandatory after
// inspection; this comparison grants no continuing authority or exclusion.
func (r *RuntimeOwner) MatchesTSIGTarget(source, encryption string) bool {
	return r != nil && r.tsig != nil && r.tsig.prepared != nil && source != "" && encryption != "" && r.tsig.prepared.SourceIdentity() == source && r.tsig.prepared.EncryptionIdentity() == encryption
}
