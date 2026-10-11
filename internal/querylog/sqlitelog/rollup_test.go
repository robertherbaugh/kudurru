package sqlitelog

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
)

// randomEvents produces a deterministic, varied workload in [from, from+n·step).
type workload struct {
	r       *rand.Rand
	clients []string
	domains []string
}

func newWorkload(seed uint64, clients, domains int) *workload {
	w := &workload{r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))} //nolint:gosec // test data
	for i := range clients {
		w.clients = append(w.clients, fmt.Sprintf("192.168.%d.%d", i/250, i%250+1))
	}
	for i := range domains {
		w.domains = append(w.domains, fmt.Sprintf("host%d.site%d.example", i, i%37))
	}
	return w
}

var statuses = []pipeline.Status{
	pipeline.StatusForwarded, pipeline.StatusForwarded, pipeline.StatusForwarded, pipeline.StatusCached, pipeline.StatusCached,
	pipeline.StatusCachedStale, pipeline.StatusAuthoritative, pipeline.StatusLocalRecord, pipeline.StatusRetried,
	pipeline.StatusBlockedList, pipeline.StatusBlockedList, pipeline.StatusBlockedRegex, pipeline.StatusBlockedCNAME,
	pipeline.StatusRefusedACL, pipeline.StatusDropped, pipeline.StatusServfailUpstream, pipeline.StatusTimeoutUpstream,
}

func (w *workload) event(ts time.Time) *querylog.Event {
	r := w.r
	// Zipf-ish: low indexes are hot.
	d := w.domains[int(float64(len(w.domains))*r.Float64()*r.Float64())]
	c := w.clients[int(float64(len(w.clients))*r.Float64()*r.Float64())]
	st := statuses[r.IntN(len(statuses))]
	opts := []evOpt{
		withStatus(st),
		withQType([]uint16{1, 1, 1, 28, 28, 65, 12, 16, 33}[r.IntN(9)]),
		withLatency(time.Duration(r.ExpFloat64()*20000) * time.Microsecond),
		withProto([]pipeline.Proto{pipeline.ProtoUDP, pipeline.ProtoUDP, pipeline.ProtoTCP, pipeline.ProtoDoT, pipeline.ProtoDoH, pipeline.ProtoDoH3, pipeline.ProtoDoQ}[r.IntN(7)]),
		withRcode([]int{0, 0, 0, 0, 3, 2, 5, 1}[r.IntN(8)]),
	}
	switch {
	case isBlocked(st):
		opts = append(opts, withList([]string{"oisd", "stevenblack", "custom"}[r.IntN(3)], "||"+d+"^"))
	case st == pipeline.StatusAuthoritative || st == pipeline.StatusLocalRecord:
		opts = append(opts, withZone([]string{"home.lan.", "corp.example."}[r.IntN(2)]), withUpstream(""))
	case st == pipeline.StatusCached || st == pipeline.StatusCachedStale:
		opts = append(opts, withUpstream(""))
	case st == pipeline.StatusDropped:
		opts = append(opts, withReply(querylog.ReplyNone), withUpstream(""))
	default:
		opts = append(opts, withUpstream([]string{"default", "default", "internal-ad"}[r.IntN(3)]))
	}
	if r.IntN(10) == 0 {
		opts = append(opts, withList("allow-corp", "@@||"+d+"^"))
	}
	return event(ts, c, d, opts...)
}

// factCols mirror baseCols as aggregate expressions over the fact table.
func factCols() []string {
	c := []string{
		"count(*)", "sum(status BETWEEN 16 AND 23)", "sum(status IN (2,3))", "sum(status IN (1,6,8))", "sum(status IN (4,5,7))", "sum(status=25)",
		"sum(rcode=0)", "sum(rcode=3)", "sum(rcode=2)", "sum(rcode=5)", "sum(rcode NOT IN (0,2,3,5))",
		"sum(proto=0)", "sum(proto=1)", "sum(proto=2)", "sum(proto IN (3,6))", "sum(proto=4)",
		"coalesce(sum(latency_us),0)", "count(latency_us)",
	}
	lo := int64(-1)
	for i := range latencyBuckets {
		hi := int64(1 << 62)
		if i < len(latencyBoundsUS) {
			hi = latencyBoundsUS[i]
		}
		c = append(c, fmt.Sprintf("sum(latency_us > %d AND latency_us <= %d)", lo, hi))
		lo = hi
	}
	return c
}

// rowsOf renders every row of q as a string, sorted.
func rowsOf(t *testing.T, db *sql.DB, q string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, q)
	}
	defer func() { _ = rows.Close() }()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]sql.NullInt64, len(cols))
		dest := make([]any, len(cols))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = fmt.Sprint(v.Int64)
		}
		out = append(out, strings.Join(parts, " "))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

func sameRows(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(want) == 0 {
		t.Errorf("%s: no reference rows", what)
	}
	if !slices.Equal(got, want) {
		n := min(len(got), len(want), 5)
		t.Errorf("%s: %d rows vs %d reference rows; first rows\n got  %q\n want %q", what, len(got), len(want), got[:n], want[:n])
	}
}

// checkRollups asserts every rollup equals the fact table (or the finer
// rollup it is derived from).
func checkRollups(t *testing.T, w *Writer) {
	t.Helper()
	db := w.w
	fc := strings.Join(factCols(), ",")
	bc := strings.Join(baseCols, ",")
	sameRows(t, "rollup_1m = facts per minute",
		rowsOf(t, db, `SELECT ts, `+bc+` FROM rollup_1m`),
		rowsOf(t, db, `SELECT (ts/60000000)*60, `+fc+` FROM query GROUP BY 1`))
	sameRows(t, "rollup_client_1m = facts",
		rowsOf(t, db, `SELECT ts, client, total, blocked FROM rollup_client_1m`),
		rowsOf(t, db, `SELECT (ts/60000000)*60, client, count(*), sum(status BETWEEN 16 AND 23) FROM query GROUP BY 1, 2`))
	sameRows(t, "rollup_qtype_1m = facts",
		rowsOf(t, db, `SELECT ts, qtype, total FROM rollup_qtype_1m`),
		rowsOf(t, db, `SELECT (ts/60000000)*60, qtype, count(*) FROM query GROUP BY 1, 2`))
	sameRows(t, "rollup_upstream_1m = facts",
		rowsOf(t, db, `SELECT ts, upstream, total, errors, timeouts, lat_sum_us, lat_cnt FROM rollup_upstream_1m`),
		rowsOf(t, db, `SELECT (ts/60000000)*60, upstream, count(*), sum(status=26), sum(status=27), coalesce(sum(latency_us),0), count(latency_us) FROM query WHERE upstream IS NOT NULL GROUP BY 1, 2`))

	hour := func(table, cols string) string {
		return `SELECT ts, ` + cols + ` FROM ` + table + ` WHERE ts < ` + fmt.Sprint(w.hourDone)
	}
	sameRows(t, "rollup_1h = sum(rollup_1m)",
		rowsOf(t, db, hour("rollup_1h", bc)),
		rowsOf(t, db, `SELECT (ts/3600)*3600, `+sumCols(baseCols)+` FROM rollup_1m WHERE ts < ? GROUP BY 1`, w.hourDone))
	sameRows(t, "rollup_1h = facts per hour",
		rowsOf(t, db, hour("rollup_1h", bc)),
		rowsOf(t, db, `SELECT (ts/3600000000)*3600, `+fc+` FROM query WHERE ts < ? GROUP BY 1`, w.hourDone*1e6))
	sameRows(t, "rollup_client_1h = facts",
		rowsOf(t, db, hour("rollup_client_1h", "client, total, blocked")),
		rowsOf(t, db, `SELECT (ts/3600000000)*3600, client, count(*), sum(status BETWEEN 16 AND 23) FROM query WHERE ts < ? GROUP BY 1, 2`, w.hourDone*1e6))
	sameRows(t, "rollup_qtype_1h = facts",
		rowsOf(t, db, hour("rollup_qtype_1h", "qtype, total")),
		rowsOf(t, db, `SELECT (ts/3600000000)*3600, qtype, count(*) FROM query WHERE ts < ? GROUP BY 1, 2`, w.hourDone*1e6))
	sameRows(t, "rollup_upstream_1h = sum(rollup_upstream_1m)",
		rowsOf(t, db, hour("rollup_upstream_1h", "upstream, "+strings.Join(upCols, ","))),
		rowsOf(t, db, `SELECT (ts/3600)*3600, upstream, `+sumCols(upCols)+` FROM rollup_upstream_1m WHERE ts < ? GROUP BY 1, 2`, w.hourDone))
	sameRows(t, "rollup_domain_1h = facts",
		rowsOf(t, db, hour("rollup_domain_1h", "domain, total, blocked")),
		rowsOf(t, db, `SELECT (ts/3600000000)*3600, domain, count(*), sum(status BETWEEN 16 AND 23) FROM query WHERE ts < ? GROUP BY 1, 2`, w.hourDone*1e6))
	sameRows(t, "rollup_list_1h = blocked facts",
		rowsOf(t, db, hour("rollup_list_1h", "list, hits")),
		rowsOf(t, db, `SELECT (ts/3600000000)*3600, list, count(*) FROM query WHERE ts < ? AND list IS NOT NULL AND status BETWEEN 16 AND 23 GROUP BY 1, 2`, w.hourDone*1e6))
	sameRows(t, "rollup_zone_1h = facts",
		rowsOf(t, db, hour("rollup_zone_1h", "zone, total")),
		rowsOf(t, db, `SELECT (ts/3600000000)*3600, zone, count(*) FROM query WHERE ts < ? AND zone IS NOT NULL GROUP BY 1, 2`, w.hourDone*1e6))

	if w.dayDone > queryInt(t, db, `SELECT min(ts) FROM rollup_1h`) {
		day := func(table, cols string) string {
			return `SELECT ts, ` + cols + ` FROM ` + table + ` WHERE ts < ` + fmt.Sprint(w.dayDone)
		}
		sameRows(t, "rollup_1d = sum(rollup_1h)",
			rowsOf(t, db, day("rollup_1d", bc)),
			rowsOf(t, db, `SELECT (ts/86400)*86400, `+sumCols(baseCols)+` FROM rollup_1h WHERE ts < ? GROUP BY 1`, w.dayDone))
		sameRows(t, "rollup_client_1d = sum(rollup_client_1h)",
			rowsOf(t, db, day("rollup_client_1d", "client, total, blocked")),
			rowsOf(t, db, `SELECT (ts/86400)*86400, client, sum(total), sum(blocked) FROM rollup_client_1h WHERE ts < ? GROUP BY 1, 2`, w.dayDone))
		sameRows(t, "rollup_domain_1d = sum(rollup_domain_1h)",
			rowsOf(t, db, day("rollup_domain_1d", "domain, total, blocked")),
			rowsOf(t, db, `SELECT (ts/86400)*86400, domain, sum(total), sum(blocked) FROM rollup_domain_1h WHERE ts < ? GROUP BY 1, 2`, w.dayDone))
	}
}

// TestRollupsEqualFacts drives three hours across midnight minute by
// minute with a fake clock and checks that every rollup equals the facts.
func TestRollupsEqualFacts(t *testing.T) {
	start := time.Date(2026, 10, 10, 22, 0, 0, 0, time.UTC)
	w, fc := newWriter(t, start)
	wl := newWorkload(1, 40, 400)
	ctx := context.Background()
	total := 0
	for m := range 180 {
		minute := start.Add(time.Duration(m) * time.Minute)
		n := wl.r.IntN(40)
		batch := make([]*querylog.Event, 0, n)
		for range n {
			batch = append(batch, wl.event(minute.Add(time.Duration(wl.r.Int64N(int64(time.Minute))))))
		}
		total += n
		if n > 0 {
			mustWrite(t, w, batch...)
		}
		fc.SetTime(minute.Add(time.Minute))
		if err := w.Maintain(ctx); err != nil {
			t.Fatal(err)
		}
	}
	fc.SetTime(start.Add(3*time.Hour + 2*time.Minute))
	if err := w.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	if got := queryInt(t, w.w, `SELECT count(*) FROM query`); got != int64(total) {
		t.Fatalf("facts = %d, want %d", got, total)
	}
	if want := start.Add(3 * time.Hour).Unix(); w.hourDone != want {
		t.Errorf("hourDone = %d, want %d", w.hourDone, want)
	}
	if want := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC).Unix(); w.dayDone != want {
		t.Errorf("dayDone = %d, want %d", w.dayDone, want)
	}
	if len(w.acc.minutes) != 0 || len(w.acc.hours) != 0 {
		t.Errorf("pending after derivation: %d minutes, %d hours", len(w.acc.minutes), len(w.acc.hours))
	}
	checkRollups(t, w)

	// A late event for an hour (and day) already derived marks it dirty;
	// the next maintenance step re-derives it.
	late := wl.event(start.Add(15*time.Minute + 7*time.Second))
	mustWrite(t, w, late)
	if _, ok := w.acc.dirty[start.Unix()]; !ok {
		t.Fatal("late event did not mark its hour dirty")
	}
	if err := w.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	if len(w.acc.dirty) != 0 {
		t.Errorf("dirty hours left: %v", w.acc.dirty)
	}
	checkRollups(t, w)
}

// TestMinuteFlushIsAdditive flushes a minute, adds a late event to it and
// flushes again.
func TestMinuteFlushIsAdditive(t *testing.T) {
	w, fc := newWriter(t, t0)
	ctx := context.Background()
	mustWrite(t, w, event(t0.Add(10*time.Second), "10.0.0.1", "a.example"), event(t0.Add(20*time.Second), "10.0.0.2", "b.example"))
	fc.SetTime(t0.Add(time.Minute + flushDelay))
	if err := w.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	if n := queryInt(t, w.w, `SELECT total FROM rollup_1m WHERE ts=?`, t0.Unix()); n != 2 {
		t.Fatalf("rollup_1m total after first flush = %d", n)
	}
	mustWrite(t, w, event(t0.Add(59*time.Second), "10.0.0.1", "c.example", withStatus(pipeline.StatusBlockedList), withList("oisd", "||c.example^")))
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if n := queryInt(t, w.w, `SELECT total FROM rollup_1m WHERE ts=?`, t0.Unix()); n != 3 {
		t.Errorf("rollup_1m total after late flush = %d, want 3", n)
	}
	if n := queryInt(t, w.w, `SELECT blocked FROM rollup_client_1m WHERE ts=? AND client=(SELECT id FROM dict_client WHERE ip='10.0.0.1')`, t0.Unix()); n != 1 {
		t.Errorf("client blocked = %d", n)
	}
	if s := w.Stats(); s.Flushes != 2 || s.Rows != 3 || s.Batches != 2 {
		t.Errorf("stats = %+v", s)
	}
}

func TestLatencyBuckets(t *testing.T) {
	for _, tc := range []struct {
		us   int64
		want int
	}{{0, 0}, {500, 0}, {501, 1}, {1000, 1}, {2500, 3}, {999_999, 10}, {1_000_000, 10}, {1_000_001, 11}, {60_000_000, 11}} {
		if got := latBucket(tc.us); got != tc.want {
			t.Errorf("latBucket(%d) = %d, want %d", tc.us, got, tc.want)
		}
	}
}

func TestCover(t *testing.T) {
	tiers := []tier{{table: "d", res: 86400, done: 3 * 86400}, {table: "h", res: 3600, done: 3*86400 + 5*3600}, {table: "m", res: 60, done: forever}}
	segs := cover(86400-7200+120, 3*86400+6*3600+300, tiers)
	var got []string
	for _, s := range segs {
		got = append(got, fmt.Sprintf("%s[%d,%d)", s.t.table, s.from, s.until))
	}
	// Expected: minutes up to the next hour, hours up to midnight, days
	// 1-3, hours up to hourDone, minutes after.
	exp := []string{
		fmt.Sprintf("m[%d,%d)", 86400-7200+120, 86400-3600),
		fmt.Sprintf("h[%d,%d)", 86400-3600, 86400),
		fmt.Sprintf("d[%d,%d)", 86400, 3*86400),
		fmt.Sprintf("h[%d,%d)", 3*86400, 3*86400+5*3600),
		fmt.Sprintf("m[%d,%d)", 3*86400+5*3600, 3*86400+6*3600+300),
	}
	if !slices.Equal(got, exp) {
		t.Errorf("cover:\n got %v\nwant %v", got, exp)
	}
}

// TestRunFlushesAtMinuteBoundary drives the maintenance runnable with the
// fake clock: the minute is flushed once the boundary (plus the flush
// delay) passes, and Run returns when its context ends.
func TestRunFlushesAtMinuteBoundary(t *testing.T) {
	w, fc := newWriter(t, t0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	mustWrite(t, w, event(t0.Add(5*time.Second), "10.0.0.1", "a.example"))
	waitFor(t, "maintenance ticker", fc.HasWaiters)
	fc.Step(30 * time.Second)
	time.Sleep(10 * time.Millisecond)
	if w.Stats().Flushes != 0 {
		t.Fatal("flushed an open minute")
	}
	fc.Step(time.Minute)
	waitFor(t, "minute flush", func() bool { return w.Stats().Flushes == 1 })
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v", err)
	}
	if n := queryInt(t, w.w, `SELECT total FROM rollup_1m WHERE ts=?`, t0.Unix()); n != 1 {
		t.Errorf("rollup_1m total = %d", n)
	}
}
