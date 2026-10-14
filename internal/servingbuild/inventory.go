package servingbuild

import (
	"time"

	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/inventory/persistence"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// InventoryOwner contains the immutable baseline from this complete image.
// A nil Snapshot represents an explicitly validated empty durable root; it is
// never permission to use a caller's unrelated producer snapshot.
type InventoryOwner struct{ Snapshot *inventory.Snapshot }

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
