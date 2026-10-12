package central

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

type metrics struct {
	up     prometheus.Gauge
	schema prometheus.Gauge
	errors *prometheus.CounterVec
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		up: prometheus.NewGauge(prometheus.GaugeOpts{Name: "kudurru_central_up",
			Help: "1 when the central database is reachable and migrated, else 0."}),
		schema: prometheus.NewGauge(prometheus.GaugeOpts{Name: "kudurru_central_schema_version",
			Help: "Schema version of the central database."}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kudurru_central_errors_total",
			Help: "Central database failures by operation (connect, migrate, ping)."}, []string{"op"}),
	}
	if reg != nil {
		m.up = register(reg, m.up)
		m.schema = register(reg, m.schema)
		m.errors = register(reg, m.errors)
	}
	return m
}

// register registers c, or returns the collector already registered
// under its name.
func register[C prometheus.Collector](reg prometheus.Registerer, c C) C {
	if err := reg.Register(c); err != nil {
		var are prometheus.AlreadyRegisteredError
		if errors.As(err, &are) {
			if prev, ok := are.ExistingCollector.(C); ok {
				return prev
			}
		}
	}
	return c
}
