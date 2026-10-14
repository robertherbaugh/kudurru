package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// AuditDiffCap is the largest before/after document stored in an audit
// row (§11.4). Larger documents are replaced by a JSON summary carrying
// their size, SHA-256 and a prefix.
const AuditDiffCap = 4096

// Audit results (§11.4).
const (
	AuditSuccess = "success"
	AuditDenied  = "denied"
	AuditError   = "error"
)

// AuditEntry is one row of the audit table.
type AuditEntry struct {
	// ID is assigned on insert; IDs only increase.
	ID   int64
	Time time.Time
	// Actor is the principal (from Options.ActorFromContext); it falls back
	// to Manager when the context names none.
	Actor string
	// Manager is the store manager of the write ("api", "file:<path>", ...).
	Manager string
	// Action is "<kind>.<verb>" in lower case, e.g. "dnszone.create";
	// verbs written by the driver are create, update, status, delete and
	// finalize.
	Action          string
	Key             store.Key
	ResourceVersion string
	Result          string
	Reason          string
	// Before and After are the stored documents (nil when absent), capped
	// at AuditDiffCap.
	Before json.RawMessage
	After  json.RawMessage
	// PrevHash and Hash chain the rows: Hash = SHA-256 over the row's
	// fields and PrevHash, PrevHash = Hash of the previous row.
	PrevHash string
	Hash     string
}

// AuditQuery filters ReadAudit. Zero fields do not filter.
type AuditQuery struct {
	Kind      store.Kind
	Namespace string
	Name      string
	// AfterID returns rows with ID > AfterID (pagination cursor).
	AfterID int64
	// BeforeID, when positive, returns rows with ID < BeforeID (the
	// cursor of a Desc page).
	BeforeID int64
	// Since returns rows at or after this time.
	Since time.Time
	// Limit bounds the result; default 100, maximum 1000.
	Limit int
	// Desc returns the newest rows first.
	Desc bool
}

// WithActor returns a context whose writes are audited as actor (for
// example "user:alice" or "token:ci"); it is store.WithActor.
func WithActor(ctx context.Context, actor string) context.Context {
	return store.WithActor(ctx, actor)
}

// ActorFrom returns the actor set by WithActor, or ""; it is
// store.ActorFrom.
func ActorFrom(ctx context.Context) string { return store.ActorFrom(ctx) }

// capDiff returns doc unchanged when it fits AuditDiffCap, else a JSON
// summary {"truncated":true,"bytes":N,"sha256":"...","prefix":"..."} that
// fits.
func capDiff(doc json.RawMessage) json.RawMessage {
	if len(doc) <= AuditDiffCap {
		return doc
	}
	sum := sha256.Sum256(doc)
	type summary struct {
		Truncated bool   `json:"truncated"`
		Bytes     int    `json:"bytes"`
		SHA256    string `json:"sha256"`
		Prefix    string `json:"prefix"`
	}
	n := AuditDiffCap - 256
	for {
		p := doc[:n]
		for len(p) > 0 && !utf8.Valid(p) {
			p = p[:len(p)-1]
		}
		b, err := json.Marshal(summary{Truncated: true, Bytes: len(doc), SHA256: hex.EncodeToString(sum[:]), Prefix: string(p)})
		if err == nil && len(b) <= AuditDiffCap {
			return b
		}
		n /= 2
	}
}

// Audit statements on the write path.
const (
	sqlLastAuditHash = `SELECT hash FROM audit ORDER BY id DESC LIMIT 1`
	sqlInsertAudit   = `INSERT INTO audit
		(ts, actor, manager, action, kind, namespace, name, resource_version, result, reason, before, after, prev_hash, hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
)

// auditHash computes the chain hash of e (whose PrevHash is set).
func auditHash(e *AuditEntry) string {
	h := sha256.New()
	for _, f := range []string{
		strconv.FormatInt(e.Time.UnixNano(), 10), e.Actor, e.Manager, e.Action,
		string(e.Key.Kind), e.Key.Namespace, e.Key.Name, e.ResourceVersion,
		e.Result, e.Reason, string(e.Before), string(e.After), e.PrevHash,
	} {
		h.Write([]byte(strconv.Itoa(len(f))))
		h.Write([]byte{':'})
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// insertAudit appends e to the chain inside tx, filling ID, PrevHash and
// Hash.
func insertAudit(ctx context.Context, tx txQueryer, e *AuditEntry) error {
	if e.Actor == "" {
		e.Actor = e.Manager
	}
	e.Before, e.After = capDiff(e.Before), capDiff(e.After)
	e.Time = time.Unix(0, e.Time.UnixNano()) // stored as UnixNano; drop the monotonic reading
	switch err := tx.QueryRowContext(ctx, sqlLastAuditHash).Scan(&e.PrevHash); {
	case errors.Is(err, sql.ErrNoRows):
		e.PrevHash = ""
	case err != nil:
		return fmt.Errorf("store/sqlite: audit chain: %w", err)
	}
	e.Hash = auditHash(e)
	res, err := tx.ExecContext(ctx, sqlInsertAudit,
		e.Time.UnixNano(), e.Actor, e.Manager, e.Action, string(e.Key.Kind), e.Key.Namespace, e.Key.Name,
		e.ResourceVersion, e.Result, e.Reason, nullJSON(e.Before), nullJSON(e.After), e.PrevHash, e.Hash)
	if err != nil {
		return fmt.Errorf("store/sqlite: write audit: %w", err)
	}
	e.ID, err = res.LastInsertId()
	return err
}

func nullJSON(b json.RawMessage) any {
	if b == nil {
		return nil
	}
	return string(b)
}

// RecordAudit appends an audit row that is not tied to a store write (a
// login, a denied request, a runtime action). Time defaults to now and
// Result to AuditSuccess. It returns the stored entry.
func (s *Store) RecordAudit(ctx context.Context, e AuditEntry) (AuditEntry, error) {
	if e.Action == "" {
		return AuditEntry{}, errors.New("store/sqlite: audit entry needs an Action")
	}
	if e.Time.IsZero() {
		e.Time = s.opts.Now()
	}
	if e.Result == "" {
		e.Result = AuditSuccess
	}
	if e.Actor == "" {
		e.Actor = s.opts.ActorFromContext(ctx)
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.isClosed() {
		return AuditEntry{}, store.ErrClosed
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return AuditEntry{}, fmt.Errorf("store/sqlite: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertAudit(ctx, cachedTx{tx: tx, c: s.ws}, &e); err != nil {
		return AuditEntry{}, err
	}
	if err := tx.Commit(); err != nil {
		return AuditEntry{}, fmt.Errorf("store/sqlite: commit audit: %w", err)
	}
	return e, nil
}

// ReadAudit returns audit rows matching q in ID order (descending with
// q.Desc).
func (s *Store) ReadAudit(ctx context.Context, q AuditQuery) ([]AuditEntry, error) {
	if s.isClosed() {
		return nil, store.ErrClosed
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	var b strings.Builder
	b.WriteString(`SELECT id, ts, actor, manager, action, kind, namespace, name, resource_version, result, reason, before, after, prev_hash, hash FROM audit WHERE id > ?`)
	args := []any{q.AfterID}
	if q.BeforeID > 0 {
		b.WriteString(` AND id < ?`)
		args = append(args, q.BeforeID)
	}
	if q.Kind != "" {
		b.WriteString(` AND kind = ?`)
		args = append(args, string(q.Kind))
	}
	if q.Namespace != "" {
		b.WriteString(` AND namespace = ?`)
		args = append(args, q.Namespace)
	}
	if q.Name != "" {
		b.WriteString(` AND name = ?`)
		args = append(args, q.Name)
	}
	if !q.Since.IsZero() {
		b.WriteString(` AND ts >= ?`)
		args = append(args, q.Since.UnixNano())
	}
	if q.Desc {
		b.WriteString(` ORDER BY id DESC LIMIT ?`)
	} else {
		b.WriteString(` ORDER BY id LIMIT ?`)
	}
	args = append(args, limit)
	return s.queryAudit(ctx, b.String(), args...)
}

func (s *Store) queryAudit(ctx context.Context, query string, args ...any) ([]AuditEntry, error) {
	rows, err := s.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store/sqlite: read audit: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts int64
		var kind string
		var before, after sql.NullString
		if err := rows.Scan(&e.ID, &ts, &e.Actor, &e.Manager, &e.Action, &kind, &e.Key.Namespace, &e.Key.Name,
			&e.ResourceVersion, &e.Result, &e.Reason, &before, &after, &e.PrevHash, &e.Hash); err != nil {
			return nil, fmt.Errorf("store/sqlite: read audit: %w", err)
		}
		e.Time = time.Unix(0, ts)
		e.Key.Kind = store.Kind(kind)
		if before.Valid {
			e.Before = json.RawMessage(before.String)
		}
		if after.Valid {
			e.After = json.RawMessage(after.String)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneAudit deletes audit rows older than before (the retention job of
// §11.4, 400 days by default, runs it daily) and returns how many rows it
// removed. The chain stays verifiable: VerifyAudit accepts an oldest row
// whose predecessor was pruned.
func (s *Store) PruneAudit(ctx context.Context, before time.Time) (int64, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.isClosed() {
		return 0, store.ErrClosed
	}
	res, err := s.w.ExecContext(ctx, `DELETE FROM audit WHERE ts < ?`, before.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("store/sqlite: prune audit: %w", err)
	}
	return res.RowsAffected()
}

// VerifyAudit walks the whole chain and returns an error naming the first
// row whose hash does not match its contents or whose PrevHash does not
// match the previous row.
func (s *Store) VerifyAudit(ctx context.Context) error {
	prev := ""
	first := true
	var after int64
	for {
		page, err := s.ReadAudit(ctx, AuditQuery{AfterID: after, Limit: 1000})
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for i := range page {
			e := &page[i]
			// The oldest retained row may point at a pruned predecessor.
			if !first && e.PrevHash != prev {
				return fmt.Errorf("%w: store/sqlite: audit row %d does not chain to row before it", store.ErrAuditIntegrity, e.ID)
			}
			if auditHash(e) != e.Hash {
				return fmt.Errorf("%w: store/sqlite: audit row %d was modified", store.ErrAuditIntegrity, e.ID)
			}
			prev, first, after = e.Hash, false, e.ID
		}
	}
}

// metaAuditSource is the meta key of AuditSourceID.
const metaAuditSource = "audit_source_id"

// AuditSourceID returns the UUID that names this file's audit rows when
// they are shipped to a cluster-wide audit store (audit.Source), minting
// it on first use. It is not cluster_id: a cluster-wide store keys rows by
// (source, id), and this names the file, not the cluster.
func (s *Store) AuditSourceID(ctx context.Context) (string, error) {
	var id string
	err := s.r.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaAuditSource).Scan(&id)
	if err == nil && id != "" {
		return id, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("store/sqlite: read %s: %w", metaAuditSource, err)
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.isClosed() {
		return "", store.ErrClosed
	}
	if _, err := s.w.ExecContext(ctx, `INSERT OR IGNORE INTO meta(key, value) VALUES (?, ?)`, metaAuditSource, uuid.NewString()); err != nil {
		return "", fmt.Errorf("store/sqlite: write %s: %w", metaAuditSource, err)
	}
	if err := s.w.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaAuditSource).Scan(&id); err != nil {
		return "", fmt.Errorf("store/sqlite: read %s: %w", metaAuditSource, err)
	}
	return id, nil
}
