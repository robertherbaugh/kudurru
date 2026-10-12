// Package spool is the node-local audit store of nodes whose store driver
// does not keep audit rows (the kube driver; docs/architecture.md §11.4):
// audit-spool.db under node.dataDir, a hash-chained SQLite table that the
// audit routes read back and that shippers forward to a cluster-wide audit
// store by its row ids.
//
// The file is the first durable copy of an event, so it is written before
// any network store; it is capped (oldest events are dropped first and
// counted) and pruned by age. It uses the cgo-free modernc.org/sqlite
// driver, like local.db and querylog.db.
package spool

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/robertherbaugh/kudurru/internal/audit"
)

// FileName is the spool's file name under node.dataDir.
const FileName = "audit-spool.db"

// SchemaVersion is the spool schema this binary writes.
const SchemaVersion = 1

// DefaultMax is the default cap on stored events.
const DefaultMax = 50000

// ErrSchemaTooNew means the file was written by a newer binary.
var ErrSchemaTooNew = errors.New("audit spool: on-disk schema is newer than this binary")

const schemaV1 = `
CREATE TABLE IF NOT EXISTS event (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	ts         INTEGER NOT NULL,
	kind       TEXT    NOT NULL,
	name       TEXT    NOT NULL,
	actor      TEXT    NOT NULL,
	actor_name TEXT    NOT NULL,
	doc        TEXT    NOT NULL,
	prev_hash  TEXT    NOT NULL,
	hash       TEXT    NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS event_kind_name ON event(kind, name, id);
CREATE INDEX IF NOT EXISTS event_ts ON event(ts);
`

// Options configures Open.
type Options struct {
	// Path is the database file (node.dataDir/audit-spool.db).
	Path string
	// Node is recorded on events that name no node.
	Node string
	// Max caps the stored events; default DefaultMax.
	Max int
	// Registerer receives kudurru_audit_spool_dropped_total; nil skips it.
	Registerer prometheus.Registerer
	// Now is the clock for events without a time; default time.Now.
	Now func() time.Time
}

// Spool is an open audit-spool.db. It implements audit.Recorder and
// audit.Reader.
type Spool struct {
	db       *sql.DB
	opts     Options
	sourceID string
	dropped  prometheus.Counter

	mu   sync.Mutex // serialises Record: the chain head
	last string
}

var (
	_ audit.Recorder = (*Spool)(nil)
	_ audit.Reader   = (*Spool)(nil)
)

// Open opens or creates the spool at o.Path.
func Open(ctx context.Context, o Options) (*Spool, error) {
	if o.Max <= 0 {
		o.Max = DefaultMax
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	// Create the file 0600 before SQLite does (it would use 0644): audit
	// events carry object documents and client addresses.
	if f, err := os.OpenFile(o.Path, os.O_RDWR|os.O_CREATE, 0o600); err == nil { // #nosec G304 -- operator-supplied data path
		_ = f.Close()
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: o.Path}).EscapedPath()+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("audit spool: open %s: %w", o.Path, err)
	}
	db.SetMaxOpenConns(4)
	s := &Spool{db: db, opts: o, dropped: prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kudurru_audit_spool_dropped_total",
		Help: "Audit events dropped from the node's audit spool because it reached its cap (oldest first).",
	})}
	if o.Registerer != nil {
		if err := o.Registerer.Register(s.dropped); err != nil {
			var are prometheus.AlreadyRegisteredError
			if !errors.As(err, &are) {
				_ = db.Close()
				return nil, err
			}
			s.dropped = are.ExistingCollector.(prometheus.Counter)
		}
	}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Spool) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("audit spool: begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT`); err != nil {
		return fmt.Errorf("audit spool: create meta: %w", err)
	}
	meta := func(key string) (string, error) {
		var v string
		err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return v, err
	}
	v, err := meta("schema_version")
	if err != nil {
		return fmt.Errorf("audit spool: read schema version: %w", err)
	}
	version := 0
	if v != "" {
		if version, err = strconv.Atoi(v); err != nil {
			return fmt.Errorf("audit spool: schema version %q: %w", v, err)
		}
	}
	if version > SchemaVersion {
		return fmt.Errorf("%w (file %d, binary %d)", ErrSchemaTooNew, version, SchemaVersion)
	}
	if version < 1 {
		if _, err := tx.ExecContext(ctx, schemaV1); err != nil {
			return fmt.Errorf("audit spool: create schema: %w", err)
		}
	}
	if s.sourceID, err = meta("source_id"); err != nil {
		return fmt.Errorf("audit spool: read source id: %w", err)
	}
	if s.sourceID == "" {
		s.sourceID = uuid.NewString()
	}
	for k, val := range map[string]string{"schema_version": strconv.Itoa(SchemaVersion), "source_id": s.sourceID} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, val); err != nil {
			return fmt.Errorf("audit spool: write meta: %w", err)
		}
	}
	switch err := tx.QueryRowContext(ctx, `SELECT hash FROM event ORDER BY id DESC LIMIT 1`).Scan(&s.last); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("audit spool: read chain head: %w", err)
	}
	return tx.Commit()
}

// SourceID is the spool's identity, a UUID minted when the file was
// created: shippers key forwarded events by (SourceID, ID), so a new file
// (a replaced emptyDir) never collides with an earlier one of the same
// node.
func (s *Spool) SourceID() string { return s.sourceID }

// Node is the node the spool records for.
func (s *Spool) Node() string { return s.opts.Node }

// Kind names the source in a cluster-wide store.
func (s *Spool) Kind() string { return "auditspool" }

// Close closes the database.
func (s *Spool) Close() error { return s.db.Close() }

// Record implements audit.Recorder: it appends ev to the chain.
func (s *Spool) Record(ctx context.Context, ev audit.Event) error {
	if ev.Time.IsZero() {
		ev.Time = s.opts.Now()
	}
	ev.Time = time.UnixMicro(ev.Time.UnixMicro()).UTC()
	if ev.Node == "" {
		ev.Node = s.opts.Node
	}
	ev.ID = 0
	s.mu.Lock()
	defer s.mu.Unlock()
	audit.Chain(&ev, s.last)
	doc, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("audit spool: encode: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("audit spool: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `INSERT INTO event (ts, kind, name, actor, actor_name, doc, prev_hash, hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.Time.UnixMicro(), ev.Resource.Kind, ev.Resource.Name, ev.Actor.String(), ev.Actor.Name, string(doc), ev.PrevHash, ev.Hash)
	if err != nil {
		return fmt.Errorf("audit spool: insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	res, err = tx.ExecContext(ctx, `DELETE FROM event WHERE id <= ?`, id-int64(s.opts.Max))
	if err != nil {
		return fmt.Errorf("audit spool: cap: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("audit spool: commit: %w", err)
	}
	s.last = ev.Hash
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		s.dropped.Add(float64(n))
	}
	return nil
}

// Read implements audit.Reader.
func (s *Spool) Read(ctx context.Context, q audit.Query) ([]audit.Event, error) {
	if q.Cursor != "" {
		return nil, audit.ErrBadCursor
	}
	var where []string
	var args []any
	add := func(cond string, a ...any) {
		where = append(where, cond)
		args = append(args, a...)
	}
	if q.AfterID > 0 {
		add("id > ?", q.AfterID)
	}
	if q.BeforeID > 0 {
		add("id < ?", q.BeforeID)
	}
	if q.Kind != "" {
		add("kind = ?", q.Kind)
	}
	if q.Name != "" {
		add("name = ?", q.Name)
	}
	if q.Actor != "" {
		add("(actor = ? OR actor_name = ?)", q.Actor, q.Actor)
	}
	if !q.Since.IsZero() {
		add("ts >= ?", q.Since.UnixMicro())
	}
	order := "ASC"
	if q.Desc {
		order = "DESC"
	}
	stmt := `SELECT id, doc FROM event`
	if len(where) > 0 {
		stmt += " WHERE " + strings.Join(where, " AND ")
	}
	stmt += " ORDER BY id " + order + " LIMIT ?"
	args = append(args, clampLimit(q.Limit))
	return s.query(ctx, stmt, args...)
}

// ReadAfter returns up to n events with ids above after, oldest first:
// the shipping read.
func (s *Spool) ReadAfter(ctx context.Context, after int64, n int) ([]audit.Event, error) {
	if n <= 0 {
		n = 100
	}
	return s.query(ctx, `SELECT id, doc FROM event WHERE id > ? ORDER BY id LIMIT ?`, after, n)
}

func (s *Spool) query(ctx context.Context, stmt string, args ...any) ([]audit.Event, error) {
	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("audit spool: read: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []audit.Event{}
	for rows.Next() {
		var id int64
		var doc string
		if err := rows.Scan(&id, &doc); err != nil {
			return nil, fmt.Errorf("audit spool: read: %w", err)
		}
		var ev audit.Event
		if err := json.Unmarshal([]byte(doc), &ev); err != nil {
			return nil, fmt.Errorf("audit spool: decode event %d: %w", id, err)
		}
		ev.ID = id
		out = append(out, ev)
	}
	return out, rows.Err()
}

// Prune deletes events older than before and returns how many it deleted.
func (s *Spool) Prune(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM event WHERE ts < ?`, before.UnixMicro())
	if err != nil {
		return 0, fmt.Errorf("audit spool: prune: %w", err)
	}
	return res.RowsAffected()
}

// Verify checks the hash chain of every stored event (audit.VerifyChain);
// the oldest event may follow pruned or dropped ones.
func (s *Spool) Verify(ctx context.Context) error {
	var prev *audit.Event
	var after int64
	for {
		evs, err := s.ReadAfter(ctx, after, 1000)
		if err != nil {
			return err
		}
		if len(evs) == 0 {
			return nil
		}
		check := evs
		if prev != nil {
			check = append([]audit.Event{*prev}, evs...)
		}
		if err := audit.VerifyChain(check); err != nil {
			return err
		}
		last := evs[len(evs)-1]
		prev, after = &last, last.ID
	}
}

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return 100
	case n > 1000:
		return 1000
	}
	return n
}
