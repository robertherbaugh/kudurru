package servingbuild

import (
	"context"

	internal "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// MigrationReads projects private journals from the original complete HTTP image.
type MigrationReads struct{}

// NewMigrationReads creates a captured journal reader with no live Store fallback.
func NewMigrationReads() *MigrationReads { return &MigrationReads{} }

// MigrationSnapshot copies journals and their original generation from the held lease.
func (r *MigrationReads) MigrationSnapshot(ctx context.Context) ([]*internal.MigrationRun, replication.Generation, error) {
	id, err := servinghttp.Identity(ctx)
	if err != nil {
		return nil, replication.Generation{}, err
	}
	owner, err := servinghttp.Component[*RuntimeOwner](ctx, "runtime")
	if err != nil {
		return nil, replication.Generation{}, err
	}
	if owner.primary == nil || !owner.primary.Base().Same(id.Base) {
		return nil, replication.Generation{}, serving.ErrComponents
	}
	out := make([]*internal.MigrationRun, len(owner.migrations))
	for i, run := range owner.migrations {
		out[i] = run.DeepCopy()
	}
	return out, id.Base, nil
}
