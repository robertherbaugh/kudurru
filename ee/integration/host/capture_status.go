package host

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

type captureMetrics struct {
	total    *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newCaptureMetrics(reg prometheus.Registerer) (captureMetrics, error) {
	if reg == nil {
		return captureMetrics{}, nil
	}
	metrics := captureMetrics{
		total: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_integration_captures_total",
			Help: "Integration host refresh captures by result.",
		}, []string{"result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kudurru_integration_capture_seconds",
			Help:    "Time to capture and validate a complete integration host frame by result.",
			Buckets: []float64{.005, .025, .1, .5, 1, 2, 5, 10, 30, 60},
		}, []string{"result"}),
	}
	if err := reg.Register(metrics.total); err != nil {
		return captureMetrics{}, err
	}
	if err := reg.Register(metrics.duration); err != nil {
		reg.Unregister(metrics.total)
		return captureMetrics{}, err
	}
	return metrics, nil
}

func (m captureMetrics) observe(reason string, duration time.Duration) {
	if m.total == nil {
		return
	}
	if reason == "" {
		reason = "Success"
	}
	m.total.WithLabelValues(reason).Inc()
	m.duration.WithLabelValues(reason).Observe(duration.Seconds())
}

func captureFailureReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "CaptureDeadlineExceeded"
	case errors.Is(err, context.Canceled):
		return "CaptureCanceled"
	case errors.Is(err, ErrCapacity), errors.Is(err, replication.ErrTooLarge):
		return "CaptureCapacityExceeded"
	case errors.Is(err, ErrInvalid), errors.Is(err, replication.ErrInvalid), errors.Is(err, replication.ErrIncompatible):
		return "CaptureInvalid"
	case errors.Is(err, ErrCredentials):
		return "CaptureCredentialsUnavailable"
	case errors.Is(err, ErrFenced), errors.Is(err, replication.ErrFenced):
		return "CaptureFenced"
	case errors.Is(err, ErrUnavailable), errors.Is(err, replication.ErrReadOnly):
		return "CaptureUnavailable"
	default:
		return "CaptureFailed"
	}
}
