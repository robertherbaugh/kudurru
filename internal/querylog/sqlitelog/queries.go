package sqlitelog

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
)

// likePattern turns a * wildcard pattern into a LIKE pattern with \ escapes.
func likePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`, `*`, `%`)
	return r.Replace(s)
}

// statusesOf lists the statuses of a class.
func statusesOf(c pipeline.Class) []pipeline.Status {
	var out []pipeline.Status
	for s := pipeline.Status(0); s < 32; s++ {
		if s.String() != "unknown" || s == pipeline.StatusUnknown {
			if s.Class() == c {
				out = append(out, s)
			}
		}
	}
	return out
}

// filterSQL builds the WHERE clause of Queries (without the cursor).
func (r *Reader) filterSQL(ctx context.Context, tx *sql.Tx, f QueryFilter) ([]string, []any, error) {
	var where []string
	var args []any
	if c := strings.TrimSpace(f.Client); c != "" {
		if pfx, err := netip.ParsePrefix(c); err == nil {
			ids, err := r.clientsIn(ctx, tx, pfx.Masked())
			if err != nil {
				return nil, nil, err
			}
			where = append(where, `q.client IN (SELECT value FROM json_each(?))`)
			args = append(args, idsJSON(ids))
		} else if a, err := netip.ParseAddr(c); err == nil {
			where = append(where, `q.client IN (SELECT id FROM dict_client WHERE ip=?)`)
			args = append(args, a.Unmap().String())
		} else if strings.Contains(c, "*") {
			where = append(where, `q.client IN (SELECT id FROM dict_client WHERE name LIKE ? ESCAPE '\')`)
			args = append(args, likePattern(c))
		} else {
			where = append(where, `q.client IN (SELECT id FROM dict_client WHERE name=?)`)
			args = append(args, c)
		}
	}
	if d := strings.TrimSpace(f.Domain); d != "" {
		if strings.Contains(d, "*") {
			where = append(where, `q.domain IN (SELECT id FROM dict_domain WHERE name LIKE ? ESCAPE '\')`)
			args = append(args, likePattern(strings.TrimSuffix(strings.ToLower(d), ".")))
		} else {
			where = append(where, `q.domain = (SELECT id FROM dict_domain WHERE name=?)`)
			args = append(args, storedName(d))
		}
	}
	statuses := append([]pipeline.Status{}, f.Status...)
	if f.StatusClass != "" {
		cls := statusesOf(f.StatusClass)
		if len(cls) == 0 {
			return nil, nil, fmt.Errorf("sqlitelog: unknown status class %q", f.StatusClass)
		}
		statuses = append(statuses, cls...)
	}
	if len(statuses) > 0 {
		ph := make([]string, len(statuses))
		for i, s := range statuses {
			ph[i] = "?"
			args = append(args, int64(s))
		}
		where = append(where, `q.status IN (`+strings.Join(ph, ",")+`)`)
	}
	if f.QType != 0 {
		where = append(where, `q.qtype=?`)
		args = append(args, int64(f.QType))
	}
	for _, x := range []struct{ v, table, col, field string }{
		{f.Upstream, "dict_upstream", "addr", "upstream"},
		{f.List, "dict_list", "name", "list"},
		{f.Zone, "dict_zone", "name", "zone"},
		{f.Policy, "dict_policy", "name", "policy"},
	} {
		if x.v != "" {
			where = append(where, `q.`+x.field+` = (SELECT id FROM `+x.table+` WHERE `+x.col+`=?)`)
			args = append(args, x.v)
		}
	}
	if !f.Since.IsZero() {
		where = append(where, `q.ts>=?`)
		args = append(args, f.Since.UnixMicro())
	}
	if !f.Until.IsZero() {
		where = append(where, `q.ts<?`)
		args = append(args, f.Until.UnixMicro())
	}
	return where, args, nil
}

// clientsIn returns the dict_client ids whose address lies in pfx.
func (r *Reader) clientsIn(ctx context.Context, tx *sql.Tx, pfx netip.Prefix) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, ip FROM dict_client WHERE ip <> ''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := []int64{}
	for rows.Next() {
		var id int64
		var ip string
		if err := rows.Scan(&id, &ip); err != nil {
			return nil, err
		}
		if a, err := netip.ParseAddr(ip); err == nil && pfx.Contains(a.Unmap()) {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// Queries answers GET /queries: newest first, keyset-paginated on id
// (stable under live inserts), with records_total/filtered and the
// earliest timestamps for the date picker.
func (r *Reader) Queries(ctx context.Context, f QueryFilter) (*QueryPage, error) {
	start := time.Now()
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)
	var cursor, cursorTS int64
	order := "q.id DESC"
	if f.TimeOrder {
		order = "q.ts DESC, q.id DESC"
	}
	if f.Cursor != "" {
		v := f.Cursor
		if f.TimeOrder {
			parts := strings.Split(v, ":")
			if len(parts) != 2 {
				return nil, fmt.Errorf("sqlitelog: invalid cursor")
			}
			var err error
			cursorTS, err = strconv.ParseInt(parts[0], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("sqlitelog: invalid cursor")
			}
			v = parts[1]
		}
		c, err := strconv.ParseInt(v, 10, 64)
		if err != nil || c <= 0 {
			return nil, fmt.Errorf("sqlitelog: invalid cursor %q", f.Cursor)
		}
		cursor = c
	}
	var out *QueryPage
	err := r.read(ctx, false, func(tx *sql.Tx, _ *snapshot) error {
		where, args, err := r.filterSQL(ctx, tx, f)
		if err != nil {
			return err
		}
		res := &QueryPage{Queries: []QueryRow{}, RecordsTotal: r.w.rows.Load()}
		cond := ""
		if len(where) > 0 {
			cond = " WHERE " + strings.Join(where, " AND ")
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM query q`+cond, args...).Scan(&res.RecordsFiltered); err != nil {
				return fmt.Errorf("sqlitelog: count filtered: %w", err)
			}
		} else {
			res.RecordsFiltered = res.RecordsTotal
		}
		pageWhere := append([]string{}, where...)
		pageArgs := append([]any{}, args...)
		if cursor > 0 {
			if f.TimeOrder {
				pageWhere = append(pageWhere, `(q.ts, q.id)<(?, ?)`)
				pageArgs = append(pageArgs, cursorTS, cursor)
			} else {
				pageWhere = append(pageWhere, `q.id<?`)
				pageArgs = append(pageArgs, cursor)
			}
		}
		pcond := ""
		if len(pageWhere) > 0 {
			pcond = " WHERE " + strings.Join(pageWhere, " AND ")
		}
		// Ids first (index-only for the indexed filters), then the wide
		// rows and their dictionary joins for one page only.
		rows, err := tx.QueryContext(ctx, `SELECT q.id, q.ts, q.latency_us, coalesce(n.name,''), q.proto, coalesce(c.ip,''), coalesce(c.name,''), coalesce(c.grp,''),
			coalesce(d.name,''), coalesce(cn.name,''), q.qtype, q.qclass, q.status, q.rcode, q.reply_type, coalesce(u.addr,''), coalesce(z.name,''),
			coalesce(l.name,''), coalesce(q.rule,''), q.dnssec, q.ede, q.flags, coalesce(q.ecs,''), q.trace_id, coalesce(q.answer,''), coalesce(p.name,'')
			FROM (SELECT q.id FROM query q`+pcond+` ORDER BY `+order+` LIMIT ?) page JOIN query q ON q.id=page.id
			LEFT JOIN dict_node n ON n.id=q.node LEFT JOIN dict_client c ON c.id=q.client
			LEFT JOIN dict_domain d ON d.id=q.domain LEFT JOIN dict_domain cn ON cn.id=q.cname
			LEFT JOIN dict_upstream u ON u.id=q.upstream LEFT JOIN dict_zone z ON z.id=q.zone LEFT JOIN dict_list l ON l.id=q.list
			LEFT JOIN dict_policy p ON p.id=q.policy
			ORDER BY `+order, append(pageArgs, limit+1)...)
		if err != nil {
			return fmt.Errorf("sqlitelog: queries: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			row, err := scanQueryRow(rows)
			if err != nil {
				return err
			}
			res.Queries = append(res.Queries, row)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(res.Queries) > limit {
			res.Queries = res.Queries[:limit]
			res.CursorNext = RowCursor(res.Queries[limit-1], f.TimeOrder)
		}
		var oldest sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT min(ts) FROM query`).Scan(&oldest); err != nil {
			return err
		}
		if oldest.Valid {
			t := time.UnixMicro(oldest.Int64).UTC()
			res.EarliestTSDisk = &t
		}
		out = res
		return nil
	})
	if err != nil {
		return nil, err
	}
	if tail := r.w.opts.Tail; tail != nil {
		if ev := tail.Recent(0, 0); len(ev) > 0 {
			t := ev[0].Time.UTC()
			out.EarliestTSMemory = &t
		}
	}
	out.Scope = r.scope(start)
	return out, nil
}

func scanQueryRow(rows *sql.Rows) (QueryRow, error) {
	var (
		row                                           QueryRow
		id, ts, proto, qtype, qclass, st, rc, rt, sec int64
		flags                                         int64
		latency, ede                                  sql.NullInt64
		trace                                         []byte
	)
	if err := rows.Scan(&id, &ts, &latency, &row.Node, &proto, &row.Client.IP, &row.Client.Name, &row.Client.Group,
		&row.Domain, &row.CNAME, &qtype, &qclass, &st, &rc, &rt, &row.Upstream, &row.Zone,
		&row.List, &row.Rule, &sec, &ede, &flags, &row.ECS, &trace, &row.Reply.Answer, &row.Policy); err != nil {
		return row, err
	}
	status := pipeline.Status(st) //nolint:gosec // status column holds a uint8
	row.ID = uint64(id)           //nolint:gosec // rowids are positive
	row.TS = time.UnixMicro(ts).UTC()
	row.Proto = protoName(proto)
	row.QType = querylog.QTypeName(uint16(qtype)) //nolint:gosec // qtype column holds a uint16
	row.QClass = int(qclass)
	row.Status, row.StatusClass = status.String(), string(status.Class())
	row.Rcode = querylog.RcodeName(int(rc))
	row.Reply.Type = querylog.ReplyType(rt).String() //nolint:gosec // reply_type holds a uint8
	if latency.Valid {
		ms := float64(latency.Int64) / 1000
		row.Reply.TimeMS = &ms
	}
	row.DNSSEC = querylog.DNSSECState(sec).String() //nolint:gosec // dnssec holds a uint8
	if ede.Valid {
		v := int(ede.Int64)
		row.EDE = &v
	}
	row.Flags = querylog.FlagsFromBits(uint16(flags)) //nolint:gosec // flags fit in 16 bits
	if len(trace) > 0 {
		row.TraceID = hex.EncodeToString(trace)
	}
	return row, nil
}

// Suggestions answers GET /queries/suggestions: up to n dictionary values
// starting with prefix (case-sensitive byte prefix; names are stored lower
// case). Client suggestions match addresses and names.
func (r *Reader) Suggestions(ctx context.Context, field, prefix string, n int) (*Suggestions, error) {
	start := time.Now()
	if n <= 0 || n > maxSuggestion {
		n = maxSuggestion
	}
	var q string
	switch field {
	case FieldDomain:
		prefix = strings.ToLower(prefix)
		q = `SELECT name FROM dict_domain WHERE name>=?1 AND name<?2 ORDER BY name LIMIT ?3`
	case FieldClient:
		q = `SELECT v FROM (SELECT ip AS v FROM dict_client WHERE ip>=?1 AND ip<?2 UNION SELECT name FROM dict_client WHERE name>=?1 AND name<?2) ORDER BY v LIMIT ?3`
	case FieldUpstream:
		q = `SELECT addr FROM dict_upstream WHERE addr>=?1 AND addr<?2 ORDER BY addr LIMIT ?3`
	case FieldList:
		q = `SELECT name FROM dict_list WHERE name>=?1 AND name<?2 ORDER BY name LIMIT ?3`
	case FieldZone:
		q = `SELECT name FROM dict_zone WHERE name>=?1 AND name<?2 ORDER BY name LIMIT ?3`
	case FieldPolicy:
		q = `SELECT name FROM dict_policy WHERE name>=?1 AND name<?2 ORDER BY name LIMIT ?3`
	default:
		return nil, fmt.Errorf("sqlitelog: unknown suggestion field %q", field)
	}
	var out *Suggestions
	err := r.read(ctx, false, func(tx *sql.Tx, _ *snapshot) error {
		// "\xff" never occurs in UTF-8, so prefix+"\xff" bounds every
		// string that starts with prefix under the BINARY collation.
		rows, err := tx.QueryContext(ctx, q, prefix, prefix+"\xff", n)
		if err != nil {
			return fmt.Errorf("sqlitelog: suggestions: %w", err)
		}
		defer func() { _ = rows.Close() }()
		res := &Suggestions{Field: field, Values: []string{}}
		for rows.Next() {
			var v sql.NullString
			if err := rows.Scan(&v); err != nil {
				return err
			}
			if v.String != "" {
				res.Values = append(res.Values, v.String)
			}
		}
		out = res
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	out.Scope = r.scope(start)
	return out, nil
}

// Subscribe is the live tail (SSE /queries/stream): events after the
// privacy transform, fanned out from the collector. The channel closes when
// ctx ends; a subscriber that falls behind by more than the tail buffer
// loses events (counted in kudurru_querylog_dropped_total{stage="tail"}).
func (r *Reader) Subscribe(ctx context.Context) <-chan *querylog.Event {
	if r.w.opts.Tail == nil {
		ch := make(chan *querylog.Event)
		close(ch)
		return ch
	}
	return r.w.opts.Tail.Subscribe(ctx)
}

// Status answers GET /querylog/status.
func (r *Reader) Status(ctx context.Context) (*Status, error) {
	start := time.Now()
	w := r.w
	dbBytes, err := w.DBBytes(ctx)
	if err != nil {
		return nil, err
	}
	res := &Status{
		Rows: w.rows.Load(), DBBytes: dbBytes, WALBytes: w.WALBytes(), Privacy: w.privacy().String(),
		Retention: Retention{
			RawSeconds: int64(w.rawRetention() / time.Second), MaxDBBytes: w.maxDBBytes(),
			Rollup1m: MinuteRetention.String(), Rollup1h: HourRetention.String(), Rollup1d: "forever",
		},
		Writer: w.Stats(),
	}
	if w.opts.Tail != nil {
		res.Subscribers = w.opts.Tail.Subscribers()
	}
	err = r.read(ctx, false, func(tx *sql.Tx, _ *snapshot) error {
		var oldest sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT min(ts) FROM query`).Scan(&oldest); err != nil {
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
	res.Scope = r.scope(start)
	return res, nil
}
