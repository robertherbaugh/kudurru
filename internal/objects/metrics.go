package objects

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// Result label values of kudurru_objects_applied_total.
const (
	ResultCreated   = "created"
	ResultUpdated   = "updated"
	ResultUnchanged = "unchanged"
	ResultDeleted   = "deleted"
	ResultInvalid   = "invalid"
	ResultConflict  = "conflict"
	ResultError     = "error"
)

var results = []string{ResultCreated, ResultUpdated, ResultUnchanged, ResultDeleted, ResultInvalid, ResultConflict, ResultError}

// metrics are the loader's instruments (dev-docs/design-record.md §11.1: fixed,
// bounded label sets; file names are never labels).
type metrics struct {
	files     prometheus.Gauge
	applied   *prometheus.CounterVec
	diagnoses *prometheus.GaugeVec
	lastLoad  prometheus.Gauge
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		files: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kudurru_objects_files",
			Help: "Object sources read by the last load: *.yaml files in objectsDir plus the inline objects of kudurru.yaml when present.",
		}),
		applied: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_objects_applied_total",
			Help: "File-managed object outcomes per load, by result (created, updated, unchanged, deleted, invalid, conflict, error).",
		}, []string{"result"}),
		diagnoses: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kudurru_objects_diagnoses",
			Help: "Diagnoses reported by the last load, by severity.",
		}, []string{"severity"}),
		lastLoad: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kudurru_objects_last_load_timestamp_seconds",
			Help: "Unix time the last load finished.",
		}),
	}
	for _, r := range results {
		m.applied.WithLabelValues(r)
	}
	for _, s := range []Severity{SeverityError, SeverityWarning, SeverityInfo} {
		m.diagnoses.WithLabelValues(string(s))
	}
	if reg != nil {
		m.files = register(reg, m.files)
		m.applied = register(reg, m.applied)
		m.diagnoses = register(reg, m.diagnoses)
		m.lastLoad = register(reg, m.lastLoad)
	}
	return m
}

// register registers c, or returns the collector already registered under
// the same descriptor (a second Loader on the same registry).
func register[C prometheus.Collector](reg prometheus.Registerer, c C) C {
	if err := reg.Register(c); err != nil {
		var are prometheus.AlreadyRegisteredError
		if errors.As(err, &are) {
			if existing, ok := are.ExistingCollector.(C); ok {
				return existing
			}
		}
		panic(err)
	}
	return c
}

// observe records one load.
func (m *metrics) observe(rep Report, files int) {
	m.files.Set(float64(files))
	add := func(result string, n int) {
		if n > 0 {
			m.applied.WithLabelValues(result).Add(float64(n))
		}
	}
	add(ResultCreated, rep.Created)
	add(ResultUpdated, rep.Updated)
	add(ResultUnchanged, rep.Unchanged)
	add(ResultDeleted, rep.Deleted)
	var invalid, conflict, failed int
	for _, d := range rep.Diagnoses {
		if d.Severity == SeverityInfo {
			continue
		}
		switch resultOf(d.Reason) {
		case ResultInvalid:
			invalid++
		case ResultConflict:
			conflict++
		default:
			failed++
		}
	}
	add(ResultInvalid, invalid)
	add(ResultConflict, conflict)
	add(ResultError, failed)
	for _, s := range []Severity{SeverityError, SeverityWarning, SeverityInfo} {
		m.diagnoses.WithLabelValues(string(s)).Set(float64(rep.Count(s)))
	}
	m.lastLoad.Set(float64(rep.Started.Add(rep.Duration).Unix()))
}

// resultOf maps a diagnosis reason to its result label.
func resultOf(r Reason) string {
	switch r {
	case ReasonInvalidYAML, ReasonNotAnObject, ReasonUnknownKind, ReasonInvalid, ReasonDuplicate:
		return ResultInvalid
	case ReasonManagedByOther, ReasonOwnerUnreadable:
		return ResultConflict
	default:
		return ResultError
	}
}
