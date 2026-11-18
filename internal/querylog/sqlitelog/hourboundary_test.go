package sqlitelog

import (
	"context"
	"testing"
	"time"
)

// TestSummaryAcrossHourBoundary writes events in the hour before a boundary
// and in the first seconds after it, then reads a rolling one-hour window
// that ends 30 seconds into the new hour. Every event must be counted:
// the previous hour's pending accumulators must not be dropped from the
// summary, client count or unique-domain count.
func TestSummaryAcrossHourBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		step func(fc interface{ Step(time.Duration) }, w *Writer)
	}{
		{name: "no ticks between writes and read"},
		{name: "after a flush tick", step: func(fc interface{ Step(time.Duration) }, w *Writer) {
			fc.Step(time.Second)
			time.Sleep(50 * time.Millisecond)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := time.Date(2026, 10, 11, 12, 58, 0, 0, time.UTC)
			w, fc := newWriter(t, prev)
			mustWrite(t, w,
				event(prev, "10.0.0.1", "a.example"),
				event(prev.Add(30*time.Second), "10.0.0.2", "b.example"),
				event(prev.Add(60*time.Second), "10.0.0.1", "c.example"),
			)
			fc.Step(2*time.Minute + 5*time.Second) // 13:00:05
			mustWrite(t, w, event(fc.Now().Add(-time.Second), "10.0.0.3", "d.example"))
			if tc.step != nil {
				tc.step(fc, w)
			}
			until := time.Date(2026, 10, 11, 13, 0, 30, 0, time.UTC)
			fc.Step(until.Sub(fc.Now()))
			s, err := w.Reader().Summary(context.Background(), SummaryQuery{Window: Window{From: until.Add(-time.Hour), Until: until}})
			if err != nil {
				t.Fatal(err)
			}
			if s.Queries.Total != 4 || s.Clients.Active != 3 || s.Queries.UniqueDomains != 4 {
				t.Fatalf("total %d, active clients %d, unique domains %d; want 4, 3, 4", s.Queries.Total, s.Clients.Active, s.Queries.UniqueDomains)
			}
			sh, err := w.Reader().SummaryShard(context.Background(), SummaryQuery{Window: Window{From: until.Add(-time.Hour), Until: until}})
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
// flushed late), then reads a one-hour window ending at HH:00:30.
func TestSummaryLateWritesIntoPreviousHour(t *testing.T) {
	now := time.Date(2026, 10, 11, 13, 0, 30, 0, time.UTC)
	w, _ := newWriter(t, now)
	ts := now.Add(-2 * time.Minute) // 12:58:30
	mustWrite(t, w,
		event(ts, "10.0.0.1", "a.example"),
		event(ts.Add(time.Second), "10.0.0.2", "b.example"),
		event(ts.Add(2*time.Second), "10.0.0.1", "c.example"),
	)
	s, err := w.Reader().Summary(context.Background(), SummaryQuery{Window: Window{From: now.Add(-time.Hour), Until: now}})
	if err != nil {
		t.Fatal(err)
	}
	if s.Queries.Total != 3 || s.Clients.Active != 2 || s.Queries.UniqueDomains != 3 {
		t.Fatalf("total %d, active clients %d, unique domains %d; want 3, 2, 3", s.Queries.Total, s.Clients.Active, s.Queries.UniqueDomains)
	}
	sh, err := w.Reader().SummaryShard(context.Background(), SummaryQuery{Window: Window{From: now.Add(-time.Hour), Until: now}})
	if err != nil {
		t.Fatal(err)
	}
	if sh.Queries.Total != 3 || len(sh.ActiveClients.Values) != 2 || len(sh.Domains.Values) != 3 {
		t.Fatalf("shard total %d, active clients %d, domains %d; want 3, 2, 3", sh.Queries.Total, len(sh.ActiveClients.Values), len(sh.Domains.Values))
	}
}
