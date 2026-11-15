package servingbuild

import (
	"context"
	"time"

	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/inventory/persistence"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// InventoryOwner contains the immutable baseline from this complete image.
// A nil Snapshot represents an explicitly validated empty durable root; it is
// never permission to use a caller's unrelated producer snapshot.
type InventoryOwner struct {
	Snapshot    *inventory.Snapshot
	SourceRoots map[string]string
	MACGrants   []policy.MACGrant
}

func prepareInventory(image *replication.Image, now time.Time) (*InventoryOwner, error) {
	state, err := persistence.Decode(image.Payload.Runtime, image.Term.ClusterID, image.CreatedAt)
	if err != nil {
		return nil, err
	}
	holder, err := inventory.RestoreState(state.State, func() time.Time { return now })
	if err != nil {
		return nil, err
	}
	return &InventoryOwner{Snapshot: holder.Snapshot()}, nil
}

// InventoryReads returns the inventory snapshot of the original HTTP request's
// serving cut: the same immutable owner DNS queries of that generation use
// (D397). It never reads the store or a live producer holder.
func InventoryReads(ctx context.Context) (*inventory.Snapshot, error) {
	if ctx == nil {
		return nil, serving.ErrComponents
	}
	if _, err := servinghttp.Identity(ctx); err != nil {
		return nil, err
	}
	owner, err := servinghttp.Component[*InventoryOwner](ctx, "inventory")
	if err != nil {
		return nil, err
	}
	if owner == nil {
		return nil, serving.ErrComponents
	}
	return owner.Snapshot, nil
}
