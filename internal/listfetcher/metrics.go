package listfetcher

import (
	"github.com/prometheus/client_golang/prometheus"
)

// fetcherMetrics are exported by the leader's fetcher (§11.1). Labels carry
// list names only, never URLs.
type fetcherMetrics struct {
	entries    *prometheus.GaugeVec
	lastUpdate *prometheus.GaugeVec
	failures   *prometheus.CounterVec
	stale      *prometheus.GaugeVec
}

func newFetcherMetrics(reg prometheus.Registerer) (*fetcherMetrics, error) {
	m := &fetcherMetrics{
		entries: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kudurru_blocklist_entries",
			Help: "Unique compiled entries of a BlockList (status.entries).",
		}, []string{"list"}),
		lastUpdate: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kudurru_blocklist_last_update_timestamp_seconds",
			Help: "Unix time of the last successful fetch or revalidation of any source of a BlockList (status.lastFetched).",
		}, []string{"list"}),
		failures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_blocklist_update_failures_total",
			Help: "Failed source downloads of a BlockList (after retries); the previous body stays in service.",
		}, []string{"list"}),
		stale: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kudurru_blocklist_stale",
			Help: "1 when a BlockList is served from an older body or without a source because its last download failed.",
		}, []string{"list"}),
	}
	if reg != nil {
		for _, c := range []prometheus.Collector{m.entries, m.lastUpdate, m.failures, m.stale} {
			if err := reg.Register(c); err != nil {
				return nil, err
			}
		}
	}
	return m, nil
}

func (m *fetcherMetrics) forget(list string) {
	m.entries.DeleteLabelValues(list)
	m.lastUpdate.DeleteLabelValues(list)
	m.failures.DeleteLabelValues(list)
	m.stale.DeleteLabelValues(list)
}

// resolverMetrics are exported by every node's resolver.
type resolverMetrics struct {
	pulls    *prometheus.CounterVec
	rebuilds prometheus.Counter
	pending  prometheus.Gauge
}

func newResolverMetrics(reg prometheus.Registerer) (*resolverMetrics, error) {
	m := &resolverMetrics{
		pulls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_blocklist_pulls_total",
			Help: "List bodies obtained by this node, by origin (leader: pulled by digest over :8082; self: fetched after the fallback delay) and result.",
		}, []string{"origin", "result"}),
		rebuilds: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kudurru_blocklist_rebuilds_total",
			Help: "Blocking snapshots compiled and handed to zonecache by this node.",
		}),
		pending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kudurru_blocklist_pending_sources",
			Help: "List sources this node is still waiting for (no body, or not the published digest).",
		}),
	}
	if reg != nil {
		for _, c := range []prometheus.Collector{m.pulls, m.rebuilds, m.pending} {
			if err := reg.Register(c); err != nil {
				return nil, err
			}
		}
	}
	return m, nil
}
