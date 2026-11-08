package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strconv"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// ErrExistingDatabase means an offline operation cannot use this database as-is.
// Recovery must not implicitly initialize, upgrade or repair its original input.
var ErrExistingDatabase = errors.New("store/sqlite: existing current-schema WAL database required")

// OpenExisting opens an already initialized current-schema database under the
// normal exclusive process lock and the caller's original finite lifetime. It
// never creates a database, migrates tables, changes journal mode, initializes
// authority or fills missing metadata. SQLite may recover its existing WAL and
// create its normal lock/shared-memory files. The caller must keep other users
// stopped and independently authorize any subsequent mutation.
func OpenExisting(ctx context.Context, path string, opts Options) (*Store, error) {
	if ctx == nil {
		return nil, ErrExistingDatabase
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline, ok := ctx.Deadline()
	if !ok || !time.Now().Before(deadline) || time.Until(deadline) > 30*time.Second {
		return nil, ErrExistingDatabase
	}
	return openStore(ctx, path, opts, true)
}

// mode=rw prevents SQLite from creating a replacement if the file disappears.
// All retained pragmas affect only this connection; journal mode is read below.
func existingDSN(path string, busy time.Duration, writer bool) string {
	u, err := url.Parse(dsn(path, busy, writer))
	if err != nil {
		panic("sqlite: internally generated database URI is invalid")
	}
	q := u.Query()
	pragmas := q["_pragma"]
	q.Del("_pragma")
	for _, pragma := range pragmas {
		if pragma != "journal_mode(WAL)" {
			q.Add("_pragma", pragma)
		}
	}
	q.Set("mode", "rw")
	u.RawQuery = q.Encode()
	return u.String()
}

func checkExistingSchema(ctx context.Context, db *sql.DB) error {
	var journal, version string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
		return err
	}
	if journal != "wal" {
		return ErrExistingDatabase
	}
	if err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, metaSchemaVersion).Scan(&version); err != nil {
		return err
	}
	if version != strconv.Itoa(store.SchemaVersion) {
		return ErrExistingDatabase
	}
	return validateAuditFormats(ctx, db)
}
