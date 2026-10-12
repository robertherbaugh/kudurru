package sqlitelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// The export side of querylog.db: what a shipper reads to copy this node's
// query log and statistics into a central database (D160). Rows carry
// dictionary values as names, never the file's ids, so the database can
// map them onto its own dictionaries.

// metaSourceID names this file as a shipping source.
const metaSourceID = "source_id"

// SourceID returns the UUID that names this file in a central database,
// minting it on first use. A new file (a replaced volume) gets a new one,
// so its row ids never collide with an earlier file's.
func (w *Writer) SourceID(ctx context.Context) (string, error) {
	var id string
	err := w.locked(func() error {
		err := w.w.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, metaSourceID).Scan(&id)
		if err == nil && id != "" {
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		id = uuid.NewString()
		return writeMeta(ctx, w.w, metaSourceID, id)
	})
	if err != nil {
		return "", fmt.Errorf("sqlitelog: source id: %w", err)
	}
	return id, nil
}

// ExportRow is one fact row with its dictionary values as names.
type ExportRow struct {
	ID         int64
	TS         int64 // unix µs
	LatencyUS  *int64
	Node       string
	Proto      int64
	ClientIP   string
	ClientName *string
	ClientGrp  *string
	Domain     string
	DomainETLD *string
	QType      int64
	QClass     int64
	Status     int64
	Rcode      int64
	ReplyType  int64
	Upstream   *string
	Zone       *string
	List       *string
	Rule       *string
	CNAME      *string
	DNSSEC     int64
	EDE        *int64
	Flags      int64
	ECS        *string
	TraceID    []byte
	Answer     *string
	Policy     *string
}

// Export returns up to limit fact rows with ids above after, oldest first.
func (w *Writer) Export(ctx context.Context, after int64, limit int) ([]ExportRow, error) {
	rows, err := w.r.QueryContext(ctx, `SELECT q.id, q.ts, q.latency_us, coalesce(n.name,''), q.proto, c.ip, c.name, c.grp,
		d.name, d.etld1, q.qtype, q.qclass, q.status, q.rcode, q.reply_type, u.addr, z.name, l.name, q.rule, cn.name,
		q.dnssec, q.ede, q.flags, q.ecs, q.trace_id, q.answer, p.name
		FROM query q
		LEFT JOIN dict_node n ON n.id=q.node JOIN dict_client c ON c.id=q.client JOIN dict_domain d ON d.id=q.domain
		LEFT JOIN dict_upstream u ON u.id=q.upstream LEFT JOIN dict_zone z ON z.id=q.zone LEFT JOIN dict_list l ON l.id=q.list
		LEFT JOIN dict_domain cn ON cn.id=q.cname LEFT JOIN dict_policy p ON p.id=q.policy
		WHERE q.id > ? ORDER BY q.id LIMIT ?`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlitelog: export: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ExportRow
	for rows.Next() {
		var r ExportRow
		var lat, ede sql.NullInt64
		var cname, cgrp, etld, up, zone, list, rule, cn, ecs, ans, pol sql.NullString
		if err := rows.Scan(&r.ID, &r.TS, &lat, &r.Node, &r.Proto, &r.ClientIP, &cname, &cgrp, &r.Domain, &etld,
			&r.QType, &r.QClass, &r.Status, &r.Rcode, &r.ReplyType, &up, &zone, &list, &rule, &cn,
			&r.DNSSEC, &ede, &r.Flags, &ecs, &r.TraceID, &ans, &pol); err != nil {
			return nil, fmt.Errorf("sqlitelog: export: %w", err)
		}
		r.LatencyUS, r.EDE = nullInt(lat), nullInt(ede)
		r.ClientName, r.ClientGrp, r.DomainETLD = nullStr(cname), nullStr(cgrp), nullStr(etld)
		r.Upstream, r.Zone, r.List, r.Rule, r.CNAME = nullStr(up), nullStr(zone), nullStr(list), nullStr(rule), nullStr(cn)
		r.ECS, r.Answer, r.Policy = nullStr(ecs), nullStr(ans), nullStr(pol)
		out = append(out, r)
	}
	return out, rows.Err()
}

func nullInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

func nullStr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	return &v.String
}

// RollupTable describes one rollup table for export: its name, its key
// dimension ("" for the base tables) and its value columns.
type RollupTable struct {
	Name string
	// Key is "", "client", "qtype", "upstream", "domain", "list" or
	// "zone": the column after (ts, node) in the primary key.
	Key  string
	Cols []string
	// Res is the bucket width in seconds (60, 3600, 86400).
	Res int64
}

// RollupTables lists every rollup table of the schema.
var RollupTables = []RollupTable{
	{"rollup_1m", "", baseCols, 60},
	{"rollup_client_1m", "client", []string{"total", "blocked"}, 60},
	{"rollup_qtype_1m", "qtype", []string{"total"}, 60},
	{"rollup_upstream_1m", "upstream", upCols, 60},
	{"rollup_1h", "", baseCols, 3600},
	{"rollup_client_1h", "client", []string{"total", "blocked"}, 3600},
	{"rollup_qtype_1h", "qtype", []string{"total"}, 3600},
	{"rollup_upstream_1h", "upstream", upCols, 3600},
	{"rollup_domain_1h", "domain", []string{"total", "blocked"}, 3600},
	{"rollup_list_1h", "list", []string{"hits"}, 3600},
	{"rollup_zone_1h", "zone", []string{"total"}, 3600},
	{"rollup_1d", "", baseCols, 86400},
	{"rollup_client_1d", "client", []string{"total", "blocked"}, 86400},
	{"rollup_domain_1d", "domain", []string{"total", "blocked"}, 86400},
}

// RollupRow is one rollup row with its key as names.
type RollupRow struct {
	TS   int64 // unix s
	Node string
	// Key is the dictionary value (a client's ip, a domain, an upstream's
	// address, a list or zone name) or the decimal qtype.
	Key string
	// ClientName and ClientGrp complete a client key.
	ClientName *string
	ClientGrp  *string
	// Values are t.Cols in order.
	Values []int64
}

// keyJoins resolves a rollup key column to names.
var keyJoins = map[string]struct{ join, cols string }{
	"":         {"", "'', NULL, NULL"},
	"client":   {"JOIN dict_client k ON k.id=r.client", "k.ip, k.name, k.grp"},
	"qtype":    {"", "CAST(r.qtype AS TEXT), NULL, NULL"},
	"upstream": {"JOIN dict_upstream k ON k.id=r.upstream", "k.addr, NULL, NULL"},
	"domain":   {"JOIN dict_domain k ON k.id=r.domain", "k.name, NULL, NULL"},
	"list":     {"JOIN dict_list k ON k.id=r.list", "k.name, NULL, NULL"},
	"zone":     {"JOIN dict_zone k ON k.id=r.zone", "k.name, NULL, NULL"},
}

// ExportRollup returns the rows of t with ts in [from, until).
func (w *Writer) ExportRollup(ctx context.Context, t RollupTable, from, until int64) ([]RollupRow, error) {
	kj, ok := keyJoins[t.Key]
	if !ok {
		return nil, fmt.Errorf("sqlitelog: unknown rollup key %q", t.Key)
	}
	vals := make([]string, len(t.Cols))
	for i, c := range t.Cols {
		vals[i] = "coalesce(r." + c + ",0)"
	}
	//nolint:gosec // identifiers are package constants; values are bound parameters
	q := `SELECT r.ts, coalesce(n.name,''), ` + kj.cols + `, ` + strings.Join(vals, ", ") + ` FROM ` + t.Name + ` r
		LEFT JOIN dict_node n ON n.id=r.node ` + kj.join + ` WHERE r.ts >= ? AND r.ts < ? ORDER BY r.ts`
	rows, err := w.r.QueryContext(ctx, q, from, until)
	if err != nil {
		return nil, fmt.Errorf("sqlitelog: export %s: %w", t.Name, err)
	}
	defer func() { _ = rows.Close() }()
	var out []RollupRow
	for rows.Next() {
		r := RollupRow{Values: make([]int64, len(t.Cols))}
		var name, grp sql.NullString
		dest := []any{&r.TS, &r.Node, &r.Key, &name, &grp}
		for i := range r.Values {
			dest = append(dest, &r.Values[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("sqlitelog: export %s: %w", t.Name, err)
		}
		r.ClientName, r.ClientGrp = nullStr(name), nullStr(grp)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RollupProgress returns the hour and day before which the _1h and _1d
// rollups are complete (unix s).
func (w *Writer) RollupProgress() (hourDone, dayDone int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.hourDone, w.dayDone
}

// MaxID returns the id of the newest fact row written.
func (w *Writer) MaxID() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.maxID
}

// SetShippedThrough makes age-based retention keep fact rows whose ids are
// above f() (the rows a shipper has not delivered yet). The size cap
// (maxDBBytes) still deletes them when the file grows too large. nil
// removes the guard.
func (w *Writer) SetShippedThrough(f func() int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.shippedThrough = f
}
