// Package pgaudit keeps the audit log of every node in the central
// database (internal/central, D159): one table, one hash chain across all
// nodes, read the same by every replica.
//
// Nodes ship their local audit store (the kube driver's spool, the sqlite
// driver's state.db) with audit.Shipper; Target appends each batch under
// the chain-head lock, so ids are gapless and in commit order and the
// keyset cursor of GET /audit stays a plain id. Verify recomputes the
// chain; PruneJob deletes a prefix of it and records where it ends.
package pgaudit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/central"
)

// stream is the ship_cursor stream of audit events.
const stream = "audit"

// Target appends shipped events to the central chain; it implements
// audit.Target.
type Target struct {
	DB *central.DB
	// Version is recorded with the source (the binary's version).
	Version string
}

var _ audit.Target = (*Target)(nil)

// Cursor implements audit.Target.
func (t *Target) Cursor(ctx context.Context, src audit.Source) (int64, error) {
	pool, err := t.DB.Pool()
	if err != nil {
		return 0, err
	}
	return central.Cursor(ctx, pool, src.SourceID(), stream)
}

// sourceKind is the kind a source reports (audit spool, state.db).
func sourceKind(src audit.Source) string {
	if k, ok := src.(interface{ Kind() string }); ok {
		return k.Kind()
	}
	return "audit"
}

// Append implements audit.Target: in one transaction it registers the
// source, locks the chain head, chains and inserts the events after the
// source's cursor and moves the cursor. Events are sanitized (canonical);
// should the database still refuse a batch for its data (SQLSTATE class
// 22), it is stored once more without the events' free text (documents,
// reason, request), with a reason saying so, rather than holding every
// later event of the source behind it forever.
func (t *Target) Append(ctx context.Context, src audit.Source, evs []audit.Event) error {
	err := t.append(ctx, src, evs, "")
	var pe *pgconn.PgError
	if errors.As(err, &pe) && strings.HasPrefix(pe.Code, "22") {
		return t.append(ctx, src, evs, "stored without details: the database refused the event ("+pe.Code+")")
	}
	return err
}

// append inserts evs; stub, when set, replaces their free text.
func (t *Target) append(ctx context.Context, src audit.Source, evs []audit.Event, stub string) error {
	return t.DB.Tx(ctx, func(tx pgx.Tx) error {
		sid, err := central.EnsureSource(ctx, tx, src.SourceID(), sourceKind(src), src.Node(), t.Version)
		if err != nil {
			return err
		}
		var seq int64
		var head string
		if err := tx.QueryRow(ctx, `SELECT seq, hash FROM audit_chain FOR UPDATE`).Scan(&seq, &head); err != nil {
			return fmt.Errorf("pgaudit: lock the chain head: %w", err)
		}
		// Under the lock no other shipper of this source can move its
		// cursor: skip what an earlier attempt already stored.
		after, err := central.Cursor(ctx, tx, src.SourceID(), stream)
		if err != nil {
			return err
		}
		var rows [][]any
		var last int64
		for _, ev := range evs {
			if ev.ID <= after {
				continue
			}
			seq++
			c := canonical(ev, src.Node())
			if stub != "" {
				c.Before, c.After, c.Request, c.Reason = nil, nil, audit.Request{}, stub
			}
			c.PrevHash, c.Hash = head, audit.ChainHash(c, head)
			head = c.Hash
			last = ev.ID
			rows = append(rows, []any{
				seq, c.Time, c.Node, sid, ev.ID, c.Actor.String(), c.Actor.Kind, c.Actor.ID, c.Actor.Name, nonNil(c.Actor.Groups), c.Actor.Auth,
				c.Action, c.Resource.Kind, c.Resource.Namespace, c.Resource.Name, c.Result, c.Reason, c.Authz, c.Manager, c.ResourceVersion,
				text(c.Before), text(c.After), c.Request.ID, c.Request.IP, c.Request.UA, c.Request.Method, c.Request.Path,
				ev.PrevHash, ev.Hash, c.PrevHash, c.Hash,
			})
		}
		if len(rows) == 0 {
			return nil
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"audit"}, columns, pgx.CopyFromRows(rows)); err != nil {
			return fmt.Errorf("pgaudit: insert %d events: %w", len(rows), err)
		}
		if _, err := tx.Exec(ctx, `UPDATE audit_chain SET seq = $1, hash = $2`, seq, head); err != nil {
			return fmt.Errorf("pgaudit: move the chain head: %w", err)
		}
		return central.SetCursor(ctx, tx, sid, stream, last)
	})
}

var columns = []string{
	"id", "ts", "node", "source", "source_id", "actor", "actor_kind", "actor_id", "actor_name", "actor_groups", "actor_auth",
	"action", "kind", "namespace", "name", "result", "reason", "authz", "manager", "resource_version",
	"before", "after", "req_id", "req_ip", "req_ua", "req_method", "req_path",
	"origin_prev_hash", "origin_hash", "prev_hash", "hash",
}

// canonical is ev as the database stores it: sanitized text, microsecond
// UTC time, the source's node when the event names none, no ids or hashes.
func canonical(ev audit.Event, node string) audit.Event {
	// Text columns take valid UTF-8 without NUL: a state.db row recorded
	// before the API sanitized its requests may hold either.
	c := audit.Sanitize(ev)
	c.ID, c.UID, c.Cursor, c.Hash, c.PrevHash = 0, "", "", "", ""
	c.Time = time.UnixMicro(ev.Time.UnixMicro()).UTC()
	if c.Node == "" {
		c.Node = node
	}
	if len(c.Actor.Groups) == 0 {
		c.Actor.Groups = nil
	}
	return c
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func text(b json.RawMessage) *string {
	if b == nil {
		return nil
	}
	s := string(b)
	return &s
}

// Reader reads the central audit log; it implements audit.Reader. Event
// ids are the chain positions, so the API's id cursor works unchanged.
type Reader struct {
	DB *central.DB
}

var _ audit.Reader = (*Reader)(nil)

// Verify checks the retained central chain and its stored anchor and head.
func (r *Reader) Verify(ctx context.Context) error { return Verify(ctx, r.DB) }

// Read implements audit.Reader.
func (r *Reader) Read(ctx context.Context, q audit.Query) ([]audit.Event, error) {
	if q.Cursor != "" {
		return nil, audit.ErrBadCursor
	}
	pool, err := r.DB.Pool()
	if err != nil {
		return nil, err
	}
	var where []string
	var args []any
	add := func(cond string, v ...any) {
		for _, a := range v {
			args = append(args, a)
			cond = strings.Replace(cond, "?", "$"+strconv.Itoa(len(args)), 1)
		}
		where = append(where, cond)
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
		add("ts >= ?", q.Since)
	}
	stmt := `SELECT ` + strings.Join(readColumns, ", ") + ` FROM audit`
	if len(where) > 0 {
		stmt += " WHERE " + strings.Join(where, " AND ")
	}
	order := "ASC"
	if q.Desc {
		order = "DESC"
	}
	args = append(args, clampLimit(q.Limit))
	stmt += " ORDER BY id " + order + " LIMIT $" + strconv.Itoa(len(args))
	return query(ctx, pool, stmt, args...)
}

var readColumns = []string{
	"id", "ts", "node", "actor_kind", "actor_id", "actor_name", "actor_groups", "actor_auth",
	"action", "kind", "namespace", "name", "result", "reason", "authz", "manager", "resource_version",
	"before", "after", "req_id", "req_ip", "req_ua", "req_method", "req_path", "prev_hash", "hash",
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func query(ctx context.Context, q querier, stmt string, args ...any) ([]audit.Event, error) {
	rows, err := q.Query(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("pgaudit: read: %w", err)
	}
	defer rows.Close()
	out := []audit.Event{}
	for rows.Next() {
		var ev audit.Event
		var before, after *string
		if err := rows.Scan(&ev.ID, &ev.Time, &ev.Node, &ev.Actor.Kind, &ev.Actor.ID, &ev.Actor.Name, &ev.Actor.Groups, &ev.Actor.Auth,
			&ev.Action, &ev.Resource.Kind, &ev.Resource.Namespace, &ev.Resource.Name, &ev.Result, &ev.Reason, &ev.Authz, &ev.Manager,
			&ev.ResourceVersion, &before, &after, &ev.Request.ID, &ev.Request.IP, &ev.Request.UA, &ev.Request.Method, &ev.Request.Path,
			&ev.PrevHash, &ev.Hash); err != nil {
			return nil, fmt.Errorf("pgaudit: read: %w", err)
		}
		ev.Time = ev.Time.UTC()
		if len(ev.Actor.Groups) == 0 {
			ev.Actor.Groups = nil
		}
		if before != nil {
			ev.Before = json.RawMessage(*before)
		}
		if after != nil {
			ev.After = json.RawMessage(*after)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
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

// Verify recomputes the chain from its pruned anchor to the head and
// names the first event that does not verify. It reads in one read-only,
// repeatable-read transaction, so appends and prunes running meanwhile
// are not mistaken for gaps or a truncated end.
func Verify(ctx context.Context, db *central.DB) error {
	pool, err := db.Pool()
	if err != nil {
		return err
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := central.NoStatementTimeout(ctx, tx); err != nil {
		return err
	}
	var through, seq int64
	var anchor, head string
	if err := tx.QueryRow(ctx, `SELECT pruned_through, pruned_hash, seq, hash FROM audit_chain`).Scan(&through, &anchor, &seq, &head); err != nil {
		return fmt.Errorf("pgaudit: read the chain head: %w", err)
	}
	prevID, prev := through, anchor
	for {
		evs, err := query(ctx, tx, `SELECT `+strings.Join(readColumns, ", ")+` FROM audit WHERE id > $1 ORDER BY id LIMIT 1000`, prevID)
		if err != nil {
			return err
		}
		for _, ev := range evs {
			switch {
			case ev.ID != prevID+1:
				return fmt.Errorf("pgaudit: events %d to %d are missing", prevID+1, ev.ID-1)
			case ev.PrevHash != prev:
				return fmt.Errorf("pgaudit: event %d does not follow event %d", ev.ID, prevID)
			}
			if want := audit.ChainHash(canonical(ev, ev.Node), ev.PrevHash); want != ev.Hash {
				return fmt.Errorf("pgaudit: event %d was changed", ev.ID)
			}
			prevID, prev = ev.ID, ev.Hash
		}
		if len(evs) < 1000 {
			break
		}
	}
	if prevID != seq || prev != head {
		return fmt.Errorf("pgaudit: the chain ends at event %d but its head names event %d (events at the end are missing or changed)", prevID, seq)
	}
	return nil
}

// PruneJob deletes the oldest events of the chain older than retention:
// the longest prefix (by id) whose events are all older, so the chain
// stays verifiable from its new anchor even when node clocks disagree.
func PruneJob(retention time.Duration) central.Job {
	return central.Job{Name: "audit-retention", Run: func(ctx context.Context, tx pgx.Tx, now time.Time) (int64, error) {
		cutoff := now.Add(-retention)
		if _, err := tx.Exec(ctx, `SELECT 1 FROM audit_chain FOR UPDATE`); err != nil {
			return 0, err
		}
		var through int64
		var hash string
		err := tx.QueryRow(ctx, `SELECT id, hash FROM audit
			WHERE id < coalesce((SELECT min(id) FROM audit WHERE ts >= $1), (SELECT seq + 1 FROM audit_chain))
			ORDER BY id DESC LIMIT 1`, cutoff).Scan(&through, &hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		res, err := tx.Exec(ctx, `DELETE FROM audit WHERE id <= $1`, through)
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `UPDATE audit_chain SET pruned_through = $1, pruned_hash = $2 WHERE pruned_through < $1`, through, hash); err != nil {
			return 0, err
		}
		return res.RowsAffected(), nil
	}}
}
