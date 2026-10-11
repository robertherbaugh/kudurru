package querylog

import "github.com/prometheus/client_golang/prometheus"

// Drop stages for kudurru_querylog_dropped_total{stage,sink}.
const (
	// DropStageQueue counts events the collector queue refused.
	DropStageQueue = "queue"
	// DropStageSink counts events in batches a sink failed to write.
	DropStageSink = "sink"
	// DropStageTail counts events a slow live-tail subscriber missed.
	DropStageTail = "tail"
)

// Metrics are the query-log instruments of docs/architecture.md §11.1. The
// collector (Async), the live tail and the SQLite writer share one set so
// every name is registered exactly once.
type Metrics struct {
	Events        prometheus.Counter
	Dropped       *prometheus.CounterVec
	BatchRows     prometheus.Histogram
	WriteDuration prometheus.Histogram
	DBBytes       prometheus.Gauge
	WALBytes      prometheus.Gauge
	Rows          prometheus.Gauge
	OldestTS      prometheus.Gauge
}

// NewMetrics creates the instruments and registers them with reg when reg
// is non-nil.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Events: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kudurru_querylog_events_total",
			Help: "Query events accepted by the query-log collector.",
		}),
		Dropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_querylog_dropped_total",
			Help: "Query events dropped, by pipeline stage (queue, sink, tail) and sink.",
		}, []string{"stage", "sink"}),
		BatchRows: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "kudurru_querylog_batch_rows",
			Help:    "Rows per query-log write batch.",
			Buckets: []float64{1, 10, 50, 100, 250, 500, 1000, 2500, 5000, 10000},
		}),
		WriteDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "kudurru_querylog_write_duration_seconds",
			Help:    "Time to commit one query-log write batch.",
			Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		}),
		DBBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kudurru_querylog_db_bytes",
			Help: "Size of querylog.db (page_count × page_size).",
		}),
		WALBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kudurru_querylog_wal_bytes",
			Help: "Size of the querylog.db write-ahead log.",
		}),
		Rows: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kudurru_querylog_rows",
			Help: "Rows in the query fact table.",
		}),
		OldestTS: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kudurru_querylog_oldest_timestamp_seconds",
			Help: "Unix time of the oldest row in the query fact table.",
		}),
	}
	if reg != nil {
		reg.MustRegister(m.Events, m.Dropped, m.BatchRows, m.WriteDuration, m.DBBytes, m.WALBytes, m.Rows, m.OldestTS)
	}
	return m
}

// Drop adds n to kudurru_querylog_dropped_total{stage,sink}; nil-safe.
func (m *Metrics) Drop(stage, sink string, n int) {
	if m != nil && n > 0 {
		m.Dropped.WithLabelValues(stage, sink).Add(float64(n))
	}
}
