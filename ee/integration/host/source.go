package host

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/inventory"
)

// SourceSnapshot is one complete authenticated observation. Providers preserve
// genuine LastSeen in ObservedAt and absolute ExpiresAt; poll time cannot renew
// stale devices. Desired=nil means no DNS publication capability/request.
type SourceSnapshot struct {
	Clients  []inventory.Client
	Desired  *DesiredBatch
	Complete bool
}

// Observer adapts inventory providers to one coherent bounded host-paced poll.
// The legacy two-list ClientSource cannot prove pagination completeness/lifetime.
type Observer interface {
	Observe(context.Context) (SourceSnapshot, error)
}

// SourceID names one immutable cluster+Integration UID inventory/publication
// owner. Configuration changes replace that source rather than leaking slots.
func SourceID(owner InstanceID) string {
	if owner.Validate() != nil {
		return ""
	}
	b, _ := json.Marshal(owner)
	return "integration-" + kinds.Digest(b)
}
func (h *Host) observe(ctx context.Context, j *job, g ExecutionGuard) (*Result, error) {
	observer, ok := j.provider.(Observer)
	if !ok {
		return nil, ErrUnsupported
	}
	if h.opts.ValidateSource != nil || h.opts.CommitSource != nil {
		if h.opts.ValidateSource == nil || h.opts.CommitSource == nil || h.opts.BeforeObserve == nil {
			return nil, ErrInvalid
		}
		return h.observeSource(ctx, j, g, observer)
	}
	if h.opts.Inventory == nil {
		return nil, ErrUnsupported
	}
	snapshot, err := observer.Observe(ctx)
	if err != nil {
		return nil, safeError(ctx, err)
	}
	if !snapshot.Complete {
		return nil, ErrInvalid
	}
	if snapshot.Desired != nil && h.opts.Publish == nil {
		return nil, ErrUnsupported
	}
	if err := checkSourceBounds(snapshot, false); err != nil {
		return nil, err
	}
	if err := h.fence(ctx, j, g); err != nil {
		return nil, err
	}
	source := SourceID(g.Ledger.Owner)
	// Validate in isolation before either publication. A malformed observation
	// cannot remove/renew last-good inventory or authorize DNS source deletes.
	verifier := inventory.New(h.opts.Now)
	if err := verifier.Replace(source, 1, snapshot.Clients); err != nil {
		return nil, ErrInvalid
	}
	if snapshot.Desired != nil {
		batch := cloneBatch(*snapshot.Desired)
		batch.SourceID = source
		if err := h.opts.Publish(ctx, g, batch); err != nil {
			return nil, err
		}
	}
	if err := h.fence(ctx, j, g); err != nil {
		return nil, err
	}
	var revision uint64 = 1
	if previous := h.opts.Inventory.Snapshot(); previous != nil {
		revision = previous.Generation() + 1
		if revision == 0 {
			return nil, ErrCapacity
		}
	}
	// Hold incarnation authority through the in-memory replacement. Root's
	// publication callback independently fences its durable admission boundary.
	err = h.WithLease(ctx, g.Ledger, func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return h.opts.Inventory.Replace(source, revision, snapshot.Clients)
	})
	if err != nil {
		return nil, err
	}
	return &Result{Clients: len(snapshot.Clients)}, nil
}

func (h *Host) observeSource(ctx context.Context, j *job, g ExecutionGuard, observer Observer) (*Result, error) {
	ctx, scope, closeScope, err := h.beginSource(ctx, j, g)
	if err != nil {
		return nil, err
	}
	defer closeScope()
	bound, release, err := h.opts.BeforeObserve(ctx, scope.lease, g)
	if release != nil {
		defer release()
	}
	if err != nil {
		return nil, err
	}
	if bound == nil || release == nil {
		return nil, ErrFenced
	}
	request, requestOK := bound.Value(scopeKey{}).(requestScope)
	original := ctx.Value(scopeKey{}).(requestScope)
	if bound.Value(sourceScopeKey{}) != scope || !requestOK || request.j != j || request.g != g || request.original != original.original {
		return nil, ErrFenced
	}
	deadline, ok := bound.Deadline()
	if !ok || deadline.After(scope.deadline) {
		return nil, ErrFenced
	}
	// Link the ORIGINAL context even if a binder incorrectly detaches its
	// cancellation. The private scope/deadline cannot be recreated or renewed.
	bound, cancel := context.WithCancel(bound)
	stopOriginal := context.AfterFunc(ctx, cancel)
	defer stopOriginal()
	defer cancel()
	if _, err := h.SourceCapture(bound, g); err != nil {
		return nil, err
	}
	if err := h.fence(bound, j, g); err != nil {
		return nil, err
	}
	snapshot, err := observer.Observe(bound)
	if err != nil {
		return nil, safeError(bound, err)
	}
	if !snapshot.Complete {
		return nil, ErrInvalid
	}
	if err := checkSourceBounds(snapshot, true); err != nil {
		return nil, err
	}
	source := SourceID(g.Ledger.Owner)
	for _, c := range snapshot.Clients {
		if c.Source != source {
			return nil, ErrInvalid
		}
	}
	if snapshot.Desired != nil && snapshot.Desired.SourceID != source {
		return nil, ErrInvalid
	}
	if err := h.opts.ValidateSource(g, snapshot); err != nil {
		return nil, err
	}
	if _, err := h.SourceCapture(bound, g); err != nil {
		return nil, err
	}
	if err := h.fence(bound, j, g); err != nil {
		return nil, err
	}
	snapshot = cloneSource(snapshot)
	result, err := h.opts.CommitSource(bound, g, snapshot)
	result, outcomeErr := sourceResult(result, err)
	if outcomeErr != nil {
		return result, outcomeErr
	}
	return result, err
}

func cloneSource(in SourceSnapshot) SourceSnapshot {
	out := SourceSnapshot{Complete: in.Complete, Clients: slices.Clone(in.Clients)}
	for i := range out.Clients {
		out.Clients[i].IPs = slices.Clone(in.Clients[i].IPs)
		out.Clients[i].MACs = slices.Clone(in.Clients[i].MACs)
		out.Clients[i].Tags = slices.Clone(in.Clients[i].Tags)
	}
	if in.Desired != nil {
		batch := cloneBatch(*in.Desired)
		out.Desired = &batch
	}
	return out
}

// A callback may return durable admission with a later publication error. Keep
// that evidence, but never turn an uncommitted/rollback result into success.
func sourceResult(in *Result, callbackErr error) (*Result, error) {
	if in == nil {
		if callbackErr != nil {
			return nil, callbackErr
		}
		return nil, ErrInvalid
	}
	r := valueResult(in)
	if r.Applied < 0 || r.Pending < 0 || r.Conflicts < 0 || r.Unchanged < 0 || r.Clients < 0 || r.Applied > 100000 || r.Pending > 100000 || r.Conflicts > 100000 || r.Unchanged > 100000 || r.Clients > kinds.MaxSourceClients {
		return nil, ErrInvalid
	}
	s := r.Source
	if s == nil {
		return nil, ErrInvalid
	}
	if len(s.ReceiptID) > 512 || strings.IndexFunc(s.ReceiptID, func(r rune) bool { return r < '!' || r > '~' }) >= 0 || (s.Published || s.Deduplicated) && !s.Committed || s.Committed && s.ReceiptID == "" {
		return nil, ErrInvalid
	}
	if !s.Committed {
		r.Applied, r.Clients, r.Unchanged = 0, 0, 0
		if callbackErr == nil {
			return &r, ErrInvalid
		}
	} else if !s.Published && r.Pending == 0 {
		r.Pending = 1
	}
	return &r, callbackErr
}

func cloneBatch(in DesiredBatch) DesiredBatch {
	out := DesiredBatch{SourceID: in.SourceID}
	for _, z := range in.Zones {
		out.Zones = append(out.Zones, z.DeepCopy())
	}
	for _, r := range in.RecordSets {
		out.RecordSets = append(out.RecordSets, r.DeepCopy())
	}
	for _, g := range in.ClientGroups {
		out.ClientGroups = append(out.ClientGroups, g.DeepCopy())
	}
	return out
}
