package license

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Collector exports the license as metrics: kudurru_license_info (state and
// plan labels, value 1), kudurru_license_expires_timestamp_seconds (0 without
// a key) and kudurru_license_node_limit. Alert on the expiry before the grace
// period ends.
type Collector struct {
	m                    *Manager
	info, expires, nodes *prometheus.Desc
}

// NewCollector returns a collector over m.
func NewCollector(m *Manager) *Collector {
	return &Collector{
		m:       m,
		info:    prometheus.NewDesc("kudurru_license_info", "The license state of this node (value 1).", []string{"state", "plan"}, nil),
		expires: prometheus.NewDesc("kudurru_license_expires_timestamp_seconds", "Expiry of the license key as a Unix time; 0 without a key.", nil, nil),
		nodes:   prometheus.NewDesc("kudurru_license_node_limit", "Effective node limit of the license.", nil, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.info
	ch <- c.expires
	ch <- c.nodes
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	s := c.m.Status()
	ch <- prometheus.MustNewConstMetric(c.info, prometheus.GaugeValue, 1, string(s.State), string(s.Plan))
	var exp float64
	if s.ExpiresAt != nil {
		exp = float64(s.ExpiresAt.Unix())
	}
	ch <- prometheus.MustNewConstMetric(c.expires, prometheus.GaugeValue, exp)
	ch <- prometheus.MustNewConstMetric(c.nodes, prometheus.GaugeValue, float64(s.Nodes))
}
