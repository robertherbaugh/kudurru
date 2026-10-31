package host

import (
	"context"
	"fmt"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
)

type scopeKey struct{}
type requestScope struct {
	j        *job
	g        ExecutionGuard
	original *requestOrigin
}

// Captured at command entry, before any publisher/observer network work.
// Receipts or a same-guard refresh cannot substitute a newer frame/lease.
type requestOrigin struct {
	instance *Instance
	lease    context.Context
	input    Input
}

func scoped(ctx context.Context, j *job, g ExecutionGuard) context.Context {
	return context.WithValue(ctx, scopeKey{}, requestScope{j: j, g: g})
}

func (h *Host) scopedOriginal(ctx context.Context, j *job, f frame, g ExecutionGuard) (context.Context, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.jobs[j.key] != j || !j.active || j.guard != g.Ledger || j.frame.instance != f.instance || j.frame.input.BundleID != g.BundleID || j.ctx.Err() != nil || j.leaseCtx == nil || j.leaseCtx.Err() != nil {
		return nil, ErrFenced
	}
	return context.WithValue(ctx, scopeKey{}, requestScope{j: j, g: g, original: &requestOrigin{instance: f.instance, lease: j.leaseCtx, input: f.input}}), nil
}
func (p providerHost) guard(ctx context.Context) (ExecutionGuard, error) {
	s, ok := ctx.Value(scopeKey{}).(requestScope)
	if !ok || s.j != p.j {
		return ExecutionGuard{}, ErrFenced
	}
	if err := p.h.fence(ctx, p.j, s.g); err != nil {
		return ExecutionGuard{}, err
	}
	return s.g, nil
}

func (p providerHost) Request(ctx context.Context, provider, account string, mutating bool, fn func(context.Context) error) error {
	if fn == nil || provider == "" || account == "" {
		return ErrInvalid
	}
	g, err := p.guard(ctx)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, p.h.opts.OperationTimeout)
	defer cancel()
	p.h.mu.RLock()
	leaseCtx := p.j.leaseCtx
	p.h.mu.RUnlock()
	stopLease := context.AfterFunc(leaseCtx, cancel)
	stopJob := context.AfterFunc(p.j.ctx, cancel)
	defer stopLease()
	defer stopJob()
	if err := p.h.fence(ctx, p.j, g); err != nil {
		return err
	}
	// Until provider user identity is verified, one conservative cluster bucket
	// covers all Cloudflare accounts/tokens and survives credential rotation.
	if provider == "cloudflare" {
		account = "all"
	}
	id := kinds.Digest([]byte(fmt.Sprintf("%s/%s/%s/%d", g.Ledger.Owner.UID, g.Ledger.LeaseID, provider, p.h.requestSequence.Add(1))))
	if _, err := p.h.opts.Ledger.ReserveBudget(ctx, g.Ledger, ledger.BudgetCharge{Provider: provider, Account: account, ID: id, Cost: 1, Capacity: 1000, WindowSeconds: 300}); err != nil {
		return err
	}
	if mutating {
		if err := p.h.fence(ctx, p.j, g); err != nil {
			return err
		}
	}
	// Providers return sanitized typed errors. The caller maps arbitrary error
	// text to safe host errors before exposing it to status/API consumers.
	return fn(ctx)
}

// RequestGuard exposes the identity and ORIGINAL lease already carried by a
// host-created request to its root commit adapter. Root may bind a driver token
// from that lease while retaining the request's cancellation/deadline. It cannot
// sample a successor frame or grant a lease: WithLease/WithSource and the actual
// command still independently fence admission.
func (h *Host) RequestGuard(ctx context.Context, expected ledger.Guard) (ExecutionGuard, context.Context, error) {
	s, ok := ctx.Value(scopeKey{}).(requestScope)
	if !ok || s.j == nil || s.g.Ledger != expected {
		return ExecutionGuard{}, nil, ErrFenced
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	j := s.j
	if h.jobs[j.key] != j || !j.active || j.ctx.Err() != nil || j.leaseCtx.Err() != nil || !h.opts.Allowed() || j.guard != expected || j.frame.input.BundleID != s.g.BundleID {
		return ExecutionGuard{}, nil, ErrFenced
	}
	lease := j.leaseCtx // setup scope was constructed directly from this lease
	if s.original != nil {
		if j.frame.instance != s.original.instance || lease != s.original.lease {
			return ExecutionGuard{}, nil, ErrFenced
		}
		lease = s.original.lease
	}
	return s.g, lease, nil
}

// RequestResource returns only the original root-owned resource retained at
// entry. It never samples a newer frame as a substitute for a held operation.
func (h *Host) RequestResource(ctx context.Context, expected ledger.Guard) (any, error) {
	if _, _, err := h.RequestGuard(ctx, expected); err != nil {
		return nil, err
	}
	s := ctx.Value(scopeKey{}).(requestScope)
	if s.original == nil {
		return nil, ErrFenced
	}
	return s.original.input.Resource, nil
}
