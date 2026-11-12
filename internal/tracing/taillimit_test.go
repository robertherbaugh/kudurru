package tracing

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestTailKeepIsCappedPerSecond: traces kept only by the tail rule
// (failed or slow) are capped per second, so a client that asks for names
// that fail cannot force an export per query; the excess is counted as
// tail_limited. Head-sampled and edge-continued traces are not capped.
func TestTailKeepIsCappedPerSecond(t *testing.T) {
	tr, _, _ := newTestTracer(t, Config{Ratio: 0, Slow: time.Millisecond, TailLimit: 3, AcceptIncoming: true})
	now := time.Unix(1_700_000_000, 0)
	tr.now = func() time.Time { return now }
	decide := func(slow, failed bool) bool {
		trace, _ := tr.Begin(query(), false, now)
		latency := time.Duration(0)
		if slow {
			latency = time.Second
		}
		kept := trace.Decide(latency, failed)
		trace.Finish(now)
		return kept
	}
	kept := 0
	for i := range 10 {
		if decide(i%2 == 0, i%2 == 1) {
			kept++
		}
	}
	if kept != 3 {
		t.Fatalf("kept %d tail traces in one second, want 3", kept)
	}
	if got := testutil.ToFloat64(tr.metrics.traces[resultTailLimited]); got != 7 {
		t.Fatalf("tail_limited = %v, want 7", got)
	}
	if got := testutil.ToFloat64(tr.metrics.traces[resultNotKept]); got != 0 {
		t.Fatalf("not_kept = %v, want 0", got)
	}
	// A trace from a trusted edge is kept whatever the cap.
	edge, _ := tr.Begin(query(traceparent(DefaultOptionCode, 0)), true, now)
	if !edge.Decide(time.Second, true) {
		t.Fatal("the cap dropped a continued edge trace")
	}
	edge.Finish(now)
	// The next second has a fresh allowance.
	now = now.Add(time.Second)
	if !decide(false, true) {
		t.Fatal("the cap did not reset after a second")
	}
	// A negative limit turns the cap off.
	off, _, _ := newTestTracer(t, Config{Ratio: 0, TailLimit: -1})
	for range 10 {
		trace, _ := off.Begin(query(), false, now)
		if !trace.Decide(0, true) {
			t.Fatal("an uncapped tracer dropped a failed trace")
		}
		trace.Finish(now)
	}
}

// TestTailCapHoldsInAnyOneSecond: the cap is a sliding second, not a
// calendar second, so a burst on either side of a second boundary cannot
// keep twice the limit; an allowance comes back once the trace it was given
// to is a second old.
func TestTailCapHoldsInAnyOneSecond(t *testing.T) {
	tr, _, _ := newTestTracer(t, Config{Ratio: 0, TailLimit: 3})
	now := time.Unix(1_700_000_000, 0).Add(999 * time.Millisecond)
	tr.now = func() time.Time { return now }
	failed := func() bool {
		trace, _ := tr.Begin(query(), false, now)
		kept := trace.Decide(0, true)
		trace.Finish(now)
		return kept
	}
	for i := range 3 {
		if !failed() {
			t.Fatalf("trace %d of the limit was not kept", i+1)
		}
	}
	now = now.Add(2 * time.Millisecond) // the next calendar second
	if failed() {
		t.Fatal("a fourth tail trace was kept 2 ms after the first three")
	}
	now = now.Add(998 * time.Millisecond) // a second after the first three
	for i := range 3 {
		if !failed() {
			t.Fatalf("trace %d after the window moved on was not kept", i+1)
		}
	}
	if failed() {
		t.Fatal("the cap let a fourth trace through in one second")
	}
	if got := testutil.ToFloat64(tr.metrics.traces[resultTailLimited]); got != 2 {
		t.Fatalf("tail_limited = %v, want 2", got)
	}
}

// TestTailCapIsNotSpentOnQueueFull: a tail trace the full export queue
// drops was never exported, so it gives its allowance back; the cap counts
// the tail traces that reach the queue.
func TestTailCapIsNotSpentOnQueueFull(t *testing.T) {
	tr, _, _ := newTestTracer(t, Config{Ratio: 0, TailLimit: 2, QueueSize: 1})
	now := time.Unix(1_700_000_000, 0)
	tr.now = func() time.Time { return now }
	failed := func() bool {
		trace, _ := tr.Begin(query(), false, now)
		kept := trace.Decide(0, true)
		trace.Finish(now)
		return kept
	}
	// Run is not started: the first trace fills the queue, the next ones
	// find it full.
	for i := range 5 {
		if !failed() {
			t.Fatalf("trace %d was limited although only one reached the queue", i+1)
		}
	}
	if got := testutil.ToFloat64(tr.metrics.traces[resultQueueFull]); got != 4 {
		t.Fatalf("queue_full = %v, want 4", got)
	}
	// The queue empties: one more allowance is left in this second, then
	// the cap holds.
	<-tr.queue
	if !failed() {
		t.Fatal("the second allowance was spent on dropped traces")
	}
	if failed() {
		t.Fatal("a third tail trace reached the queue in one second")
	}
	if got := testutil.ToFloat64(tr.metrics.traces[resultTailLimited]); got != 1 {
		t.Fatalf("tail_limited = %v, want 1", got)
	}
}
