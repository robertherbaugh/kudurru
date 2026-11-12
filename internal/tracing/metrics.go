package tracing

import (
	"context"
	"errors"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Label values of kudurru_tracing_traceparent_total.
const (
	resultAccepted  = "accepted"
	resultUntrusted = "untrusted"
	resultIgnored   = "ignored"
	resultMalformed = "malformed"
)

// Label values of kudurru_tracing_traces_total.
const (
	resultNotKept   = "not_kept"
	resultExported  = "exported"
	resultQueueFull = "queue_full"
	resultFailed    = "export_failed"
	resultShutdown  = "shutdown"
)

type metrics struct {
	parent map[string]prometheus.Counter
	traces map[string]prometheus.Counter
}

func newMetrics(reg prometheus.Registerer, queue func() float64) (*metrics, error) {
	parent := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kudurru_tracing_traceparent_total",
		Help: "Queries that carried an EDNS TRACEPARENT option, by result: accepted (a trusted edge peer, the trace continues), untrusted (any other sender; removed and ignored), ignored (tracing.acceptTraceparent is false) or malformed.",
	}, []string{"result"})
	traces := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kudurru_tracing_traces_total",
		Help: "Traced queries, by result: not_kept (neither sampled, failed nor slow), exported, queue_full (dropped because the export queue was full), export_failed (the collector refused or did not answer the batch) or shutdown.",
	}, []string{"result"})
	length := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "kudurru_tracing_queue_length",
		Help: "Kept traces waiting for export.",
	}, queue)
	m := &metrics{parent: map[string]prometheus.Counter{}, traces: map[string]prometheus.Counter{}}
	for _, r := range []string{resultAccepted, resultUntrusted, resultIgnored, resultMalformed} {
		m.parent[r] = parent.WithLabelValues(r)
	}
	for _, r := range []string{resultNotKept, resultExported, resultQueueFull, resultFailed, resultShutdown} {
		m.traces[r] = traces.WithLabelValues(r)
	}
	if reg != nil {
		for _, c := range []prometheus.Collector{parent, traces, length} {
			if err := reg.Register(c); err != nil {
				var already prometheus.AlreadyRegisteredError
				if !errors.As(err, &already) {
					return nil, err
				}
			}
		}
	}
	return m, nil
}

func (m *metrics) traceparent(result string) { m.parent[result].Inc() }

func (m *metrics) trace(result string, n int) {
	if n > 0 {
		m.traces[result].Add(float64(n))
	}
}

func (m *metrics) unkept() { m.traces[resultNotKept].Inc() }

// Memory is an Exporter that keeps every exported trace, for tests.
type Memory struct {
	mu     sync.Mutex
	traces []*Trace
	// Fail, when set, makes Export return it.
	Fail error
}

// Export implements Exporter.
func (m *Memory) Export(_ context.Context, traces []*Trace) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	m.traces = append(m.traces, traces...)
	return nil
}

// Traces returns the exported traces in export order.
func (m *Memory) Traces() []*Trace {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*Trace(nil), m.traces...)
}
