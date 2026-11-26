package serving

import (
	"context"
	"errors"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// Status distinguishes actual serving identity from installed data. Installed
// is a partial primary cursor until the matching complete candidate was read;
// InstalledComplete explicitly reports whether its digest is known.
type Status struct {
	Authority         replication.Authority
	Installed         replication.Generation
	InstalledComplete bool
	Served            Identity
	Preparing         bool
	DNSOnly           bool
	ProtectedReady    bool
	Reason            string
	// Wakes counts the Publisher's Wake calls so far. ServedWakes is the
	// count the served publication's preparation noted before it read its
	// candidate: that publication includes what each of those Wakes
	// announced. A Wake counted in Wakes and not in ServedWakes is still
	// answered by a later preparation, which publishes the same cut again
	// when nothing else changed (D405). Wakes is read before the served
	// publication, so ServedWakes may exceed it.
	Wakes       uint64
	ServedWakes uint64
}

// Status reads local atomics/bookkeeping, never SQLite or owner private values.
func (p *Publisher) Status() Status {
	// A dependency cursor may reference owner state; retain it while checking
	// eligibility so a concurrent replacement cannot retire its resources.
	wakes := p.wakes.Load()
	lease, _ := p.Capture()
	defer lease.Release()
	var bundle *Bundle
	if lease != nil {
		bundle = lease.bundle
	}
	a, blocked := p.opts.Source.ServingAuthority()
	p.mu.Lock()
	token, preparing, reason := p.installed, p.preparing, p.reason
	p.mu.Unlock()
	s := Status{Authority: a, Installed: a.Installed, Preparing: preparing, Reason: reason, Wakes: wakes}
	if token.Matches(a) {
		s.Installed = token.Generation()
	}
	s.InstalledComplete = s.Installed.Validate() == nil
	if bundle != nil {
		s.Served, s.ServedWakes = bundle.identity, bundle.wakes
		s.DNSOnly = bundle.dnsOnly || blocked || a.Mode == replication.Fenced || a.Mode == replication.Conflict || !a.Term.Same(bundle.identity.Base.Term)
		s.ProtectedReady = !p.closed.Load() && eligible(bundle, a, blocked)
		if !s.ProtectedReady && !s.DNSOnly && reason == "current" {
			s.Reason = "serving_generation_pending"
		}
	}
	return s
}

// Subscribe signals publisher bookkeeping/publication changes. Subscribe before
// Status/WaitTerm checks; also subscribe to Source.AuthorityChanges for immediate
// authority-only gates. The preparation loop propagates those changes here.
func (p *Publisher) Subscribe() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.changed
}

// AuthorityChanges subscribes to the source's authority cursor. Background
// acknowledgment waiters subscribe before capturing a lease, so a fenced writer
// can stop even while preparation is blocked. This is not a query-path method.
func (p *Publisher) AuthorityChanges() <-chan struct{} { return p.opts.Source.AuthorityChanges() }

// CheckWriterLease checks that an owned lease is still the current complete,
// eligible publication in the exact caller's writable incarnation. It reads
// only memory and confers no transaction authority or continuing exclusion.
// Call again after examining owner values, before acknowledging a publication.
func (p *Publisher) CheckWriterLease(ctx context.Context, lease *Lease, target TermActivation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if target.Term.Validate() != nil || target.Incarnation == 0 {
		return replication.ErrInvalid
	}
	if p.closed.Load() {
		return ErrClosed
	}
	a, blocked := p.opts.Source.ServingAuthority()
	if !a.Term.Same(target.Term) || a.Incarnation != target.Incarnation || !a.Writable() || blocked {
		return ErrSuperseded
	}
	bundle := lease.Bundle()
	if bundle == nil || bundle != p.current.Load() || !eligible(bundle, a, blocked) {
		return ErrPending
	}
	return nil
}

// CheckWriterCut is CheckWriterLease for a current HTTP command (D399): it
// also admits a lease whose publication was replaced by a publication of the
// same cut, that is the same installed generation (term, revision and
// digest) with the same dependency identity, while the lease's own
// dependency cursors are still current. Such a publication is the same
// source prepared again after a Wake that arrived while its first
// preparation ran (D405); every input of the publication is the one the
// lease holds, so refusing the command as superseded protected nothing. A
// new generation, an authority change, a changed dependency identity, a
// lease whose dependencies were replaced and a DNS-only publication refuse
// exactly as in CheckWriterLease. Background acknowledgments and the other
// callers of CheckWriterLease keep the exact publication rule.
func (p *Publisher) CheckWriterCut(ctx context.Context, lease *Lease, target TermActivation) error {
	err := p.CheckWriterLease(ctx, lease, target)
	if !errors.Is(err, ErrPending) {
		return err
	}
	bundle, current := lease.Bundle(), p.current.Load()
	if bundle == nil || current == nil || current == bundle || current.dnsOnly ||
		!current.identity.Base.Same(bundle.identity.Base) || current.identity.Dependencies != bundle.identity.Dependencies {
		return err
	}
	a, blocked := p.opts.Source.ServingAuthority()
	if p.closed.Load() || !eligible(bundle, a, blocked) || !eligible(current, a, blocked) {
		return err
	}
	return nil
}

// CheckWriterAdvanced is the memory-only source check for a LATER step of one
// request whose own commit retired its original lease (D378). The caller must
// separately hold an exact proof that no command of another caller changed
// canonical state since the request's previous step, such as the provider
// coordinator's command chain, and must reauthorize in the transaction. This
// check alone admits nothing.
//
// It requires the exact writable incarnation of target, and that the Store's
// authority is the lease's original authority advanced ONLY in its installed
// revision: any change of mode, member, term, incarnation, observed epoch,
// lineage, parent, conflict or divergence refuses. Unlike CheckWriterLease it
// does not require the lease to be the current publication, which the
// request's own commits make impossible. It never captures a successor lease.
func (p *Publisher) CheckWriterAdvanced(ctx context.Context, lease *Lease, target TermActivation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if target.Term.Validate() != nil || target.Incarnation == 0 {
		return replication.ErrInvalid
	}
	if p.closed.Load() {
		return ErrClosed
	}
	a, blocked := p.opts.Source.ServingAuthority()
	if !a.Term.Same(target.Term) || a.Incarnation != target.Incarnation || !a.Writable() || blocked {
		return ErrSuperseded
	}
	bundle := lease.Bundle()
	if bundle == nil || bundle.dnsOnly || !bundle.token.AdvancedTo(a) {
		return ErrPending
	}
	return nil
}

// TermActivation is the exact local authority incarnation that must be served
// before root activates ordinary leader-only writers.
type TermActivation struct {
	Term        replication.Term
	Incarnation uint64
}

// WaitTerm waits for current complete publication in this writable incarnation.
// Preserve the elector's writer-token context when calling it. It does not mint
// writer authority or change the durable role itself.
func (p *Publisher) WaitTerm(ctx context.Context, target TermActivation) error {
	for {
		changed := p.Subscribe()
		authority := p.opts.Source.AuthorityChanges()
		s := p.Status()
		if p.closed.Load() {
			return ErrClosed
		}
		if !s.Authority.Term.Same(target.Term) || s.Authority.Incarnation != target.Incarnation {
			return ErrSuperseded
		}
		if s.ProtectedReady && s.Authority.Writable() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.stop:
			return ErrClosed
		case <-changed:
		case <-authority:
		}
	}
}
