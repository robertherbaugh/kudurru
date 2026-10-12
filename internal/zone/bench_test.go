package zone

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// benchRecords is the size of the benchmark zone (docs/m1/zone.md).
const benchRecords = 100_000

// largeEndpoints returns endpoints for exactly benchRecords records below
// bench.example.: 80k hosts with one A record spread over 100 subdomains
// (so the tree has empty non-terminals), 10k AAAA on some of those hosts,
// 5k CNAMEs to hosts, 3k TXT, 1k SRV and 1k wildcard A records.
func largeEndpoints() []v1alpha1.Endpoint {
	eps := make([]v1alpha1.Endpoint, 0, benchRecords)
	host := func(i int) string { return fmt.Sprintf("h%d.r%d.bench.example", i, i%100) }
	for i := range 80_000 {
		eps = append(eps, ep(host(i), "A", fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255)))
	}
	for i := range 10_000 {
		eps = append(eps, ep(host(i*8), "AAAA", fmt.Sprintf("2001:db8::%x", i)))
	}
	for i := range 5_000 {
		eps = append(eps, ep(fmt.Sprintf("c%d.bench.example", i), "CNAME", host(i*16)))
	}
	for i := range 3_000 {
		eps = append(eps, ep(fmt.Sprintf("t%d.r%d.bench.example", i, i%100), "TXT", fmt.Sprintf("v=spf1 ip4:10.0.%d.0/24 -all", i&255)))
	}
	for i := range 1_000 {
		eps = append(eps, ep(fmt.Sprintf("_svc%d._tcp.bench.example", i), "SRV", fmt.Sprintf("10 5 %d %s", 1000+i, host(i))))
	}
	for i := range 1_000 {
		eps = append(eps, ep(fmt.Sprintf("*.w%d.bench.example", i), "A", "192.0.2.1"))
	}
	return eps
}

func benchZoneSpec() *v1alpha1.DNSZone { return dnsZone("bench.example.") }

var (
	largeOnce sync.Once
	largeZone *Zone
)

func benchZone(b *testing.B) *Zone {
	largeOnce.Do(func() {
		largeZone = mustBuild(b, benchZoneSpec(), largeEndpoints()...)
	})
	return largeZone
}

// sink keeps the compiler from discarding lookups.
var sink Result

// BenchmarkLookup100k measures Zone.Lookup on a 100k-record zone for each
// result kind, with qnames drawn at random so the tree walk is not served
// from one hot path. It reports ns/op and allocations from the testing
// package and per-call p50/p99 latency (including ~20-30 ns of time.Now
// overhead) as custom metrics.
func BenchmarkLookup100k(b *testing.B) {
	z := benchZone(b)
	if n := z.RecordCount(); n < benchRecords {
		b.Fatalf("zone has %d records, want >= %d", n, benchRecords)
	}
	r := rand.New(rand.NewPCG(7, 7)) //nolint:gosec // G404: deterministic benchmark input
	pick := func(n int, f func(int) string) []string {
		out := make([]string, 4096)
		for i := range out {
			out[i] = f(r.IntN(n))
		}
		return out
	}
	for _, bc := range []struct {
		name  string
		qtype uint16
		kind  Kind
		names []string
	}{
		{"hit", dns.TypeA, Success, pick(80_000, func(i int) string { return fmt.Sprintf("h%d.r%d.bench.example.", i, i%100) })},
		{"cname", dns.TypeA, Success, pick(5_000, func(i int) string { return fmt.Sprintf("c%d.bench.example.", i) })},
		{"wildcard", dns.TypeA, Success, pick(1_000, func(i int) string { return fmt.Sprintf("x%d.w%d.bench.example.", i, i) })},
		{"nodata", dns.TypeMX, NoData, pick(80_000, func(i int) string { return fmt.Sprintf("h%d.r%d.bench.example.", i, i%100) })},
		{"nxdomain", dns.TypeA, NXDomain, pick(80_000, func(i int) string { return fmt.Sprintf("n%d.r%d.bench.example.", i, i%100) })},
	} {
		b.Run(bc.name, func(b *testing.B) {
			if res := z.Lookup(bc.names[0], bc.qtype); res.Kind != bc.kind {
				b.Fatalf("%s: kind %v, want %v", bc.names[0], res.Kind, bc.kind)
			}
			lat := make([]time.Duration, 0, 1<<20)
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				q := bc.names[i&4095]
				t0 := time.Now()
				sink = z.Lookup(q, bc.qtype)
				lat = append(lat, time.Since(t0))
				i++
			}
			slices.Sort(lat)
			b.ReportMetric(float64(lat[len(lat)/2].Nanoseconds()), "p50-ns")
			b.ReportMetric(float64(lat[len(lat)*99/100].Nanoseconds()), "p99-ns")
		})
	}
}

// BenchmarkBuild100k measures building the 100k-record zone from
// endpoints (parse, tree insert, normalise, content hash) and reports the
// heap the built zone retains per record.
func BenchmarkBuild100k(b *testing.B) {
	eps := largeEndpoints()
	dz := benchZoneSpec()
	var perRecord float64
	b.ReportAllocs()
	for b.Loop() {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		z := mustBuild(b, dz, eps...)
		runtime.GC()
		runtime.ReadMemStats(&after)
		perRecord = float64(after.HeapAlloc-before.HeapAlloc) / float64(z.RecordCount())
		runtime.KeepAlive(z)
	}
	b.ReportMetric(perRecord, "B/record")
}

// BenchmarkExportBIND100k measures exporting the 100k-record zone.
func BenchmarkExportBIND100k(b *testing.B) {
	z := benchZone(b)
	b.ReportAllocs()
	for b.Loop() {
		if err := ExportBIND(discard{}, z); err != nil {
			b.Fatal(err)
		}
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
