// Package central is the optional cluster-wide database of runtime data:
// audit events, query-log rows and statistics from every node in one
// PostgreSQL (16 or newer) database, so every replica gives the same
// answer and the data outlives the nodes (dev-docs/design-record.md §4.5,
// §11.2, §11.4; D158).
//
// Configuration objects never go here (they stay in CRDs or state.db).
// The database is never on the DNS path and never part of readiness:
// nodes write their local SQLite files first and ship to the database
// (audit.Shipper, the query-log shipper), so an outage delays the
// cluster-wide view but loses nothing the local files still hold.
//
// The driver is pgx v5, pure Go, so CGO_ENABLED=0 holds.
package central

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus"
)

// MinServerVersion is the oldest PostgreSQL kudurru supports (16; the
// schema needs 15 for UNIQUE NULLS NOT DISTINCT).
const MinServerVersion = 160000

// Sentinel errors of Pool.
var (
	// ErrUnavailable: the database has not been reached (or migrated) yet,
	// or the last health check failed.
	ErrUnavailable = errors.New("central: database unavailable")
	// ErrIncompatible: the database schema was migrated by a newer kudurru
	// that this binary cannot use (compat_version above SchemaVersion).
	ErrIncompatible = errors.New("central: database schema is newer than this binary can use")
)

// Options configures Open.
type Options struct {
	// DSN is a PostgreSQL connection string (URL or key=value).
	DSN string
	// MaxConns caps the pool; default 4.
	MaxConns int32
	// Node and Version name the connections (application_name).
	Node, Version string
	// Registerer receives the kudurru_central_* metrics; nil skips them.
	Registerer prometheus.Registerer
	Logger     *slog.Logger
	// HealthInterval is how often a ready database is pinged; default
	// 15 s. MaxBackoff caps the reconnect delay; default 1 min.
	HealthInterval, MaxBackoff time.Duration
}

// State is what the node knows of the database.
type State struct {
	// Ready: connected, version checked, schema migrated.
	Ready bool `json:"ready"`
	// LastError is the last connection or migration failure.
	LastError string `json:"last_error,omitempty"`
	// Since is when Ready last changed.
	Since time.Time `json:"since"`
	// ServerVersion is server_version_num (e.g. 170010).
	ServerVersion int `json:"server_version,omitempty"`
	// SchemaVersion and CompatVersion are the database's schema versions.
	SchemaVersion int `json:"schema_version,omitempty"`
	CompatVersion int `json:"compat_version,omitempty"`
	// Incompatible: a newer kudurru migrated the schema past what this
	// binary can use.
	Incompatible bool `json:"incompatible,omitempty"`
}

// DB is the node's handle on the central database.
type DB struct {
	opts    Options
	pool    *pgxpool.Pool
	metrics *metrics

	sqlOnce sync.Once
	sqlDB   *sql.DB

	mu    sync.Mutex
	state State
}

// Open parses the DSN and builds the pool. It does not connect: Run does,
// in the background, so a node starts and serves DNS while the database
// is away. Errors never contain the DSN (it may hold a password).
func Open(o Options) (*DB, error) {
	if o.MaxConns <= 0 {
		o.MaxConns = 4
	}
	if o.HealthInterval <= 0 {
		o.HealthInterval = 15 * time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = time.Minute
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	cfg, err := ParseDSN(o.DSN)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = o.MaxConns
	cfg.MinConns = 0
	cfg.HealthCheckPeriod = o.HealthInterval
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	if _, ok := cfg.ConnConfig.RuntimeParams["application_name"]; !ok {
		cfg.ConnConfig.RuntimeParams["application_name"] = "kudurru/" + o.Node
	}
	if _, ok := cfg.ConnConfig.RuntimeParams["statement_timeout"]; !ok {
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = "15000"
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, errors.New("central: database.dsn: cannot build the connection pool")
	}
	return &DB{opts: o, pool: pool, metrics: newMetrics(o.Registerer), state: State{Since: time.Now()}}, nil
}

// ParseDSN parses a connection string without echoing it in errors.
func ParseDSN(dsn string) (*pgxpool.Config, error) {
	if dsn == "" {
		return nil, errors.New("central: database.dsn is empty")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("central: database.dsn is not a valid PostgreSQL connection string (URL or key=value)")
	}
	return cfg, nil
}

// Close closes the pool.
func (d *DB) Close() {
	if d.sqlDB != nil {
		_ = d.sqlDB.Close()
	}
	d.pool.Close()
}

// SQL returns a database/sql handle on the pool (the pgx driver) once the
// database is ready, for readers written against database/sql.
func (d *DB) SQL() (*sql.DB, error) {
	if _, err := d.Pool(); err != nil {
		return nil, err
	}
	d.sqlOnce.Do(func() { d.sqlDB = stdlib.OpenDBFromPool(d.pool) })
	return d.sqlDB, nil
}

// Pool returns the pool once the database is ready.
func (d *DB) Pool() (*pgxpool.Pool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case d.state.Incompatible:
		return nil, ErrIncompatible
	case !d.state.Ready:
		return nil, ErrUnavailable
	}
	return d.pool, nil
}

// State returns the current state.
func (d *DB) State() State {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state
}

// Run connects, checks the server version, migrates the schema and then
// pings every HealthInterval until ctx ends. Failures are retried with
// backoff and never end it: the database is optional runtime data.
func (d *DB) Run(ctx context.Context) error {
	log := d.opts.Logger.With("component", "central")
	backoff := time.Duration(0)
	for {
		wait := d.opts.HealthInterval
		var err error
		if d.State().Ready {
			err = d.ping(ctx)
		} else {
			err = d.connect(ctx)
			if err == nil {
				st := d.State()
				log.Info("central database ready", "server_version", st.ServerVersion, "schema_version", st.SchemaVersion)
			}
		}
		if err != nil && ctx.Err() == nil {
			backoff = min(max(2*backoff, time.Second), d.opts.MaxBackoff)
			wait = backoff
			if d.setFailed(err) {
				log.Warn("central database unavailable; nodes keep their local data and ship it when it is back", "err", err, "retry_in", backoff)
			}
		} else if err == nil {
			backoff = 0
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

func (d *DB) ping(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := d.pool.Ping(pctx); err != nil {
		d.metrics.errors.WithLabelValues("ping").Inc()
		return err
	}
	return nil
}

func (d *DB) connect(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var v string
	if err := d.pool.QueryRow(cctx, "SHOW server_version_num").Scan(&v); err != nil {
		d.metrics.errors.WithLabelValues("connect").Inc()
		return fmt.Errorf("connect: %w", err)
	}
	num, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("server_version_num %q: %w", v, err)
	}
	if num < MinServerVersion {
		return fmt.Errorf("PostgreSQL %d.%d is too old; kudurru needs 16 or newer", num/10000, num%10000)
	}
	// nosemgrep: invalid-usage-of-modified-variable -- Migrate returns a MigrateResult value, filled on ErrIncompatible
	res, err := Migrate(cctx, d.pool)
	if err != nil {
		d.metrics.errors.WithLabelValues("migrate").Inc()
		if errors.Is(err, ErrIncompatible) {
			d.mu.Lock()
			d.state.Incompatible, d.state.SchemaVersion, d.state.CompatVersion = true, res.SchemaVersion, res.CompatVersion
			d.mu.Unlock()
		}
		return err
	}
	d.mu.Lock()
	if !d.state.Ready {
		d.state.Since = time.Now()
	}
	d.state = State{Ready: true, Since: d.state.Since, ServerVersion: num, SchemaVersion: res.SchemaVersion, CompatVersion: res.CompatVersion}
	d.mu.Unlock()
	d.metrics.up.Set(1)
	d.metrics.schema.Set(float64(res.SchemaVersion))
	return nil
}

// setFailed records err; it reports whether the state changed from ready
// or the error is new (worth a log line).
func (d *DB) setFailed(err error) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	changed := d.state.Ready || d.state.LastError != err.Error()
	if d.state.Ready {
		d.state.Since = time.Now()
	}
	d.state.Ready, d.state.LastError = false, err.Error()
	d.metrics.up.Set(0)
	return changed
}

// Tx runs fn in a transaction on the ready pool.
func (d *DB) Tx(ctx context.Context, fn func(pgx.Tx) error) error {
	pool, err := d.Pool()
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, pool, fn)
}
