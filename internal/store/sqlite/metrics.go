package sqlite

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// Metrics are the kudurru_store_* instruments of the driver
// (docs/architecture.md §11.1). Create them once per process with
// NewMetrics and pass them in Options.Metrics; nil disables them.
type Metrics struct {
	// Operations counts store calls by op (get, list, apply, delete,
	// update_status, watch, snapshot, restore) and result (ok, not_found,
	// conflict, managed_by, invalid, closed, canceled, error).
	Operations *prometheus.CounterVec
	// Duration is the latency of the same calls by op; for apply, delete
	// and update_status it is the write latency including the commit.
	Duration *prometheus.HistogramVec
	// Catchups is kudurru_store_watch_catchups_total{kind}: watchers that
	// fell behind the in-process broadcast and caught up from the change
	// feed (or relisted after compaction) without closing, the feed-lag
	// signal. kudurru_store_watch_resyncs_total{kind}, the consumer-side
	// re-list after a watch closed, is exported by zonecache; sharing the
	// name would make the two registrations collide.
	Catchups *prometheus.CounterVec
	// Watchers is the number of open watches.
	Watchers prometheus.Gauge
}

// Store operations (the op label).
const (
	opGet          = "get"
	opList         = "list"
	opApply        = "apply"
	opDelete       = "delete"
	opUpdateStatus = "update_status"
	opWatch        = "watch"
	opSnapshot     = "snapshot"
	opRestore      = "restore"
)

// NewMetrics registers the store metrics with reg (nil: unregistered).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Operations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_store_operations_total",
			Help: "Store calls by operation and result.",
		}, []string{"op", "result"}),
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kudurru_store_operation_duration_seconds",
			Help:    "Latency of store calls by operation (writes include the commit).",
			Buckets: []float64{.0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		}, []string{"op"}),
		Catchups: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_store_watch_catchups_total",
			Help: "Watchers that fell behind the live broadcast and caught up from the change feed (or relisted after compaction) without closing, by kind.",
		}, []string{"kind"}),
		Watchers: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kudurru_store_watchers",
			Help: "Open store watches.",
		}),
	}
	if reg != nil {
		reg.MustRegister(m.Operations, m.Duration, m.Catchups, m.Watchers)
	}
	return m
}

// observe records one call; use as `defer s.observe(op, time.Now(), &err)`.
func (s *Store) observe(op string, start time.Time, errp *error) {
	m := s.opts.Metrics
	if m == nil {
		return
	}
	var err error
	if errp != nil {
		err = *errp
	}
	m.Operations.WithLabelValues(op, resultOf(err)).Inc()
	m.Duration.WithLabelValues(op).Observe(time.Since(start).Seconds())
}

// catchupObserved counts one watcher catch-up for kind.
func (s *Store) catchupObserved(kind store.Kind) {
	if m := s.opts.Metrics; m != nil {
		m.Catchups.WithLabelValues(string(kind)).Inc()
	}
}

// watchersAdd moves the open-watch gauge.
func (s *Store) watchersAdd(d float64) {
	if m := s.opts.Metrics; m != nil {
		m.Watchers.Add(d)
	}
}

// resultOf maps an error onto the bounded result label.
func resultOf(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, store.ErrNotFound):
		return "not_found"
	case errors.Is(err, store.ErrConflict):
		return "conflict"
	case store.IsManagedBy(err):
		return "managed_by"
	case validator.IsInvalid(err):
		return "invalid"
	case errors.Is(err, store.ErrClosed):
		return "closed"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	}
	return "error"
}
