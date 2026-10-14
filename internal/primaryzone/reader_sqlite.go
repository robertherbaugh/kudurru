package primaryzone

import (
	"context"

	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

type sqliteReadCommand interface {
	Authority() replication.Authority
	Command(context.Context, replication.Term, func(context.Context, *sqlite.CommandTx) error) error
}

// readSQLite bounds each page while keeping one actual database snapshot.
// This existing command capability requires current primary authority and
// holds the writer fence during the bounded read; no external callbacks run
// inside it. No-op Command rolls back its read transaction without revisions.
func (r *Reader) readSQLite(ctx context.Context, id zonekinds.ZoneID, targets []zonestate.DesiredTarget) (bool, *zonestate.DesiredInputs, error) {
	st, ok := r.store.(sqliteReadCommand)
	if !ok {
		return false, nil, nil
	}
	var out *zonestate.DesiredInputs
	err := st.Command(ctx, st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		out, err = r.readCut(ctx, id, targets, tx, true)
		return err
	})
	return true, out, err
}
