package secondary

import "github.com/prometheus/client_golang/prometheus"

// Collector describes independent secondary health on this node. It reads the
// current clock at scrape time, so expiry is visible between scheduler ticks.
type Collector struct {
	manager                                  *Manager
	available, expires, confirmed, refreshes *prometheus.Desc
}

// NewCollector returns a collector to register once for the node's manager.
func NewCollector(m *Manager) *Collector {
	return &Collector{manager: m,
		available: prometheus.NewDesc("kudurru_secondary_available", "Whether this node has an unexpired secondary copy.", []string{"zone"}, nil),
		expires:   prometheus.NewDesc("kudurru_secondary_expires_timestamp_seconds", "Absolute expiry of this node's last secondary copy; zero before initial transfer.", []string{"zone"}, nil),
		confirmed: prometheus.NewDesc("kudurru_secondary_confirmed_timestamp_seconds", "Last successful secondary confirmation on this node; zero before initial transfer.", []string{"zone"}, nil),
		refreshes: prometheus.NewDesc("kudurru_secondary_refresh_total", "Completed secondary refresh attempts on this node, reset when configuration changes.", []string{"zone", "result"}, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.available
	ch <- c.expires
	ch <- c.confirmed
	ch <- c.refreshes
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	for _, st := range c.manager.Statuses() {
		available, expires, confirmed := 0.0, 0.0, 0.0
		if st.Available {
			available = 1
		}
		if !st.ConfirmedAt.IsZero() {
			confirmed = float64(st.ConfirmedAt.Unix())
			expires = float64(st.ExpiresAt.Unix())
		}
		ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, available, st.Zone)
		ch <- prometheus.MustNewConstMetric(c.expires, prometheus.GaugeValue, expires, st.Zone)
		ch <- prometheus.MustNewConstMetric(c.confirmed, prometheus.GaugeValue, confirmed, st.Zone)
		ch <- prometheus.MustNewConstMetric(c.refreshes, prometheus.CounterValue, float64(st.successes), st.Zone, "success")
		ch <- prometheus.MustNewConstMetric(c.refreshes, prometheus.CounterValue, float64(st.failures), st.Zone, "error")
	}
}
