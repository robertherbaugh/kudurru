package sqlitelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// CentralOptions configures NewCentralReader.
type CentralOptions struct {
	// Now is the clock; default time.Now.
	Now func() time.Time
	// RawRetention is the central database's fact retention
	// (database.querylog.retention.raw); 0 means unbounded.
	RawRetention time.Duration
	// DomainsOnLists reports the summary's blocked-domain count.
	DomainsOnLists func() int64
	// Active is how recently a node must have shipped to count in the
	// answer and in the rollup progress (default 2 h).
	Active time.Duration
	// Lagging is how far a node's statistics may trail before the answer
	// is marked partial (default 5 min).
	Lagging time.Duration
}

// centralCap bounds the exact row counts of the central query log.
const centralCap = 100000

// central is the backend of a central reader.
type central struct {
	open func() (*sql.DB, error)
	opts CentralOptions

	node string // immutable exact historical node selector; empty selects the cluster
}

// NewCentralReader answers the stats and query-log questions for every
// node from the central database (internal/central, D161): its tables
// mirror querylog.db with central dictionary ids and a node on every row,
// so the same queries, run through the PostgreSQL dialect, sum over the
// nodes. open returns a database/sql handle on the pgx driver, or the
// error that says why the database cannot be used now. Rollups are
// complete up to the slowest active node; newer data comes from the fact
// table, as on a node.
func NewCentralReader(open func() (*sql.DB, error), opts CentralOptions) *Reader {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Active <= 0 {
		opts.Active = 2 * time.Hour
	}
	if opts.Lagging <= 0 {
		opts.Lagging = 5 * time.Minute
	}
	return &Reader{c: &central{open: open, opts: opts}, d: dialectPostgres}
}

// ErrUnknownNode means the central database has no retained query-log source
// registered for the requested node.
var ErrUnknownNode = errors.New("sqlitelog: unknown historical node")

// ErrCentralRead identifies an unavailable historical read without exposing database errors.
var ErrCentralRead = errors.New("sqlitelog: central history unavailable")

// ForNode derives an independent immutable central reader. A node's local
// reader cannot be reinterpreted as a central reader.
func (r *Reader) ForNode(node string) *Reader {
	if r.c == nil || node == "" {
		return nil
	}
	return &Reader{c: &central{open: r.c.open, opts: r.c.opts, node: node}, d: r.d}
}

func (c *central) domainsOnLists() func() int64 {
	if c == nil {
		return nil
	}
	return c.opts.DomainsOnLists
}

// read runs fn in a read-only, repeatable-read transaction with a
// snapshot of the cluster's rollup progress: the hours and days every
// active node has shipped.
func (c *central) read(ctx context.Context, fn func(tx *rtx, s *snapshot) error) (err error) {
	defer func() {
		if err != nil && !errors.Is(err, ErrUnknownNode) {
			err = fmt.Errorf("%w: %w", ErrCentralRead, err)
		}
	}()
	db, err := c.open()
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return fmt.Errorf("sqlitelog: begin central read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	t := &rtx{tx: tx, d: dialectPostgres}
	now := c.opts.Now()
	progressWhere := "s.last_seen >= ?"
	progressArgs := []any{now.Add(-c.opts.Active)}
	if c.node != "" {
		t.scoped, t.node = true, -1
		if err := t.QueryRowContext(ctx, `SELECT id FROM dict_node WHERE name=?`, c.node).Scan(&t.node); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// source.node can change when an existing file is renamed. Retained
		// facts/rollups still prove the old node existed; an orphan dictionary
		// name alone does not. Source registration also preserves empty history.
		args := []any{c.node, t.node}
		visible := t.dictionaryWhere("dict_node", "n.", &args)
		var known bool
		if err := t.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM source WHERE kind='querylog' AND node=?) OR EXISTS(SELECT 1 FROM dict_node n WHERE n.id=?`+visible+`)`, args...).Scan(&known); err != nil {
			return err
		}
		if !known {
			return ErrUnknownNode
		}
		progressWhere, progressArgs = "(s.node=? OR s.id IN (SELECT source FROM query WHERE node=?))", []any{c.node, t.node}

	}
	rows, err := t.QueryContext(ctx, `SELECT s.node, coalesce(h.last_id, 0), coalesce(d.last_id, 0), m.updated_at, s.last_seen
		FROM source s
		LEFT JOIN ship_cursor h ON h.source = s.id AND h.stream = 'rollup_1h'
		LEFT JOIN ship_cursor d ON d.source = s.id AND d.stream = 'rollup_1d'
		LEFT JOIN ship_cursor m ON m.source = s.id AND m.stream = 'rollup_1m'
		WHERE s.kind = 'querylog' AND `+progressWhere, progressArgs...)
	if err != nil {
		return fmt.Errorf("sqlitelog: central progress: %w", err)
	}
	s := &snapshot{now: now, hourDone: floorTo(now.Unix(), 3600), dayDone: floorTo(now.Unix(), 86400)}
	byNode := map[string]NodeResult{}
	first := true
	for rows.Next() {
		var node string
		var hour, day int64
		var minute sql.NullTime
		var seen time.Time
		if err := rows.Scan(&node, &hour, &day, &minute, &seen); err != nil {
			_ = rows.Close()
			return err
		}
		if first || hour < s.hourDone {
			s.hourDone = hour
		}
		if first || day < s.dayDone {
			s.dayDone = day
		}
		first = false
		if c.node != "" {
			node = c.node
		}
		nr := NodeResult{Name: node, OK: true}
		switch {
		case c.node != "" && seen.Before(now.Add(-c.opts.Active)):
			// Inactive sources describe retained history, not peer health.
		case !minute.Valid:
			nr.OK, nr.Error = false, "no statistics received yet"
		case now.Sub(minute.Time) > c.opts.Lagging:
			nr.OK, nr.Error = false, "statistics last received "+now.Sub(minute.Time).Round(time.Second).String()+" ago"
		}
		// A node with several sources (a replaced volume) counts once,
		// as its freshest.
		if prev, ok := byNode[node]; !ok || nr.OK && !prev.OK {
			byNode[node] = nr
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if c.node != "" && len(byNode) == 0 {
		byNode[c.node] = NodeResult{Name: c.node, OK: true}
	}
	nodes := make([]NodeResult, 0, len(byNode))
	partial := false
	for _, nr := range byNode {
		nodes = append(nodes, nr)
		partial = partial || !nr.OK
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	s.scope = Scope{Scope: ScopeCluster, Nodes: nodes, Partial: partial, Source: SourceCentral}
	if c.node != "" {
		s.scope.Scope = ScopeNode
	}
	return fn(t, s)
}

// status answers GET /querylog/status for the central database.
func (c *central) status(ctx context.Context, r *Reader) (*Status, error) {
	start := time.Now()
	res := &Status{Privacy: "", Retention: Retention{
		RawSeconds: int64(c.opts.RawRetention / time.Second),
		Rollup1m:   MinuteRetention.String(), Rollup1h: HourRetention.String(), Rollup1d: "forever",
	}}
	scope, err := r.readScoped(ctx, false, start, func(tx *rtx, _ *snapshot) error {
		args := []any{}
		where := tx.nodeWhere("", &args)
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM query WHERE 1=1`+where+` LIMIT ?) AS t`, append(args, centralCap+1)...).Scan(&res.Rows); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(pg_total_relation_size(c.oid)), 0)::bigint FROM pg_class c
			WHERE c.relnamespace = current_schema()::regnamespace AND (c.relname = 'query' OR c.relname LIKE 'query\_%' OR c.relname LIKE 'rollup\_%' OR c.relname LIKE 'dict\_%')`).Scan(&res.DBBytes); err != nil {
			return err
		}
		var oldest sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT min(ts) FROM query WHERE 1=1`+where, args...).Scan(&oldest); err != nil {
			return err
		}
		if oldest.Valid {
			t := time.UnixMicro(oldest.Int64).UTC()
			res.OldestTS = &t
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	res.Scope = scope
	return res, nil
}
