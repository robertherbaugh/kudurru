package zonecache

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// cacheMetrics are the zonecache instruments (dev-docs/design-record.md §11.1).
// Labels are bounded: kind is one of the watched kinds, builder one of the
// registered builders, result ok|error.
type cacheMetrics struct {
	resyncs         *prometheus.CounterVec
	rebuilds        prometheus.Counter
	rebuildDuration prometheus.Histogram
	buildErrors     *prometheus.CounterVec
	objects         *prometheus.GaugeVec
	serving         prometheus.Gauge
	snapshotWrites  *prometheus.CounterVec
	lastWrite       prometheus.Gauge
}

func newCacheMetrics() *cacheMetrics {
	return &cacheMetrics{
		resyncs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_store_watch_resyncs_total",
			Help: "Watches re-established (full re-list) after the store closed the watch channel, by kind.",
		}, []string{"kind"}),
		rebuilds: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kudurru_zonecache_rebuilds_total",
			Help: "Serving snapshots published by zonecache.",
		}),
		rebuildDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "kudurru_zonecache_rebuild_duration_seconds",
			Help:    "Time to index the object set and run every builder for one snapshot.",
			Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		}),
		buildErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_zonecache_build_errors_total",
			Help: "Builder failures; the builder's previous view stays in service.",
		}, []string{"builder"}),
		objects: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kudurru_zonecache_objects",
			Help: "Objects in the served snapshot, by kind.",
		}, []string{"kind"}),
		serving: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kudurru_store_snapshot_serving",
			Help: "1 while objects are served from the local.db snapshot because the store has not synced.",
		}),
		snapshotWrites: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_store_snapshot_writes_total",
			Help: "local.db snapshot writes, by result (ok, error).",
		}, []string{"result"}),
		lastWrite: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kudurru_store_snapshot_last_write_timestamp_seconds",
			Help: "Unix time of the last successful local.db snapshot write.",
		}),
	}
}

// register registers every instrument plus the snapshot age gauge, whose
// value age computes at scrape time. Series for kinds and results are
// created up front so dashboards see zeros.
func (m *cacheMetrics) register(reg prometheus.Registerer, kinds []store.Kind, age func() float64) error {
	ageGauge := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kudurru_store_snapshot_age_seconds",
		Help: "Age of the local.db snapshot being served; 0 when serving live objects.",
	}, age)
	for _, c := range []prometheus.Collector{m.resyncs, m.rebuilds, m.rebuildDuration, m.buildErrors, m.objects, m.serving, m.snapshotWrites, m.lastWrite, ageGauge} {
		if err := reg.Register(c); err != nil {
			return err
		}
	}
	for _, k := range kinds {
		m.resyncs.WithLabelValues(string(k))
		m.objects.WithLabelValues(string(k))
	}
	m.snapshotWrites.WithLabelValues("ok")
	m.snapshotWrites.WithLabelValues("error")
	return nil
}
