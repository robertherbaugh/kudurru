package central

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// EnsureSource registers a shipping source (a node's local file, named by
// its UUID) inside tx and returns its id; it refreshes the node, version
// and last_seen of a known one.
func EnsureSource(ctx context.Context, tx pgx.Tx, uuid, kind, node, version string) (int32, error) {
	var id int32
	err := tx.QueryRow(ctx, `INSERT INTO source (uuid, kind, node, version) VALUES ($1, $2, $3, $4)
		ON CONFLICT (uuid) DO UPDATE SET node = excluded.node, version = excluded.version, last_seen = now()
		RETURNING id`, uuid, kind, node, version).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("central: register source %s: %w", uuid, err)
	}
	return id, nil
}

// Cursor returns the last id shipped from the source with uuid on stream,
// 0 when none.
func Cursor(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, uuid, stream string) (int64, error) {
	var last int64
	err := q.QueryRow(ctx, `SELECT coalesce((SELECT c.last_id FROM ship_cursor c JOIN source s ON s.id = c.source
		WHERE s.uuid = $1 AND c.stream = $2), 0)`, uuid, stream).Scan(&last)
	if err != nil {
		return 0, fmt.Errorf("central: read the %s cursor of %s: %w", stream, uuid, err)
	}
	return last, nil
}

// SetCursor records the last id shipped from source on stream inside tx.
func SetCursor(ctx context.Context, tx pgx.Tx, source int32, stream string, last int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO ship_cursor (source, stream, last_id, updated_at) VALUES ($1, $2, $3, now())
		ON CONFLICT (source, stream) DO UPDATE SET last_id = excluded.last_id, updated_at = now()`, source, stream, last)
	if err != nil {
		return fmt.Errorf("central: write the %s cursor: %w", stream, err)
	}
	return nil
}
