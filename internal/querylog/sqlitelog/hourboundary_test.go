package sqlitelog

import (
	"context"
	"testing"
	"time"
)

// TestSummaryAcrossHourBoundary writes events in the hour before a boundary
// and in the first seconds after it, then reads a rolling one-hour window
// that ends shortly after it. Every event must be counted whichever
// maintenance has run: the previous hour's pending accumulators must not be
// dropped from the summary, client count or unique-domain count. Maintenance
// is driven by calling Maintain at a chosen clock time (the writer's Run
// ticker is not started here), and each case checks the rollup rows it
// should have produced, so a case cannot pass by testing nothing.
func TestSummaryAcrossHourBoundary(t *testing.T) {
	boundary := time.Date(2026, 10, 11, 13, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		maintain time.Time // clock time of one Maintain call before the read; zero for none
		readAt   time.Time
		want1m   bool // minute rollups must be on disk
		want1h   bool // the 12:00 hourly rollup must be on disk
	}{
		{name: "no maintenance before the read", readAt: boundary.Add(30 * time.Second)},
		{name: "minutes flushed before the read", maintain: boundary.Add(30 * time.Second), readAt: boundary.Add(30 * time.Second), want1m: true},
		{name: "previous hour derived before the read", maintain: boundary.Add(95 * time.Second), readAt: boundary.Add(95 * time.Second), want1m: true, want1h: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := boundary.Add(-2 * time.Minute) // 12:58:00
			w, fc := newWriter(t, prev)
			mustWrite(t, w,
				event(prev, "10.0.0.1", "a.example"),
				event(prev.Add(30*time.Second), "10.0.0.2", "b.example"),
				event(prev.Add(60*time.Second), "10.0.0.1", "c.example"),
			)
			fc.SetTime(boundary.Add(5 * time.Second))
			mustWrite(t, w, event(boundary.Add(4*time.Second), "10.0.0.3", "d.example"))
			if !tc.maintain.IsZero() {
				fc.SetTime(tc.maintain)
				if err := w.Maintain(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			fc.SetTime(tc.readAt)
			if got := queryInt(t, w.w, `SELECT count(*) FROM rollup_1m`) > 0; got != tc.want1m {
				t.Fatalf("minute rollups on disk = %v, want %v", got, tc.want1m)
			}
			if got := queryInt(t, w.w, `SELECT count(*) FROM rollup_1h WHERE ts=?`, prev.Truncate(time.Hour).Unix()) > 0; got != tc.want1h {
				t.Fatalf("12:00 hourly rollup on disk = %v, want %v", got, tc.want1h)
			}
			win := Window{From: tc.readAt.Add(-time.Hour), Until: tc.readAt}
			s, err := w.Reader().Summary(context.Background(), SummaryQuery{Window: win})
			if err != nil {
				t.Fatal(err)
			}
			if s.Queries.Total != 4 || s.Clients.Active != 3 || s.Queries.UniqueDomains != 4 {
				t.Fatalf("total %d, active clients %d, unique domains %d; want 4, 3, 4", s.Queries.Total, s.Clients.Active, s.Queries.UniqueDomains)
			}
			sh, err := w.Reader().SummaryShard(context.Background(), SummaryQuery{Window: win})
			if err != nil {
				t.Fatal(err)
			}
			if sh.Queries.Total != 4 || len(sh.ActiveClients.Values) != 3 || len(sh.Domains.Values) != 4 {
				t.Fatalf("shard total %d, active clients %d, domains %d; want 4, 3, 4", sh.Queries.Total, len(sh.ActiveClients.Values), len(sh.Domains.Values))
			}
		})
	}
}

// TestSummaryLateWritesIntoPreviousHour writes events whose timestamps are in
// the previous hour after the clock has crossed the boundary (a queued batch
// flushed late), then reads a one-hour window ending at HH:00:30 with the
// counts either still in memory or flushed to the minute rollups.
func TestSummaryLateWritesIntoPreviousHour(t *testing.T) {
	now := time.Date(2026, 10, 11, 13, 0, 30, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		flush bool
	}{
		{name: "unflushed"},
		{name: "flushed", flush: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			w, _ := newWriter(t, now)
			at := now.Add(-2 * time.Minute)
			mustWrite(t, w,
				event(at, "10.0.0.1", "a.example"),
				event(at.Add(time.Second), "10.0.0.2", "b.example"),
				event(at.Add(2*time.Second), "10.0.0.1", "c.example"),
			)
			if tc.flush {
				if err := w.Flush(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if onDisk := queryInt(t, w.w, `SELECT count(*) FROM rollup_1m`) > 0; onDisk != tc.flush {
				t.Fatalf("minute rollups on disk = %t, want %t", onDisk, tc.flush)
			}
			query := SummaryQuery{Window: Window{From: now.Add(-time.Hour), Until: now}}
			summary, err := w.Reader().Summary(ctx, query)
			if err != nil {
				t.Fatal(err)
			}
			if summary.Queries.Total != 3 || summary.Clients.Active != 2 || summary.Queries.UniqueDomains != 3 {
				t.Fatalf("total %d, active clients %d, unique domains %d; want 3, 2, 3", summary.Queries.Total, summary.Clients.Active, summary.Queries.UniqueDomains)
			}
			shard, err := w.Reader().SummaryShard(ctx, query)
			if err != nil {
				t.Fatal(err)
			}
			if shard.Queries.Total != 3 || len(shard.ActiveClients.Values) != 2 || len(shard.Domains.Values) != 3 {
				t.Fatalf("shard total %d, active clients %d, domains %d; want 3, 2, 3", shard.Queries.Total, len(shard.ActiveClients.Values), len(shard.Domains.Values))
			}
		})
	}
}
