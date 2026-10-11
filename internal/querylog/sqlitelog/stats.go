package sqlitelog

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
)

// Summary answers GET /stats/summary.
func (r *Reader) Summary(ctx context.Context, q SummaryQuery) (*Summary, error) {
	start := time.Now()
	win := r.window(q.Window)
	var out *Summary
	err := r.read(ctx, true, func(tx *sql.Tx, s *snapshot) error {
		from, until := win.From.Unix(), win.Until.Unix()
		res := &Summary{Window: win, Rcodes: map[string]int64{}, Protocols: map[string]int64{}, Types: map[string]int64{}}
		series, approx, err := r.baseSeries(ctx, tx, s, from, until, 0)
		if err != nil {
			return err
		}
		var b [nBase]int64
		if v := series[0]; v != nil {
			b = *v
		}
		qs := &res.Queries
		qs.Total, qs.Blocked, qs.Cached, qs.Forwarded, qs.Authoritative, qs.Dropped =
			b[cTotal], b[cBlocked], b[cCached], b[cForwarded], b[cAuthoritative], b[cDropped]
		qs.Other = qs.Total - qs.Blocked - qs.Cached - qs.Forwarded - qs.Authoritative
		qs.PercentBlocked = percent(qs.Blocked, qs.Total)
		if span := until - floorTo(from, 60); span > 0 {
			qs.QPSAvg = float64(qs.Total) / float64(span)
		}
		last := floorTo(s.now.Unix(), 60) - 60
		if lm, _, err := r.baseSeries(ctx, tx, s, last, last+60, 0); err != nil {
			return err
		} else if v := lm[0]; v != nil {
			qs.QPS1m = float64(v[cTotal]) / 60
		}
		for name, i := range map[string]int{"NOERROR": cRcNoError, "NXDOMAIN": cRcNXDomain, "SERVFAIL": cRcServfail, "REFUSED": cRcRefused, "other": cRcOther} {
			res.Rcodes[name] = b[i]
		}
		for name, i := range map[string]int{"udp": cUDP, "tcp": cTCP, "dot": cDoT, "doh": cDoH, "doq": cDoQ} {
			res.Protocols[name] = b[i]
		}
		res.Latency = latencySummary(b[cLatSum], b[cLatCnt], b[cLatB0:cLatB0+latencyBuckets])

		_, active, a2, err := r.keyed(ctx, tx, s, r.clientSource(s), from, until, 0, "", "", 0, true)
		if err != nil {
			return err
		}
		res.Clients.Active = active
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM dict_client`).Scan(&res.Clients.Total); err != nil {
			return err
		}
		_, uniq, a3, err := r.keyed(ctx, tx, s, r.domainSource(s), from, until, 0, "", "", 0, true)
		if err != nil {
			return err
		}
		qs.UniqueDomains = uniq
		types, _, _, err := r.keyed(ctx, tx, s, r.qtypeSource(s), from, until, 0, "", "a DESC", 0, false)
		if err != nil {
			return err
		}
		for _, t := range types {
			res.Types[querylog.QTypeName(uint16(t.k))] += t.a //nolint:gosec // qtype column holds uint16
		}
		res.Approximate = approx || a2 || a3
		if f := r.w.opts.DomainsOnLists; f != nil {
			res.DomainsOnLists = f()
		}
		out = res
		return nil
	})
	if err != nil {
		return nil, err
	}
	out.Scope = r.scope(start)
	return out, nil
}

func percent(n, total int64) float64 {
	if total == 0 {
		return 0
	}
	return math.Round(float64(n)/float64(total)*10000) / 100
}

// latencySummary computes the average and histogram percentiles in ms.
// Percentiles are bucket upper bounds; the +Inf bucket reports the last
// finite bound (1 s).
func latencySummary(sum, cnt int64, buckets []int64) LatencySummary {
	if cnt == 0 {
		return LatencySummary{}
	}
	pct := func(q float64) float64 {
		target := int64(math.Ceil(q * float64(cnt)))
		var acc int64
		for i, n := range buckets {
			acc += n
			if acc >= target {
				if i < len(latencyBoundsUS) {
					return float64(latencyBoundsUS[i]) / 1000
				}
				break
			}
		}
		return float64(latencyBoundsUS[len(latencyBoundsUS)-1]) / 1000
	}
	return LatencySummary{
		AvgMS: math.Round(float64(sum)/float64(cnt)) / 1000,
		P50MS: pct(0.50), P95MS: pct(0.95), P99MS: pct(0.99),
	}
}

// autoStep picks a bucket width: at most maxPoints buckets, at least a
// minute, at least an hour beyond the minute rollups' retention and a day
// beyond the hour rollups'.
func autoStep(win Window, now time.Time) int64 {
	span := int64(win.Until.Sub(win.From) / time.Second)
	minStep := int64(60)
	if win.From.Before(now.Add(-MinuteRetention)) {
		minStep = 3600
	}
	if win.From.Before(now.Add(-HourRetention)) {
		minStep = 86400
	}
	for _, s := range []int64{60, 300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400, 7 * 86400, 30 * 86400} {
		if s >= minStep && span/s <= maxPoints {
			return s
		}
	}
	return 30 * 86400
}

// stepSeconds validates an explicit step (rounded up to whole minutes).
func stepSeconds(step time.Duration, win Window, now time.Time) int64 {
	if step <= 0 {
		return autoStep(win, now)
	}
	s := ceilTo(int64(step/time.Second), 60)
	return max(s, 60)
}

// Timeseries answers GET /stats/timeseries: dense buckets of the status
// classes (total/blocked/cached/forwarded/authoritative/other).
func (r *Reader) Timeseries(ctx context.Context, q TimeseriesQuery) (*Timeseries, error) {
	start := time.Now()
	win := r.window(q.Window)
	var out *Timeseries
	err := r.read(ctx, false, func(tx *sql.Tx, s *snapshot) error {
		step := stepSeconds(q.Step, win, s.now)
		from, until := floorTo(win.From.Unix(), step), win.Until.Unix()
		series, approx, err := r.baseSeries(ctx, tx, s, from, until, step)
		if err != nil {
			return err
		}
		res := &Timeseries{Window: win, Step: step, Approximate: approx}
		for bk := from; bk < until; bk += step {
			p := Point{TS: time.Unix(bk, 0).UTC()}
			if v := series[bk]; v != nil {
				p.Total, p.Blocked, p.Cached, p.Forwarded, p.Authoritative = v[cTotal], v[cBlocked], v[cCached], v[cForwarded], v[cAuthoritative]
				p.Other = p.Total - p.Blocked - p.Cached - p.Forwarded - p.Authoritative
			}
			res.Points = append(res.Points, p)
		}
		out = res
		return nil
	})
	if err != nil {
		return nil, err
	}
	out.Scope = r.scope(start)
	return out, nil
}

func topN(n int) int {
	if n <= 0 {
		return DefaultTopN
	}
	return min(n, MaxTopN)
}

// top runs a ranked keyed query and resolves names with resolve.
func (r *Reader) top(ctx context.Context, dim string, q TopQuery, withHours bool,
	src func(s *snapshot) keyedSource, resolve func(ctx context.Context, tx *sql.Tx, ids []int64) (map[int64]TopItem, error)) (*TopList, error) {
	start := time.Now()
	win := r.window(q.Window)
	n := topN(q.N)
	var out *TopList
	err := r.read(ctx, withHours, func(tx *sql.Tx, s *snapshot) error {
		from, until := win.From.Unix(), win.Until.Unix()
		having, order, count := "a-b > 0", "a-b DESC, k", func(kr keyedRow) int64 { return kr.a - kr.b }
		if q.Blocked {
			having, order, count = "b > 0", "b DESC, k", func(kr keyedRow) int64 { return kr.b }
		}
		rows, _, approx, err := r.keyed(ctx, tx, s, src(s), from, until, 0, having, order, n, false)
		if err != nil {
			return err
		}
		base, _, err := r.baseSeries(ctx, tx, s, from, until, 0)
		if err != nil {
			return err
		}
		res := &TopList{Window: win, Dimension: dim, Blocked: q.Blocked, Approximate: approx, Items: []TopItem{}}
		if v := base[0]; v != nil {
			res.TotalInWindow = v[cTotal]
			if q.Blocked {
				res.TotalInWindow = v[cBlocked]
			}
		}
		ids := make([]int64, len(rows))
		for i, kr := range rows {
			ids[i] = kr.k
		}
		names, err := resolve(ctx, tx, ids)
		if err != nil {
			return err
		}
		for _, kr := range rows {
			it := names[kr.k]
			if it.Key == "" {
				it.Key = "#" + strconv.FormatInt(kr.k, 10)
			}
			it.Count, it.Total, it.Blocked = count(kr), kr.a, kr.b
			it.Percent = percent(it.Count, res.TotalInWindow)
			res.Items = append(res.Items, it)
		}
		out = res
		return nil
	})
	if err != nil {
		return nil, err
	}
	out.Scope = r.scope(start)
	return out, nil
}

// TopDomains answers GET /stats/top?dim=domain|blocked_domain.
func (r *Reader) TopDomains(ctx context.Context, q TopQuery) (*TopList, error) {
	return r.top(ctx, "domain", q, true, r.domainSource, r.names("dict_domain", "name"))
}

// TopClients answers GET /stats/top?dim=client|blocked_client.
func (r *Reader) TopClients(ctx context.Context, q TopQuery) (*TopList, error) {
	return r.top(ctx, "client", q, false, r.clientSource, r.clientNames)
}

// TopLists answers GET /stats/top?dim=list: blocklists ranked by the
// queries they blocked (Blocked) or allowlists by the queries they let
// through. Permitted list hits have no rollup (rollup_list_1h counts
// blocks, like kudurru_block_list_hits_total), so that ranking reads the
// fact table outside the pending hours and is bounded by raw retention.
func (r *Reader) TopLists(ctx context.Context, q TopQuery) (*TopList, error) {
	src := func(s *snapshot) keyedSource { return r.listSource(s, q.Blocked) }
	return r.top(ctx, "list", q, true, src, r.names("dict_list", "name"))
}

// TopZones ranks the authoritative and forward zones by queries.
func (r *Reader) TopZones(ctx context.Context, q TopQuery) (*TopList, error) {
	q.Blocked = false
	return r.top(ctx, "zone", q, true, r.zoneSource, r.names("dict_zone", "name"))
}

// names resolves ids of a name-only dictionary.
func (r *Reader) names(table, col string) func(ctx context.Context, tx *sql.Tx, ids []int64) (map[int64]TopItem, error) {
	return func(ctx context.Context, tx *sql.Tx, ids []int64) (map[int64]TopItem, error) {
		out := map[int64]TopItem{}
		if len(ids) == 0 {
			return out, nil
		}
		rows, err := tx.QueryContext(ctx, `SELECT id, `+col+` FROM `+table+` WHERE id IN (SELECT value FROM json_each(?))`, idsJSON(ids)) //nolint:gosec // identifiers and expressions are package constants; values are bound parameters
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				return nil, err
			}
			out[id] = TopItem{Key: name}
		}
		return out, rows.Err()
	}
}

func (r *Reader) clientNames(ctx context.Context, tx *sql.Tx, ids []int64) (map[int64]TopItem, error) {
	out := map[int64]TopItem{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, ip, coalesce(name,''), coalesce(grp,'') FROM dict_client WHERE id IN (SELECT value FROM json_each(?))`, idsJSON(ids))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var it TopItem
		if err := rows.Scan(&id, &it.Key, &it.Name, &it.Group); err != nil {
			return nil, err
		}
		if it.Key == "" {
			it.Key = it.Name
		}
		out[id] = it
	}
	return out, rows.Err()
}

func idsJSON(ids []int64) string {
	b := []byte{'['}
	for i, id := range ids {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendInt(b, id, 10)
	}
	return string(append(b, ']'))
}

// TopUpstreams answers GET /stats/upstreams: traffic, errors, timeouts and
// latency per upstream ("cache" and "zone:<name>" included).
func (r *Reader) TopUpstreams(ctx context.Context, q TopQuery) (*UpstreamTop, error) {
	start := time.Now()
	win := r.window(q.Window)
	n := topN(q.N)
	var out *UpstreamTop
	err := r.read(ctx, false, func(tx *sql.Tx, s *snapshot) error {
		from, until := floorTo(win.From.Unix(), 60), win.Until.Unix()
		segs := cover(from, until, []tier{
			{table: "rollup_upstream_1h", res: 3600, done: s.hourDone, keep: HourRetention},
			{table: "rollup_upstream_1m", res: 60, done: forever, keep: MinuteRetention},
		})
		acc := map[int64]*[nUp]int64{}
		get := func(k int64) *[nUp]int64 {
			v := acc[k]
			if v == nil {
				v = new([nUp]int64)
				acc[k] = v
			}
			return v
		}
		if len(segs) > 0 {
			var parts []string
			var args []any
			for _, sg := range segs {
				parts = append(parts, `SELECT upstream, `+strings.Join(upCols, ",")+` FROM `+sg.t.table+` WHERE ts>=? AND ts<?`)
				args = append(args, sg.from, sg.until)
			}
			rows, err := tx.QueryContext(ctx, `SELECT upstream, `+sumCols(upCols)+` FROM (`+strings.Join(parts, " UNION ALL ")+`) GROUP BY upstream`, args...) //nolint:gosec // identifiers and expressions are package constants; values are bound parameters
			if err != nil {
				return fmt.Errorf("sqlitelog: upstreams: %w", err)
			}
			for rows.Next() {
				var k int64
				vals := make([]sql.NullInt64, nUp)
				dest := []any{&k}
				for i := range vals {
					dest = append(dest, &vals[i])
				}
				if err := rows.Scan(dest...); err != nil {
					_ = rows.Close()
					return err
				}
				v := get(k)
				for i := range vals {
					v[i] += vals[i].Int64
				}
			}
			if err := rows.Close(); err != nil {
				return err
			}
		}
		for ts, m := range s.minutes {
			if ts >= from && ts < until {
				for k, u := range m.upstreams {
					v := get(k)
					for i := range v {
						v[i] += u[i]
					}
				}
			}
		}
		ids := make([]int64, 0, len(acc))
		var total int64
		for k, v := range acc {
			ids = append(ids, k)
			total += v[uTotal]
		}
		sort.Slice(ids, func(i, j int) bool {
			if acc[ids[i]][uTotal] != acc[ids[j]][uTotal] {
				return acc[ids[i]][uTotal] > acc[ids[j]][uTotal]
			}
			return ids[i] < ids[j]
		})
		if len(ids) > n {
			ids = ids[:n]
		}
		names, err := r.names("dict_upstream", "addr")(ctx, tx, ids)
		if err != nil {
			return err
		}
		res := &UpstreamTop{Window: win, Items: []UpstreamItem{}, Approximate: approximate(segs, s.now)}
		for _, k := range ids {
			v := acc[k]
			lat := latencySummary(v[uLatSum], v[uLatCnt], v[uLatB0:uLatB0+latencyBuckets])
			res.Items = append(res.Items, UpstreamItem{
				Addr: names[k].Key, Total: v[uTotal], Errors: v[uErrors], Timeouts: v[uTimeouts],
				Percent: percent(v[uTotal], total), AvgMS: lat.AvgMS, P95MS: lat.P95MS,
			})
		}
		out = res
		return nil
	})
	if err != nil {
		return nil, err
	}
	out.Scope = r.scope(start)
	return out, nil
}

// ClientTimeseries answers GET /stats/clients/timeseries: the top N
// clients by queries in the window, one series each, plus "other".
func (r *Reader) ClientTimeseries(ctx context.Context, q ClientTimeseriesQuery) (*ClientTimeseries, error) {
	start := time.Now()
	win := r.window(q.Window)
	n := topN(q.N)
	var out *ClientTimeseries
	err := r.read(ctx, false, func(tx *sql.Tx, s *snapshot) error {
		step := stepSeconds(q.Step, win, s.now)
		from, until := floorTo(win.From.Unix(), step), win.Until.Unix()
		src := r.clientSource(s)
		top, _, approx, err := r.keyed(ctx, tx, s, src, from, until, 0, "", "a DESC, k", n, false)
		if err != nil {
			return err
		}
		ids := make([]int64, len(top))
		col := map[int64]int{}
		for i, t := range top {
			ids[i] = t.k
			col[t.k] = i
		}
		names, err := r.clientNames(ctx, tx, ids)
		if err != nil {
			return err
		}
		res := &ClientTimeseries{Window: win, Step: step, Approximate: approx}
		for _, id := range ids {
			it := names[id]
			res.Clients = append(res.Clients, ClientKey{Key: it.Key, Name: it.Name})
		}
		res.Clients = append(res.Clients, ClientKey{Key: "other"})
		rows, _, _, err := r.keyed(ctx, tx, s, src, from, until, step, "", "", 0, false)
		if err != nil {
			return err
		}
		buckets := map[int64][]int64{}
		for _, kr := range rows {
			c := buckets[kr.bk]
			if c == nil {
				c = make([]int64, len(res.Clients))
				buckets[kr.bk] = c
			}
			i, ok := col[kr.k]
			if !ok {
				i = len(res.Clients) - 1
			}
			c[i] += kr.a
		}
		for bk := from; bk < until; bk += step {
			c := buckets[bk]
			if c == nil {
				c = make([]int64, len(res.Clients))
			}
			res.Points = append(res.Points, ClientPoint{TS: time.Unix(bk, 0).UTC(), Counts: c})
		}
		out = res
		return nil
	})
	if err != nil {
		return nil, err
	}
	out.Scope = r.scope(start)
	return out, nil
}

// RecentBlocked answers GET /stats/recent_blocked through the partial
// status index (one ordered probe per blocked status).
func (r *Reader) RecentBlocked(ctx context.Context, n int) (*RecentBlocked, error) {
	start := time.Now()
	n = topN(n)
	var out *RecentBlocked
	err := r.read(ctx, false, func(tx *sql.Tx, _ *snapshot) error {
		var parts []string
		var args []any
		for st := pipeline.StatusBlockedList; st <= pipeline.StatusBlockedRateLimit; st++ {
			parts = append(parts, `SELECT * FROM (SELECT id, ts, status, client, domain, list FROM query WHERE status=? ORDER BY ts DESC LIMIT ?)`)
			args = append(args, int64(st), n)
		}
		//nolint:gosec // identifiers and expressions are package constants; values are bound parameters
		rows, err := tx.QueryContext(ctx, `SELECT b.id, b.ts, b.status, coalesce(c.ip,''), coalesce(c.name,''), coalesce(d.name,''), coalesce(l.name,'')
			FROM (`+strings.Join(parts, " UNION ALL ")+`) b
			LEFT JOIN dict_client c ON c.id=b.client LEFT JOIN dict_domain d ON d.id=b.domain LEFT JOIN dict_list l ON l.id=b.list
			ORDER BY b.ts DESC, b.id DESC LIMIT ?`, append(args, n)...)
		if err != nil {
			return fmt.Errorf("sqlitelog: recent blocked: %w", err)
		}
		defer func() { _ = rows.Close() }()
		res := &RecentBlocked{Items: []BlockedItem{}}
		for rows.Next() {
			var it BlockedItem
			var id, ts, st int64
			if err := rows.Scan(&id, &ts, &st, &it.Client, &it.ClientName, &it.Domain, &it.List); err != nil {
				return err
			}
			it.ID, it.TS, it.Status = uint64(id), time.UnixMicro(ts).UTC(), pipeline.Status(st).String() //nolint:gosec // ids and statuses are small non-negative ints
			res.Items = append(res.Items, it)
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
