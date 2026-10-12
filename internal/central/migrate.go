package central

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// SchemaVersion is the schema this binary writes; CompatVersion is the
// oldest schema version a binary must understand to use a database at
// SchemaVersion. Additive migrations keep CompatVersion; a migration
// that older binaries cannot work with raises it to its own version.
const (
	SchemaVersion = 2
	CompatVersion = 1
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLock is the pg_advisory_xact_lock key that serialises
// migrations of every replica ("kudurru" in ASCII).
const migrationLock = 0x6b7564757272

// MigrateResult is the database's schema after Migrate.
type MigrateResult struct {
	SchemaVersion, CompatVersion int
	// Applied lists the migrations this call ran.
	Applied []int
}

type migration struct {
	version int
	name    string
	sql     string
}

func migrations() ([]migration, error) {
	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, n := range names {
		base := strings.TrimPrefix(n, "migrations/")
		v, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
		if err != nil {
			return nil, fmt.Errorf("central: migration %s: no version prefix", base)
		}
		b, err := migrationFiles.ReadFile(n)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: base, sql: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// Querier is what Migrate needs (a pool or a connection).
type Querier interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Migrate brings the schema to SchemaVersion in one transaction, under an
// advisory lock so replicas starting together migrate once. A database at
// a newer schema is used as is when its CompatVersion is at most this
// binary's SchemaVersion (rolling upgrades mix versions); otherwise
// Migrate returns ErrIncompatible and leaves it alone. It never refuses
// to let the node start: the caller keeps serving without the database.
func Migrate(ctx context.Context, q Querier) (MigrateResult, error) {
	var res MigrateResult
	ms, err := migrations()
	if err != nil {
		return res, err
	}
	tx, err := q.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(migrationLock)); err != nil {
		return res, fmt.Errorf("central: migration lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS kudurru_meta (key text PRIMARY KEY, value text NOT NULL)`); err != nil {
		return res, fmt.Errorf("central: create kudurru_meta: %w", err)
	}
	meta := func(key string) (int, error) {
		var v string
		err := tx.QueryRow(ctx, `SELECT value FROM kudurru_meta WHERE key = $1`, key).Scan(&v)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		return strconv.Atoi(v)
	}
	if res.SchemaVersion, err = meta("schema_version"); err != nil {
		return res, fmt.Errorf("central: read schema_version: %w", err)
	}
	if res.CompatVersion, err = meta("compat_version"); err != nil {
		return res, fmt.Errorf("central: read compat_version: %w", err)
	}
	if res.SchemaVersion >= SchemaVersion {
		if res.CompatVersion > SchemaVersion {
			return res, fmt.Errorf("%w (database schema %d needs a binary at schema %d or newer; this binary is at %d)",
				ErrIncompatible, res.SchemaVersion, res.CompatVersion, SchemaVersion)
		}
		return res, nil
	}
	for _, m := range ms {
		if m.version <= res.SchemaVersion || m.version > SchemaVersion {
			continue
		}
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) {
				return res, fmt.Errorf("central: migration %s: %s (%s)", m.name, pe.Message, pe.Code)
			}
			return res, fmt.Errorf("central: migration %s: %w", m.name, err)
		}
		res.Applied = append(res.Applied, m.version)
	}
	compat := max(res.CompatVersion, CompatVersion)
	for k, v := range map[string]int{"schema_version": SchemaVersion, "compat_version": compat} {
		if _, err := tx.Exec(ctx, `INSERT INTO kudurru_meta(key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, k, strconv.Itoa(v)); err != nil {
			return res, fmt.Errorf("central: write %s: %w", k, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("central: commit migration: %w", err)
	}
	res.SchemaVersion, res.CompatVersion = SchemaVersion, compat
	return res, nil
}
