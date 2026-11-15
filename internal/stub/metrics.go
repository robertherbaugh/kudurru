package stub

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Collector reports node-local routing health. Labels contain configuration
// object names only; discovered addresses/hosts never create metric series.
type Collector struct {
	manager                                             *Manager
	available, expires, confirmed, nextAttempt, servers *prometheus.Desc
}

// NewCollector returns a collector to register once for the node's manager.
func NewCollector(manager *Manager) *Collector {
	return &Collector{manager: manager,
		available:   prometheus.NewDesc("kudurru_stub_available", "Whether this node has valid unexpired stub routing.", []string{"zone"}, nil),
		expires:     prometheus.NewDesc("kudurru_stub_expires_timestamp_seconds", "Absolute stub routing expiry; zero before discovery.", []string{"zone"}, nil),
		confirmed:   prometheus.NewDesc("kudurru_stub_confirmed_timestamp_seconds", "Last successful stub discovery; zero before discovery.", []string{"zone"}, nil),
		nextAttempt: prometheus.NewDesc("kudurru_stub_next_attempt_timestamp_seconds", "Next scheduled stub discovery attempt; zero when unscheduled.", []string{"zone"}, nil),
		servers:     prometheus.NewDesc("kudurru_stub_discovered_servers", "Number of captured discovered authoritative endpoints.", []string{"zone"}, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.available
	ch <- c.expires
	ch <- c.confirmed
	ch <- c.nextAttempt
	ch <- c.servers
}

// Collect checks expiry at scrape time, independently of scheduler ticks.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	if c.manager == nil {
		return
	}
	for _, st := range c.manager.Statuses() {
		available := 0.0
		if st.Available {
			available = 1
		}
		ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, available, st.Zone)
		ch <- prometheus.MustNewConstMetric(c.expires, prometheus.GaugeValue, timestamp(st.ExpiresAt), st.Zone)
		ch <- prometheus.MustNewConstMetric(c.confirmed, prometheus.GaugeValue, timestamp(st.ConfirmedAt), st.Zone)
		ch <- prometheus.MustNewConstMetric(c.nextAttempt, prometheus.GaugeValue, timestamp(st.NextRefresh), st.Zone)
		ch <- prometheus.MustNewConstMetric(c.servers, prometheus.GaugeValue, float64(len(st.Servers)), st.Zone)
	}
}

func timestamp(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.Unix())
}
