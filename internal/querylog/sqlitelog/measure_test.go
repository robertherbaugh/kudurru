package sqlitelog

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
)

// homeTraffic generates a household/small-office mix: 200 clients, 20 000
// domains with a heavy head, ~15 % blocked, ~45 % cache hits, two-address
// answers (the answer summary is stored under privacy full).
type homeTraffic struct {
	r       *rand.Rand
	clients []netip.Addr
	domains []string
}

func newHomeTraffic(seed uint64) *homeTraffic {
	h := &homeTraffic{r: rand.New(rand.NewPCG(seed, 42))} //nolint:gosec // test data
	for i := range 200 {
		h.clients = append(h.clients, netip.AddrFrom4([4]byte{192, 168, byte(1 + i/250), byte(1 + i%250)}))
	}
	tlds := []string{"com", "net", "org", "io", "de", "co.uk"}
	for i := range 20000 {
		h.domains = append(h.domains, fmt.Sprintf("%s%d.%s%d.%s.", []string{"www", "api", "cdn", "img", "edge"}[i%5], i, "site", i/7, tlds[i%len(tlds)]))
	}
	return h
}

func (h *homeTraffic) event(ts time.Time) *querylog.Event {
	r := h.r
	x := r.Float64()
	d := h.domains[int(float64(len(h.domains))*x*x*x)]
	c := h.clients[int(float64(len(h.clients))*r.Float64()*r.Float64())]
	ev := &querylog.Event{
		Time: ts, Latency: time.Duration(200+r.IntN(40000)) * time.Microsecond, Node: "node-a", Proto: pipeline.ProtoUDP,
		Client: c, Name: d, QType: []uint16{1, 1, 28, 65}[r.IntN(4)], QClass: 1, Rcode: 0, ReplyType: querylog.ReplyIP,
		Answers: 2, Flags: querylog.Flags{RD: true},
	}
	switch p := r.IntN(100); {
	case p < 15:
		ev.Status, ev.List, ev.Rule, ev.ReplyType, ev.Answers = pipeline.StatusBlockedList, "oisd", "||"+d+"^", querylog.ReplyIP, 1
		ev.Answer = "0.0.0.0"
	case p < 60:
		ev.Status, ev.Flags.Cached = pipeline.StatusCached, true
		ev.Answer = fmt.Sprintf("104.16.%d.%d, 104.16.%d.%d", r.IntN(256), r.IntN(256), r.IntN(256), r.IntN(256))
	default:
		ev.Status, ev.Upstream = pipeline.StatusForwarded, "default"
		ev.Answer = fmt.Sprintf("104.16.%d.%d, 104.16.%d.%d", r.IntN(256), r.IntN(256), r.IntN(256), r.IntN(256))
	}
	return ev
}

type ingestResult struct {
	rows                       int
	writeWall, maintWall       time.Duration
	maxBatch                   time.Duration
	totalBytes, factBytes      int64
	factTableBytes, indexBytes int64
}

// ingest simulates `seconds` seconds of traffic at `rate` events/s on a
// fake clock: each simulated second is handed to the writer in batches of
// at most 1 000 (the collector's policy), then Maintain runs. Only Write
// and Maintain are timed.
func ingest(t testing.TB, rate, seconds int, mode querylog.Mode) ingestResult {
	w, fc := newWriter(t, t0, func(o *Options) { o.Privacy = mode; o.RawRetention = 0 })
	ctx := context.Background()
	empty, err := w.DBBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := newHomeTraffic(7)
	var res ingestResult
	batch := make([]*querylog.Event, 0, 1000)
	for s := range seconds {
		sec := t0.Add(time.Duration(s) * time.Second)
		for i := range rate {
			batch = append(batch, h.event(sec.Add(time.Duration(i)*time.Second/time.Duration(rate))))
			if len(batch) == cap(batch) || i == rate-1 {
				start := time.Now()
				if err := w.Write(batch); err != nil {
					t.Fatal(err)
				}
				d := time.Since(start)
				res.writeWall += d
				res.maxBatch = max(res.maxBatch, d)
				batch = batch[:0]
			}
		}
		fc.SetTime(sec.Add(time.Second))
		start := time.Now()
		if err := w.Maintain(ctx); err != nil {
			t.Fatal(err)
		}
		res.maintWall += time.Since(start)
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	res.rows = rate * seconds
	total, _ := w.DBBytes(ctx)
	res.totalBytes = total - empty
	// dbstat attributes pages to the fact table and its four indexes.
	rows, err := w.w.Query(`SELECT name, sum(pgsize) FROM dbstat GROUP BY name`)
	if err != nil {
		t.Logf("dbstat unavailable: %v", err)
		return res
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			t.Fatal(err)
		}
		switch name {
		case "query":
			res.factTableBytes = n
		case "query_ts", "query_client_ts", "query_domain_ts", "query_status_ts":
			res.indexBytes += n
		}
	}
	res.factBytes = res.factTableBytes + res.indexBytes
	return res
}

// TestMeasureIngest produces the numbers in dev-docs/milestones/m2/querylog.md: write
// capacity, duty cycle and bytes per row at 1 000 and 10 000 events/s.
// KUDURRU_MEASURE=1 runs the full 5-minute and 1-minute simulations
// (300 000 and 600 000 rows); the default run is a 20 s / 3 s smoke test
// with the same assertions, and -race only exercises the path.
func TestMeasureIngest(t *testing.T) {
	if testing.Short() {
		t.Skip("simulates up to 600 000 queries")
	}
	full := os.Getenv("KUDURRU_MEASURE") == "1"
	for _, tc := range []struct {
		rate, seconds, quick int
		mode                 querylog.Mode
	}{
		{1000, 300, 20, querylog.PrivacyFull},
		{10000, 60, 3, querylog.PrivacyFull},
		{10000, 60, 3, querylog.PrivacyAnonymizeClient},
	} {
		switch {
		case raceEnabled:
			tc.rate, tc.seconds = tc.rate/10, 1 // the numbers mean nothing under -race
		case !full:
			tc.seconds = tc.quick
		}
		t.Run(fmt.Sprintf("%d_per_s_%s", tc.rate, tc.mode), func(t *testing.T) {
			r := ingest(t, tc.rate, tc.seconds, tc.mode)
			sim := time.Duration(tc.seconds) * time.Second
			t.Logf("%d rows in %s simulated: write %s (%.0f rows/s capacity, %.1f %% duty, max batch %s), maintenance %s",
				r.rows, sim, r.writeWall.Round(time.Millisecond), float64(r.rows)/r.writeWall.Seconds(),
				100*r.writeWall.Seconds()/sim.Seconds(), r.maxBatch.Round(time.Microsecond), r.maintWall.Round(time.Millisecond))
			t.Logf("db %.1f B/row overall (incl. dictionaries and rollups); fact table %.1f B/row + 4 indexes %.1f B/row = %.1f B/row",
				float64(r.totalBytes)/float64(r.rows), float64(r.factTableBytes)/float64(r.rows),
				float64(r.indexBytes)/float64(r.rows), float64(r.factBytes)/float64(r.rows))
			if !raceEnabled && r.writeWall > sim {
				t.Errorf("writer cannot keep up with %d events/s: %s of writes per %s", tc.rate, r.writeWall, sim)
			}
		})
	}
}

// BenchmarkWriteBatch measures one 1 000-row batch (one transaction).
func BenchmarkWriteBatch(b *testing.B) {
	w, _ := newWriter(b, t0, func(o *Options) { o.RawRetention = 0 })
	h := newHomeTraffic(3)
	ctx := context.Background()
	empty, _ := w.DBBytes(ctx)
	batches := make([][]*querylog.Event, 16)
	for i := range batches {
		for j := range 1000 {
			batches[i] = append(batches[i], h.event(t0.Add(time.Duration(i*1000+j)*time.Millisecond)))
		}
	}
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		if err := w.Write(batches[i%len(batches)]); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	rows := float64(b.N) * 1000
	b.ReportMetric(rows/b.Elapsed().Seconds(), "rows/s")
	_ = w.Flush(ctx)
	_, _ = w.Checkpoint(ctx)
	if total, err := w.DBBytes(ctx); err == nil {
		b.ReportMetric(float64(total-empty)/rows, "B/row")
	}
}

// BenchmarkSummary24h measures the dashboard summary over the last 24 h:
// 25 h of traffic at one query per second over the 20 000-domain mix
// (tens of thousands of rollup_domain_1h rows, which the unique-domain
// count scans).
func BenchmarkSummary24h(b *testing.B) {
	w, fc := newWriter(b, t0.Add(-25*time.Hour))
	h := newHomeTraffic(5)
	ctx := context.Background()
	for m := range 25 * 60 {
		ts := t0.Add(-25*time.Hour + time.Duration(m)*time.Minute)
		batch := make([]*querylog.Event, 0, 60)
		for i := range 60 {
			batch = append(batch, h.event(ts.Add(time.Duration(i)*time.Second)))
		}
		if err := w.Write(batch); err != nil {
			b.Fatal(err)
		}
		if m%60 == 59 {
			fc.SetTime(ts.Add(time.Minute))
			if err := w.Maintain(ctx); err != nil {
				b.Fatal(err)
			}
		}
	}
	fc.SetTime(t0)
	_ = w.Maintain(ctx)
	r := w.Reader()
	b.ResetTimer()
	for b.Loop() {
		if _, err := r.Summary(ctx, SummaryQuery{}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkQueriesPage measures one 100-row page of the query log with a
// domain filter over 200 000 rows.
func BenchmarkQueriesPage(b *testing.B) {
	w, _ := newWriter(b, t0)
	h := newHomeTraffic(9)
	batch := make([]*querylog.Event, 0, 1000)
	for i := range 200000 {
		batch = append(batch, h.event(t0.Add(-time.Duration(200000-i)*10*time.Millisecond)))
		if len(batch) == cap(batch) {
			if err := w.Write(batch); err != nil {
				b.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	r := w.Reader()
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		if _, err := r.Queries(ctx, QueryFilter{Domain: h.domains[0]}); err != nil {
			b.Fatal(err)
		}
	}
}
