package upstream

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the upstream instruments. The upstream label is
// "<group>/<server name>", the server name being the display name or the
// address as configured, so cardinality is bounded by the configuration and
// resolved IPs never appear (docs/architecture.md §11.1).
type Metrics struct {
	Healthy  *prometheus.GaugeVec
	Requests *prometheus.CounterVec
	Errors   *prometheus.CounterVec
	Duration *prometheus.HistogramVec
}

// NewMetrics registers the upstream metrics with reg. Create them once per
// process and pass them to every Set (reloads build new Sets).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Healthy: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kudurru_upstream_healthy",
			Help: "1 when the upstream server is healthy, 0 while it is in failure backoff.",
		}, []string{"upstream"}),
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_upstream_requests_total",
			Help: "Exchanges attempted with an upstream server.",
		}, []string{"upstream"}),
		Errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_upstream_errors_total",
			Help: "Failed exchanges with an upstream server, by reason (timeout, network, refused, servfail).",
		}, []string{"upstream", "reason"}),
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kudurru_upstream_request_duration_seconds",
			Help:    "Round-trip time of exchanges with an upstream server that returned a response.",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		}, []string{"upstream"}),
	}
	if reg != nil {
		reg.MustRegister(m.Healthy, m.Requests, m.Errors, m.Duration)
	}
	return m
}

// forget removes the series of a server that is no longer configured.
func (m *Metrics) forget(label string) {
	if m == nil {
		return
	}
	m.Healthy.DeleteLabelValues(label)
	m.Requests.DeleteLabelValues(label)
	m.Duration.DeleteLabelValues(label)
	for _, r := range []string{reasonTimeout, reasonNetwork, reasonRefused, reasonServfail} {
		m.Errors.DeleteLabelValues(label, r)
	}
}

// Error reasons.
const (
	reasonTimeout  = "timeout"
	reasonNetwork  = "network"
	reasonRefused  = "refused"
	reasonServfail = "servfail"
)
