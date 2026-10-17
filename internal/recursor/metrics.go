package recursor

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics are the resolver's own instruments; the upstream metrics also
// count its answers under the server "<group>/recursive".
type Metrics struct {
	// Outgoing is kudurru_recursor_outgoing_queries_total{result}: queries
	// sent to authoritative servers (ok, timeout, error).
	Outgoing *prometheus.CounterVec
	// Validations is kudurru_recursor_validations_total{result}: answers
	// by DNSSEC state (secure, insecure, bogus).
	Validations *prometheus.CounterVec
	// CacheEntries is kudurru_recursor_cache_entries: cached delegations,
	// server addresses and keys.
	CacheEntries prometheus.GaugeFunc

	ok, timeout, failed prometheus.Counter
	secure, insecure    prometheus.Counter
	bogus, unknown      prometheus.Counter
}

// NewMetrics registers the resolver metrics with reg. cacheLen reports the
// cache size (nil reports 0); create the metrics once per process.
func NewMetrics(reg prometheus.Registerer, cacheLen func() int) *Metrics {
	m := &Metrics{
		Outgoing: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_recursor_outgoing_queries_total",
			Help: "Queries the recursive resolver sent to authoritative name servers, by result (ok, timeout, error).",
		}, []string{"result"}),
		Validations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_recursor_validations_total",
			Help: "Answers of the recursive resolver by DNSSEC state (secure, insecure, bogus, unknown).",
		}, []string{"result"}),
		CacheEntries: prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "kudurru_recursor_cache_entries",
			Help: "Delegations, name-server addresses and DNSSEC keys cached by the recursive resolver.",
		}, func() float64 {
			if cacheLen == nil {
				return 0
			}
			return float64(cacheLen())
		}),
	}
	m.ok = m.Outgoing.WithLabelValues("ok")
	m.timeout = m.Outgoing.WithLabelValues("timeout")
	m.failed = m.Outgoing.WithLabelValues("error")
	m.secure = m.Validations.WithLabelValues("secure")
	m.insecure = m.Validations.WithLabelValues("insecure")
	m.bogus = m.Validations.WithLabelValues("bogus")
	m.unknown = m.Validations.WithLabelValues("unknown")
	if reg != nil {
		reg.MustRegister(m.Outgoing, m.Validations, m.CacheEntries)
	}
	return m
}

func (m *Metrics) outgoing(timeout bool, err error) {
	if m == nil || errors.Is(err, errBudget) {
		return
	}
	switch {
	case err == nil:
		m.ok.Inc()
	case timeout:
		m.timeout.Inc()
	default:
		m.failed.Inc()
	}
}

func (m *Metrics) validated(s Security) {
	if m == nil {
		return
	}
	switch s {
	case SecSecure:
		m.secure.Inc()
	case SecInsecure:
		m.insecure.Inc()
	case SecBogus:
		m.bogus.Inc()
	default:
		m.unknown.Inc()
	}
}
