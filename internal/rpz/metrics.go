package rpz

import "github.com/prometheus/client_golang/prometheus"

// Collector describes this node's RPZ feeds (D391). It reads the manager at
// scrape time, so expiry is visible between scheduler ticks. Labels are the
// BlockList (namespace/name) and the source name; no primary or error text.
type Collector struct {
	manager                               *Manager
	available, expires, confirmed, serial *prometheus.Desc
	rules, ignored                        *prometheus.Desc
}

// NewCollector returns a collector to register once for the node's manager.
func NewCollector(m *Manager) *Collector {
	labels := []string{"list", "source"}
	return &Collector{manager: m,
		available: prometheus.NewDesc("kudurru_rpz_feed_available", "Whether this node can apply the RPZ feed now (an unexpired copy and a current configuration).", labels, nil),
		expires:   prometheus.NewDesc("kudurru_rpz_feed_expires_timestamp_seconds", "Absolute expiry of this node's RPZ copy; zero before the first transfer.", labels, nil),
		confirmed: prometheus.NewDesc("kudurru_rpz_feed_confirmed_timestamp_seconds", "Last confirmation of this node's RPZ copy; zero before the first transfer.", labels, nil),
		serial:    prometheus.NewDesc("kudurru_rpz_feed_serial", "SOA serial of this node's RPZ copy; absent before the first transfer.", labels, nil),
		rules:     prometheus.NewDesc("kudurru_rpz_feed_rules", "Compiled policy owners of this node's RPZ copy.", labels, nil),
		ignored:   prometheus.NewDesc("kudurru_rpz_feed_ignored_rules", "Policy owners of this node's RPZ copy ignored by the compiler, per reason.", []string{"list", "source", "reason"}, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.available, c.expires, c.confirmed, c.serial, c.rules, c.ignored} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	// Source names are not unique by admission; a duplicate series would
	// fail the whole scrape, so only the first source of a name is exported.
	seen := map[[2]string]bool{}
	for _, st := range c.manager.Statuses() {
		key := [2]string{st.List, st.Source}
		if seen[key] {
			continue
		}
		seen[key] = true
		available, expires, confirmed := 0.0, 0.0, 0.0
		if st.Available {
			available = 1
		}
		if !st.ConfirmedAt.IsZero() {
			confirmed, expires = float64(st.ConfirmedAt.Unix()), float64(st.ExpiresAt.Unix())
		}
		ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, available, st.List, st.Source)
		ch <- prometheus.MustNewConstMetric(c.expires, prometheus.GaugeValue, expires, st.List, st.Source)
		ch <- prometheus.MustNewConstMetric(c.confirmed, prometheus.GaugeValue, confirmed, st.List, st.Source)
		if st.ConfirmedAt.IsZero() {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.serial, prometheus.GaugeValue, float64(st.Serial), st.List, st.Source)
		ch <- prometheus.MustNewConstMetric(c.rules, prometheus.GaugeValue, float64(st.Stats.Rules), st.List, st.Source)
		for _, ig := range st.IgnoredReasons() {
			ch <- prometheus.MustNewConstMetric(c.ignored, prometheus.GaugeValue, float64(ig.Count), st.List, st.Source, ig.Reason)
		}
	}
}
