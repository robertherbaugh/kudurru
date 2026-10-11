package sqlitelog

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Reader answers the §10 stats and log queries for one node. Reads use a
// separate query_only connection pool and never block ingest; results
// include the writer's not-yet-flushed accumulators, so they are current
// to the last committed batch. All methods are safe for concurrent use.
type Reader struct {
	w *Writer
}

// Read limits.
const (
	DefaultTopN   = 10
	MaxTopN       = 100
	DefaultLimit  = 100
	MaxLimit      = 1000
	maxPoints     = 300
	maxSuggestion = 100
)

// snapshot is a consistent copy of the writer state a read needs.
type snapshot struct {
	now      time.Time
	hourDone int64
	dayDone  int64
	minutes  map[int64]*minuteAcc
	hours    map[int64]*hourAcc
}

// read runs fn in a read transaction with a snapshot of the pending
// accumulators. A rollup flush between the snapshot and the transaction
// would count the flushed minutes twice, so the read is retried when the
// flush sequence moved (at most twice; flushes happen once a minute).
func (r *Reader) read(ctx context.Context, withHours bool, fn func(tx *sql.Tx, s *snapshot) error) error {
	w := r.w
	w.readers.Add(1)
	defer w.readers.Add(-1)
	for attempt := 0; ; attempt++ {
		w.mu.Lock()
		if w.closed {
			w.mu.Unlock()
			return ErrClosed
		}
		seq := w.flushSeq.Load()
		s := &snapshot{now: w.clock.Now(), hourDone: w.hourDone, dayDone: w.dayDone, minutes: make(map[int64]*minuteAcc, len(w.acc.minutes))}
		for ts, m := range w.acc.minutes {
			c := newMinuteAcc()
			c.merge(m)
			s.minutes[ts] = c
		}
		if withHours {
			s.hours = make(map[int64]*hourAcc, len(w.acc.hours))
			for ts, h := range w.acc.hours {
				c := newHourAcc()
				c.merge(h)
				s.hours[ts] = c
			}
		}
		w.mu.Unlock()

		tx, err := w.r.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlitelog: begin read: %w", err)
		}
		err = fn(tx, s)
		_ = tx.Rollback()
		if err != nil {
			return err
		}
		if w.flushSeq.Load() == seq || attempt >= 2 {
			return nil
		}
	}
}

func (r *Reader) scope(start time.Time) Scope {
	return Scope{Scope: ScopeNode, Nodes: []NodeResult{{
		Name: r.w.opts.Node, OK: true, TookMS: float64(time.Since(start).Microseconds()) / 1000,
	}}}
}

// window applies the defaults: the last 24 hours ending now.
func (r *Reader) window(w Window) Window {
	if w.Until.IsZero() {
		w.Until = r.w.clock.Now()
	}
	if w.From.IsZero() {
		w.From = w.Until.Add(-24 * time.Hour)
	}
	return w
}

// tier is one resolution of a dimension.
type tier struct {
	table string
	res   int64 // bucket width in seconds; 1 for the fact table
	done  int64 // complete for buckets starting before done
	keep  time.Duration
	facts bool
}

type segment struct {
	t           tier
	from, until int64 // unix s
}

const forever = math.MaxInt64

// cover splits [from, until) across tiers, coarse to fine: each tier takes
// the aligned middle it has complete data for and hands the ragged edges to
// the next finer tier.
func cover(from, until int64, tiers []tier) []segment {
	if from >= until || len(tiers) == 0 {
		return nil
	}
	if len(tiers) == 1 {
		return []segment{{tiers[0], from, until}}
	}
	t := tiers[0]
	a := ceilTo(from, t.res)
	b := min(floorTo(until, t.res), floorTo(t.done, t.res))
	if a >= b {
		return cover(from, until, tiers[1:])
	}
	out := cover(from, a, tiers[1:])
	out = append(out, segment{t, a, b})
	return append(out, cover(b, until, tiers[1:])...)
}

func ceilTo(v, step int64) int64 {
	f := floorTo(v, step)
	if f == v {
		return v
	}
	return f + step
}

// approximate reports whether a segment reaches past its table's retention.
func approximate(segs []segment, now time.Time) bool {
	for _, s := range segs {
		if s.t.keep > 0 && s.from < now.Add(-s.t.keep).Unix() {
			return true
		}
	}
	return false
}

func bucketExpr(step int64, facts bool) string {
	if step <= 0 {
		return "0"
	}
	if facts {
		return fmt.Sprintf("(ts/1000000/%d)*%d", step, step)
	}
	return fmt.Sprintf("(ts/%d)*%d", step, step)
}

func bucketOf(ts, step int64) int64 {
	if step <= 0 {
		return 0
	}
	return floorTo(ts, step)
}

func (r *Reader) baseTiers(s *snapshot, step int64) []tier {
	all := []tier{
		{table: "rollup_1d", res: 86400, done: s.dayDone},
		{table: "rollup_1h", res: 3600, done: s.hourDone, keep: HourRetention},
		{table: "rollup_1m", res: 60, done: forever, keep: MinuteRetention},
	}
	return stepTiers(all, step)
}

// stepTiers keeps the tiers whose resolution divides step (the finest tier
// always stays).
func stepTiers(all []tier, step int64) []tier {
	if step <= 0 {
		return all
	}
	var out []tier
	for i, t := range all {
		if step%t.res == 0 || i == len(all)-1 {
			out = append(out, t)
		}
	}
	return out
}

// baseSeries sums the rollup_1m-shaped columns per bucket over the window.
func (r *Reader) baseSeries(ctx context.Context, tx *sql.Tx, s *snapshot, from, until, step int64) (map[int64]*[nBase]int64, bool, error) {
	from = floorTo(from, 60)
	segs := cover(from, until, r.baseTiers(s, step))
	out := map[int64]*[nBase]int64{}
	get := func(b int64) *[nBase]int64 {
		v := out[b]
		if v == nil {
			v = new([nBase]int64)
			out[b] = v
		}
		return v
	}
	if len(segs) > 0 {
		var parts []string
		var args []any
		for _, sg := range segs {
			parts = append(parts, `SELECT `+bucketExpr(step, false)+` AS bk, `+strings.Join(baseCols, ",")+` FROM `+sg.t.table+` WHERE ts>=? AND ts<?`)
			args = append(args, sg.from, sg.until)
		}
		q := `SELECT bk, ` + sumCols(baseCols) + ` FROM (` + strings.Join(parts, " UNION ALL ") + `) GROUP BY bk` //nolint:gosec // identifiers and expressions are package constants; values are bound parameters
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, false, fmt.Errorf("sqlitelog: base series: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var bk int64
			vals := make([]sql.NullInt64, nBase)
			dest := []any{&bk}
			for i := range vals {
				dest = append(dest, &vals[i])
			}
			if err := rows.Scan(dest...); err != nil {
				return nil, false, err
			}
			v := get(bk)
			for i := range vals {
				v[i] += vals[i].Int64
			}
		}
		if err := rows.Err(); err != nil {
			return nil, false, err
		}
	}
	for ts, m := range s.minutes {
		if ts >= from && ts < until {
			v := get(bucketOf(ts, step))
			for i := range v {
				v[i] += m.base[i]
			}
		}
	}
	return out, approximate(segs, s.now), nil
}

// keyedSource describes one keyed dimension at every resolution.
type keyedSource struct {
	tiers []tier
	// rollup expressions
	key, a, b string
	// fact-table expressions and filter
	fKey, fA, fB, fWhere string
	// pending contributions
	minute func(m *minuteAcc, emit func(k, a, b int64))
	hour   func(h *hourAcc, emit func(k, a, b int64))
}

type keyedRow struct{ bk, k, a, b int64 }

// keyed aggregates a dimension over the window: one SQL statement over the
// tier segments plus the pending accumulators (passed as JSON), grouped by
// (bucket, key), filtered by having, ordered and limited in SQLite.
func (r *Reader) keyed(ctx context.Context, tx *sql.Tx, s *snapshot, src keyedSource, from, until, step int64, having, order string, limit int, countOnly bool) ([]keyedRow, int64, bool, error) {
	if src.minute != nil {
		from = floorTo(from, 60)
	}
	segs := cover(from, until, stepTiers(src.tiers, step))
	var parts []string
	var args []any
	pending := pendingJSON{}
	nowS := s.now.Unix()
	for _, sg := range segs {
		if sg.t.facts {
			if src.hour != nil && s.hours != nil && sg.from >= s.hourDone && sg.from%3600 == 0 && sg.until >= nowS {
				for h, acc := range s.hours {
					if h >= sg.from && h < sg.until {
						src.hour(acc, func(k, a, b int64) { pending.add(bucketOf(h, step), k, a, b) })
					}
				}
				continue
			}
			where := ""
			if src.fWhere != "" {
				where = " AND " + src.fWhere
			}
			parts = append(parts, `SELECT `+bucketExpr(step, true)+` AS bk, `+src.fKey+` AS k, `+src.fA+` AS a, `+src.fB+` AS b FROM query WHERE ts>=? AND ts<?`+where)
			args = append(args, sg.from*1e6, sg.until*1e6)
			continue
		}
		parts = append(parts, `SELECT `+bucketExpr(step, false)+` AS bk, `+src.key+` AS k, `+src.a+` AS a, `+src.b+` AS b FROM `+sg.t.table+` WHERE ts>=? AND ts<?`)
		args = append(args, sg.from, sg.until)
	}
	if src.minute != nil {
		for ts, m := range s.minutes {
			if ts >= from && ts < until {
				src.minute(m, func(k, a, b int64) { pending.add(bucketOf(ts, step), k, a, b) })
			}
		}
	}
	if pending.n > 0 {
		parts = append(parts, `SELECT value->>0, value->>1, value->>2, value->>3 FROM json_each(?)`)
		args = append(args, pending.String())
	}
	if len(parts) == 0 {
		return nil, 0, approximate(segs, s.now), nil
	}
	q := `SELECT bk, k, sum(a) AS a, sum(b) AS b FROM (` + strings.Join(parts, " UNION ALL ") + `) GROUP BY bk, k` //nolint:gosec // identifiers and expressions are package constants; values are bound parameters
	if having != "" {
		q += ` HAVING ` + having
	}
	if countOnly {
		var n int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM (`+q+`)`, args...).Scan(&n); err != nil {
			return nil, 0, false, fmt.Errorf("sqlitelog: keyed count: %w", err)
		}
		return nil, n, approximate(segs, s.now), nil
	}
	if order != "" {
		q += ` ORDER BY ` + order
	}
	if limit > 0 {
		q += ` LIMIT ` + strconv.Itoa(limit)
	}
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, false, fmt.Errorf("sqlitelog: keyed: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []keyedRow
	for rows.Next() {
		var kr keyedRow
		var k sql.NullInt64
		if err := rows.Scan(&kr.bk, &k, &kr.a, &kr.b); err != nil {
			return nil, 0, false, err
		}
		kr.k = k.Int64
		out = append(out, kr)
	}
	return out, int64(len(out)), approximate(segs, s.now), rows.Err()
}

// pendingJSON builds [[bk,k,a,b],...] for json_each.
type pendingJSON struct {
	b []byte
	n int
}

func (p *pendingJSON) add(bk, k, a, b int64) {
	if p.n == 0 {
		p.b = append(p.b, '[')
	} else {
		p.b = append(p.b, ',')
	}
	p.n++
	p.b = append(p.b, '[')
	p.b = strconv.AppendInt(p.b, bk, 10)
	p.b = append(p.b, ',')
	p.b = strconv.AppendInt(p.b, k, 10)
	p.b = append(p.b, ',')
	p.b = strconv.AppendInt(p.b, a, 10)
	p.b = append(p.b, ',')
	p.b = strconv.AppendInt(p.b, b, 10)
	p.b = append(p.b, ']')
}

func (p *pendingJSON) String() string {
	if p.n == 0 {
		return "[]"
	}
	return string(p.b) + "]"
}

// Dimension sources.

func (r *Reader) clientSource(s *snapshot) keyedSource {
	return keyedSource{
		tiers: []tier{
			{table: "rollup_client_1d", res: 86400, done: s.dayDone},
			{table: "rollup_client_1h", res: 3600, done: s.hourDone, keep: HourRetention},
			{table: "rollup_client_1m", res: 60, done: forever, keep: MinuteRetention},
		},
		key: "client", a: "total", b: "blocked",
		minute: func(m *minuteAcc, emit func(k, a, b int64)) {
			for c, v := range m.clients {
				emit(c, v[0], v[1])
			}
		},
	}
}

func (r *Reader) factTier() tier {
	return tier{table: "query", res: 1, done: forever, keep: r.w.opts.RawRetention, facts: true}
}

func (r *Reader) domainSource(s *snapshot) keyedSource {
	return keyedSource{
		tiers: []tier{
			{table: "rollup_domain_1d", res: 86400, done: s.dayDone},
			{table: "rollup_domain_1h", res: 3600, done: s.hourDone, keep: HourRetention},
			r.factTier(),
		},
		key: "domain", a: "total", b: "blocked",
		fKey: "domain", fA: "1", fB: "(status BETWEEN 16 AND 23)",
		hour: func(h *hourAcc, emit func(k, a, b int64)) {
			for d, v := range h.domains {
				emit(d, v[0], v[1])
			}
		},
	}
}

func (r *Reader) listSource(s *snapshot, blocked bool) keyedSource {
	if blocked {
		return keyedSource{
			tiers: []tier{{table: "rollup_list_1h", res: 3600, done: s.hourDone, keep: HourRetention}, r.factTier()},
			key:   "list", a: "hits", b: "hits",
			fKey: "list", fA: "1", fB: "1", fWhere: "list IS NOT NULL AND status BETWEEN 16 AND 23",
			hour: func(h *hourAcc, emit func(k, a, b int64)) {
				for l, v := range h.lists {
					if v[0] > 0 {
						emit(l, v[0], v[0])
					}
				}
			},
		}
	}
	return keyedSource{
		tiers: []tier{r.factTier()},
		fKey:  "list", fA: "1", fB: "0", fWhere: "list IS NOT NULL AND status NOT BETWEEN 16 AND 23",
		hour: func(h *hourAcc, emit func(k, a, b int64)) {
			for l, v := range h.lists {
				if v[1] > 0 {
					emit(l, v[1], 0)
				}
			}
		},
	}
}

func (r *Reader) zoneSource(s *snapshot) keyedSource {
	return keyedSource{
		tiers: []tier{{table: "rollup_zone_1h", res: 3600, done: s.hourDone, keep: HourRetention}, r.factTier()},
		key:   "zone", a: "total", b: "0",
		fKey: "zone", fA: "1", fB: "0", fWhere: "zone IS NOT NULL",
		hour: func(h *hourAcc, emit func(k, a, b int64)) {
			for z, v := range h.zones {
				emit(z, v, 0)
			}
		},
	}
}

func (r *Reader) qtypeSource(s *snapshot) keyedSource {
	return keyedSource{
		tiers: []tier{
			{table: "rollup_qtype_1h", res: 3600, done: s.hourDone, keep: HourRetention},
			{table: "rollup_qtype_1m", res: 60, done: forever, keep: MinuteRetention},
		},
		key: "qtype", a: "total", b: "0",
		minute: func(m *minuteAcc, emit func(k, a, b int64)) {
			for q, v := range m.qtypes {
				emit(int64(q), v, 0)
			}
		},
	}
}
