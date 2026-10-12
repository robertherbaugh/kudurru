package sqlitelog

import (
	"context"
	"fmt"
	"math"
	"time"
)

// Purge deletes the per-query rows older than before, or every row when
// before is zero (DELETE /api/v1/querylog; it implements api.QuerylogPurger).
// It runs in DeleteBatch-row transactions with an incremental_vacuum after
// each, like retention, so ingest continues meanwhile. The rollups (the
// statistics behind the dashboard charts) and the dictionaries are kept;
// retention ages them out. Events still in the live tail's memory ring are
// not touched.
func (w *Writer) Purge(ctx context.Context, before time.Time) (int64, error) {
	cutoff := int64(math.MaxInt64)
	if !before.IsZero() {
		cutoff = before.UnixMicro()
	}
	n, err := w.deleteFacts(ctx, `DELETE FROM query WHERE id IN (SELECT id FROM query WHERE ts < ? LIMIT `+fmt.Sprint(w.deleteBatch)+`)`, cutoff)
	if err != nil {
		return n, err
	}
	w.log.Info("query log purged", "deleted", n, "before", before)
	return n, nil
}
