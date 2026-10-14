package host

import (
	"context"
	"encoding/json"

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
	if len(snapshot.Clients) > 100000 || snapshot.Desired != nil && (len(snapshot.Desired.Zones) > 256 || len(snapshot.Desired.RecordSets) > 10000 || len(snapshot.Desired.ClientGroups) > 256) {
		return nil, ErrCapacity
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil || len(encoded) > 32<<20 {
		return nil, ErrCapacity
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
