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
