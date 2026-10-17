package cache

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the cache instruments (dev-docs/design-record.md §11.1). All
// methods are safe on a nil *Metrics.
type Metrics struct {
	Entries    *prometheus.GaugeVec   // kind
	Hits       *prometheus.CounterVec // kind, stale
	Requests   prometheus.Counter
	Evictions  *prometheus.CounterVec // reason
	Prefetches prometheus.Counter

	// Pre-resolved children, indexed by Kind.
	entriesBy [2]prometheus.Gauge
	freshBy   [2]prometheus.Counter
	staleBy   [2]prometheus.Counter
	evictBy   map[string]prometheus.Counter
}

// NewMetrics registers the cache metrics with reg (nil: unregistered).
// Create them once per process; a cache rebuilt on reload reuses them.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Entries: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kudurru_cache_entries",
			Help: "Cached responses, by kind (positive, negative); includes entries kept for serve-stale.",
		}, []string{"kind"}),
		Hits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_cache_hits_total",
			Help: "Queries answered from the cache, by kind and whether the answer was stale (RFC 8767).",
		}, []string{"kind", "stale"}),
		Requests: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kudurru_cache_requests_total",
			Help: "Cache lookups for cacheable queries; the hit ratio is kudurru_cache_hits_total over this.",
		}),
		Evictions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_cache_evictions_total",
			Help: "Entries removed from the cache, by reason (capacity, expired, flush).",
		}, []string{"reason"}),
		Prefetches: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kudurru_cache_prefetches_total",
			Help: "Background refreshes of hot entries started before expiry.",
		}),
		evictBy: map[string]prometheus.Counter{},
	}
	for _, k := range []Kind{Positive, Negative} {
		m.entriesBy[k] = m.Entries.WithLabelValues(k.String())
		m.freshBy[k] = m.Hits.WithLabelValues(k.String(), "false")
		m.staleBy[k] = m.Hits.WithLabelValues(k.String(), "true")
	}
	for _, r := range []string{ReasonCapacity, ReasonExpired, ReasonFlush} {
		m.evictBy[r] = m.Evictions.WithLabelValues(r)
	}
	if reg != nil {
		reg.MustRegister(m.Entries, m.Requests, m.Hits, m.Evictions, m.Prefetches)
	}
	return m
}

func (m *Metrics) request() {
	if m != nil {
		m.Requests.Inc()
	}
}

func (m *Metrics) hit(k Kind, stale bool) {
	if m == nil {
		return
	}
	if stale {
		m.staleBy[k].Inc()
	} else {
		m.freshBy[k].Inc()
	}
}

func (m *Metrics) entries(k Kind, delta float64) {
	if m != nil {
		m.entriesBy[k].Add(delta)
	}
}

// evicted counts one removal and lowers the entry gauge.
func (m *Metrics) evicted(reason string, k Kind) { m.evictedN(reason, k, 1) }

func (m *Metrics) evictedN(reason string, k Kind, n int) {
	if m == nil || n == 0 {
		return
	}
	m.evictBy[reason].Add(float64(n))
	m.entriesBy[k].Sub(float64(n))
}

// Prefetched counts a started prefetch.
func (m *Metrics) Prefetched() {
	if m != nil {
		m.Prefetches.Inc()
	}
}
