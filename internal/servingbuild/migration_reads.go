package servingbuild

import (
	"context"
	internal "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

type MigrationReads struct{}

func NewMigrationReads() *MigrationReads { return &MigrationReads{} }
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
