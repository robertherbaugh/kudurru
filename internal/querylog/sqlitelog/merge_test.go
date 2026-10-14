package sqlitelog

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"
)

func TestDistinctMerge(t *testing.T) {
	for _, n := range []int{100, 10000, 40000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			var a, b Distinct
			for i := 0; i < n; i++ {
				a.hash(fmt.Sprint(i))
				b.hash(fmt.Sprint(i + n/2))
			}
			a.finish()
			b.finish()
			// Exercise the wire format, which must not rely on the local exact map.
			wire, err := json.Marshal(b)
			if err != nil {
				t.Fatal(err)
			}
			var decoded Distinct
			if err := json.Unmarshal(wire, &decoded); err != nil {
				t.Fatal(err)
			}
			a.Merge(decoded)
			count, approx := a.Count()
			expected := float64(n + n/2)
			if math.Abs(float64(count)-expected)/expected > 0.02 {
				t.Fatalf("count %d, expected %.0f", count, expected)
			}
			if n == 100 && (approx || count != 150) {
				t.Fatalf("small union must be exact: %d %v", count, approx)
			}
			if n > 10000 && (!approx || len(a.Registers) != 1<<sketchBits || len(a.Values) != 0 || a.exact != nil) {
				t.Fatalf("large union is not bounded")
			}
			a.Merge(decoded)
			again, _ := a.Count()
			if count != again {
				t.Fatalf("union is not idempotent: %d, %d", count, again)
			}
		})
	}
}
func TestLatencyMerge(t *testing.T) {
	a := LatencyMerge{Sum: 1000, Count: 1}
	a.Buckets[1] = 1
	b := LatencyMerge{Sum: 900000, Count: 9}
	b.Buckets[7] = 9
	a.Add(b)
	got := a.Summary()
	if got.AvgMS != 90.1 || got.P50MS != 100 || got.P95MS != 100 {
		t.Fatalf("merged latency %+v", got)
	}
}

func TestTopCandidates(t *testing.T) {
	w, _ := newWriter(t, t0)
	for i := 0; i < 110; i++ {
		ev := event(t0, "10.0.0.1", fmt.Sprintf("domain-%03d.test", i))
		ev.ID = uint64(i + 1)
		mustWrite(t, w, ev)
	}
	q := TopQuery{Window: Window{From: t0.Add(-time.Hour), Until: t0.Add(time.Minute)}, N: 400}
	normal, err := w.Reader().TopDomains(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	q.Candidates = true
	shard, err := w.Reader().TopDomains(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(normal.Items) != 100 || len(shard.Items) != 110 {
		t.Fatalf("normal %d, shard %d", len(normal.Items), len(shard.Items))
	}
}
