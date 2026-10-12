package upstream

import (
	"context"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// BenchmarkStrategies measures one exchange per strategy against three
// in-process UDP fakes on loopback, reporting the median and p99 latency:
//
//   - fast: every server answers at once;
//   - slow-first: the first configured server answers after 2 ms, which
//     strict always pays and the other strategies learn or race around;
//   - jitter: every server sleeps 0-3 ms at random per query, where racing
//     two servers cuts the tail.
func BenchmarkStrategies(b *testing.B) {
	scenarios := []struct {
		name  string
		setup func(fs []*fake)
	}{
		{"fast", func([]*fake) {}},
		{"slow-first", func(fs []*fake) { fs[0].set(answer, 2*time.Millisecond) }},
		{"jitter", func(fs []*fake) {
			for _, f := range fs {
				f.mu.Lock()
				f.jitter = 3 * time.Millisecond
				f.mu.Unlock()
			}
		}},
	}
	for _, sc := range scenarios {
		for _, st := range []Strategy{ParallelBest, Strict, Random, RTTWeighted} {
			b.Run(sc.name+"/"+string(st), func(b *testing.B) {
				fs := []*fake{startFake(b), startFake(b), startFake(b)}
				sc.setup(fs)
				s := newSet(b, group(st, time.Second, fs...), Options{Metrics: NewMetrics(nil)})
				q := s.PrepareQuery(query("bench.example.", dns.TypeA), netip.Addr{})
				ctx := context.Background()
				// Warm connections and RTT estimates.
				for range 20 {
					if _, err := s.Exchange(ctx, DefaultGroup, q); err != nil {
						b.Fatal(err)
					}
				}
				var lat []time.Duration
				b.ReportAllocs()
				for b.Loop() {
					start := time.Now()
					if _, err := s.Exchange(ctx, DefaultGroup, q); err != nil {
						b.Fatal(err)
					}
					lat = append(lat, time.Since(start))
				}
				slices.Sort(lat)
				b.ReportMetric(float64(lat[len(lat)/2].Microseconds()), "p50-us")
				b.ReportMetric(float64(lat[len(lat)*99/100].Microseconds()), "p99-us")
			})
		}
	}
}
