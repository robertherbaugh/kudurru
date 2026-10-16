package dhcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type leaseSetup struct {
	token    string
	expected uint64
}

// LeaseSettings is detached committed API metadata, read without opening a
// mutable allocator. Revision zero means no persisted API settings exist.
type LeaseSettings struct {
	Revision          uint64
	Options           PoolOptions
	Reservations      []Reservation
	AppliedSetupToken string
}

// OpenLeasesForSetup applies a scope's staged configuration once. The matching
// token on later opens preserves ordinary live API edits. A changed token must
// match the captured revision; desired settings, live-state validation and the
// new token commit in one schema transaction, before returning any allocator.
func OpenLeasesForSetup(path string, cfg Config, token string, expectedRevision uint64) (*LeaseStore, error) {
	if token == "" || len(token) > 128 || strings.ContainsAny(token, "\x00\r\n") {
		return nil, fmt.Errorf("dhcp: invalid setup token")
	}
	if cfg.Management != "api" {
		return nil, fmt.Errorf("dhcp: UI setup requires management=api")
	}
	return openLeases(path, cfg, &leaseSetup{token: token, expected: expectedRevision})
}

func (s *LeaseStore) applySetup(tx *sql.Tx, revision uint64) (Config, uint64, error) {
	if revision != s.setup.expected {
		return Config{}, 0, fmt.Errorf("%w: staged setup expected revision %d, found %d", ErrRevisionConflict, s.setup.expected, revision)
	}
	if revision >= math.MaxInt64 {
		return Config{}, 0, fmt.Errorf("dhcp: configuration revision exhausted")
	}
	next := revision + 1
	if err := writeLeaseSettings(context.Background(), tx, s.cfg, next); err != nil {
		return Config{}, 0, err
	}
	if _, err := tx.Exec("UPDATE lease_settings SET setup_token=? WHERE singleton=1", s.setup.token); err != nil {
		return Config{}, 0, err
	}
	// initialize validates every live row against this candidate before COMMIT.
	return s.cfg, next, nil
}

func openLeaseReader(ctx context.Context, path string) (*sql.DB, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// Inspect without flock: an active allocator may own the DB. The file helper
	// rejects aliases/special files, and mode=ro never creates a missing DB.
	file, err := readOnlyLeaseFile(path)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	uri := url.URL{Scheme: "file", Path: path}
	uri.RawQuery = url.Values{"mode": {"ro"}, "_pragma": {"query_only(ON)", "busy_timeout(1000)"}}.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// A crash before the first schema commit, or a rejected first apply, can leave
// an empty SQLite file. Expose revision zero for repair without accepting any
// unrelated schema or bypassing startup's expected revision check.
func leaseReaderSchema(ctx context.Context, tx *sql.Tx) (int, error) {
	var version, app int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, err
	}
	if err := tx.QueryRowContext(ctx, "PRAGMA application_id").Scan(&app); err != nil {
		return 0, err
	}
	if app == 0 && version == 0 {
		var entries int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*'").Scan(&entries); err != nil {
			return 0, err
		}
		if entries == 0 {
			return 0, nil
		}
	}
	if app != leaseAppID || version < 1 || version > leaseSchema {
		return 0, fmt.Errorf("dhcp: unsupported lease database application/schema version %d/%d", app, version)
	}
	return version, nil
}

func readLeaseSettings(ctx context.Context, tx *sql.Tx) (LeaseSettings, error) {
	version, err := leaseReaderSchema(ctx, tx)
	if err != nil {
		return LeaseSettings{}, err
	}
	if version <= 1 {
		return LeaseSettings{}, nil
	}
	var settings LeaseSettings
	var options, reservations string
	query := "SELECT revision, options, reservations FROM lease_settings WHERE singleton=1"
	dest := []any{&settings.Revision, &options, &reservations}
	if version >= 3 {
		query = "SELECT revision, options, reservations, setup_token FROM lease_settings WHERE singleton=1"
		dest = append(dest, &settings.AppliedSetupToken)
	}
	err = tx.QueryRowContext(ctx, query).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return LeaseSettings{}, nil
	}
	if err != nil {
		return LeaseSettings{}, err
	}
	if settings.Revision == 0 || settings.Revision > math.MaxInt64 {
		return LeaseSettings{}, fmt.Errorf("dhcp: invalid persisted configuration revision")
	}
	if err := json.Unmarshal([]byte(options), &settings.Options); err != nil {
		return LeaseSettings{}, fmt.Errorf("dhcp: decode persisted options: %w", err)
	}
	if err := json.Unmarshal([]byte(reservations), &settings.Reservations); err != nil {
		return LeaseSettings{}, fmt.Errorf("dhcp: decode persisted reservations: %w", err)
	}
	return settings, nil
}

// ReadLeaseSettings reads a SQLite snapshot without locking its owner or
// changing settings, leases, schema, or permissions. A missing DB returns zero
// settings, exists=false and nil, and creates no DB or sidecar files.
func ReadLeaseSettings(ctx context.Context, path string) (settings LeaseSettings, exists bool, err error) {
	if err := ctx.Err(); err != nil {
		return LeaseSettings{}, false, err
	}
	db, err := openLeaseReader(ctx, path)
	if errors.Is(err, os.ErrNotExist) {
		return LeaseSettings{}, false, nil
	}
	if err != nil {
		return LeaseSettings{}, false, err
	}
	defer func() { _ = db.Close() }()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return LeaseSettings{}, true, err
	}
	defer func() { _ = tx.Rollback() }()
	settings, err = readLeaseSettings(ctx, tx)
	return settings, true, err
}

// CheckLeaseSetup checks revision and all current live rows in a read-only
// SQLite snapshot. Startup repeats this check in its write transaction because
// the active allocator may commit again after this advisory staging check.
func CheckLeaseSetup(ctx context.Context, path string, cfg Config, expectedRevision uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	db, err := openLeaseReader(ctx, path)
	if errors.Is(err, os.ErrNotExist) {
		if expectedRevision != 0 {
			return ErrRevisionConflict
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	settings, err := readLeaseSettings(ctx, tx)
	if err != nil {
		return err
	}
	if settings.Revision != expectedRevision {
		return ErrRevisionConflict
	}
	if settings.Revision == 0 {
		version, err := leaseReaderSchema(ctx, tx)
		if err != nil {
			return err
		}
		if version == 0 {
			return nil
		}
	}
	leases, err := readStoredLeases(ctx, tx)
	if err != nil {
		return err
	}
	now := time.Now()
	live := leases[:0]
	for _, l := range leases {
		if now.Before(l.ExpiresAt) {
			live = append(live, l)
		}
	}
	return validateLeaseCandidate(normalizeLeaseConfig(cfg), live)
}
