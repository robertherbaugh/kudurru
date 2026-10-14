package serving

import (
	"context"

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
}

// Status reads local atomics/bookkeeping, never SQLite or owner private values.
func (p *Publisher) Status() Status {
	// A dependency cursor may reference owner state; retain it while checking
	// eligibility so a concurrent replacement cannot retire its resources.
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
	s := Status{Authority: a, Installed: a.Installed, Preparing: preparing, Reason: reason}
	if token.Matches(a) {
		s.Installed = token.Generation()
	}
	s.InstalledComplete = s.Installed.Validate() == nil
	if bundle != nil {
		s.Served = bundle.identity
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
