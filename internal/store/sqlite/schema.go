package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// meta keys.
const (
	metaSchemaVersion   = "schema_version"
	metaClusterID       = "cluster_id"
	metaClusterEpoch    = "cluster_epoch"
	metaResourceVersion = "resource_version"
	metaFeedCompacted   = "feed_compacted_through"
	metaCreatedAt       = "created_at"
)

// migration moves state.db from version-1 to version. Steps run in order
// inside the single transaction migrate opens, so a failed upgrade leaves
// the database exactly as it was.
type migration struct {
	version int
	name    string
	up      func(ctx context.Context, tx *sql.Tx) error
}

// migrations is the ordered list of schema steps; migrations[i].version
// must be i+1 and the last version must equal store.SchemaVersion
// (TestMigrationsCoverSchemaVersion).
var migrations = []migration{
	{version: 1, name: "objects, change feed, audit", up: migrate1},
	{version: 2, name: "replication authority and owned sections", up: migrate2},
	{version: 3, name: "bind stored credentials to account identities", up: migrateCredentialUIDs},
}

// schemaV1 is migration 1. Objects keep the whole JSON document in data;
// the other columns are what the driver filters, orders and compares on.
// changes.seq is the resourceVersion of the write that produced the row.
const schemaV1 = `
CREATE TABLE objects (
	kind             TEXT    NOT NULL,
	namespace        TEXT    NOT NULL,
	name             TEXT    NOT NULL,
	uid              TEXT    NOT NULL,
	generation       INTEGER NOT NULL,
	resource_version INTEGER NOT NULL,
	managed_by       TEXT    NOT NULL,
	created_at       TEXT    NOT NULL,
	updated_at       INTEGER NOT NULL,
	spec_hash        TEXT    NOT NULL,
	labels           TEXT    NOT NULL DEFAULT '{}',
	deleting         INTEGER NOT NULL DEFAULT 0,
	data             TEXT    NOT NULL,
	PRIMARY KEY (kind, namespace, name)
) STRICT, WITHOUT ROWID;

CREATE TABLE changes (
	seq              INTEGER PRIMARY KEY,
	kind             TEXT    NOT NULL,
	namespace        TEXT    NOT NULL,
	name             TEXT    NOT NULL,
	type             TEXT    NOT NULL CHECK (type IN ('ADDED', 'MODIFIED', 'DELETED')),
	resource_version TEXT    NOT NULL,
	tombstone        TEXT
) STRICT;
CREATE INDEX changes_by_kind ON changes(kind, seq);

CREATE TABLE audit (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	ts               INTEGER NOT NULL,
	actor            TEXT    NOT NULL,
	manager          TEXT    NOT NULL,
	action           TEXT    NOT NULL,
	kind             TEXT    NOT NULL,
	namespace        TEXT    NOT NULL,
	name             TEXT    NOT NULL,
	resource_version TEXT    NOT NULL,
	result           TEXT    NOT NULL,
	reason           TEXT    NOT NULL DEFAULT '',
	before           TEXT,
	after            TEXT,
	prev_hash        TEXT    NOT NULL,
	hash             TEXT    NOT NULL
) STRICT;
CREATE INDEX audit_by_key ON audit(kind, namespace, name, id);
CREATE INDEX audit_by_ts ON audit(ts);
`

func migrate1(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, schemaV1)
	return err
}

func migrate2(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TABLE replication_sections (
		section TEXT NOT NULL CHECK (section IN ('runtime', 'security', 'membership', 'dedup')),
		key TEXT NOT NULL, data TEXT NOT NULL,
		PRIMARY KEY(section, key)
	) STRICT, WITHOUT ROWID;
	CREATE TABLE replication_quarantine (
		generation TEXT PRIMARY KEY, image TEXT NOT NULL
	) STRICT, WITHOUT ROWID;`)
	return err
}

// migrate brings the database to target in one transaction: it creates
// the meta table, refuses a database newer than target (ErrSchemaTooNew),
// runs every step above the recorded version, and initialises the identity
// rows (cluster_id once, cluster_epoch, counters) when they are missing.
func migrate(ctx context.Context, db *sql.DB, steps []migration, target int, now func() time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store/sqlite: begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT, WITHOUT ROWID`); err != nil {
		return fmt.Errorf("store/sqlite: create meta: %w", err)
	}
	current := 0
	var v string
	switch err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaSchemaVersion).Scan(&v); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("store/sqlite: read schema_version: %w", err)
	default:
		if current, err = strconv.Atoi(v); err != nil || current < 0 {
			return fmt.Errorf("store/sqlite: bad schema_version %q in state.db", v)
		}
	}
	if current > target {
		return fmt.Errorf("%w: state.db is at schema version %d, this binary supports up to %d; "+
			"run the newer kudurru release or restore a backup taken with this one", ErrSchemaTooNew, current, target)
	}
	for _, m := range steps {
		if m.version <= current || m.version > target {
			continue
		}
		if m.version != current+1 {
			return fmt.Errorf("store/sqlite: no migration from schema %d to %d", current, m.version)
		}
		if err := m.up(ctx, tx); err != nil {
			return fmt.Errorf("store/sqlite: migration %d (%s): %w", m.version, m.name, err)
		}
		current = m.version
	}
	if current != target {
		return fmt.Errorf("store/sqlite: no migration reaches schema %d (stopped at %d)", target, current)
	}
	defaults := map[string]string{
		metaClusterID:       uuid.NewString(),
		metaClusterEpoch:    "1",
		metaResourceVersion: "0",
		metaFeedCompacted:   "0",
		metaCreatedAt:       now().UTC().Format(time.RFC3339),
	}
	for k, val := range defaults {
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO NOTHING`, k, val); err != nil {
			return fmt.Errorf("store/sqlite: init meta %s: %w", k, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		metaSchemaVersion, strconv.Itoa(target)); err != nil {
		return fmt.Errorf("store/sqlite: write schema_version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store/sqlite: commit migration: %w", err)
	}
	return nil
}

// queryer is satisfied by *sql.DB and *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// readMeta returns every meta row.
func readMeta(ctx context.Context, q queryer) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT key, value FROM meta`)
	if err != nil {
		return nil, fmt.Errorf("store/sqlite: read meta: %w", err)
	}
	defer func() { _ = rows.Close() }()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("store/sqlite: read meta: %w", err)
		}
		m[k] = v
	}
	return m, rows.Err()
}
