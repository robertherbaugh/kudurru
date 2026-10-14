package zoneview

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// Served returns the authoritative zones (primary and secondary; forward
// zones only route queries) this node serves from c, sorted by name, or
// nil while the cache is unsynced. Edge supervisors poll it through GET
// /internal/zones and the zone metrics read it.
func Served(c *zonecache.Cache, copies ...*secondary.Manager) []*zone.Zone {
	snap := c.Snapshot()
	if snap == nil || snap.State == zonecache.Unsynced {
		return nil
	}
	zs, ok := zonecache.View[*zone.Snapshot](snap, ViewName)
	if !ok || zs == nil {
		return nil
	}
	var out []*zone.Zone
	var transfers *secondary.Snapshot
	if len(copies) > 0 && copies[0] != nil {
		transfers = copies[0].Snapshot()
	}
	for _, z := range zs.Zones() {
		if z.Type() == v1alpha1.ZoneTypeSecondary {
			if !snap.Authoritative() {
				continue
			}
			z = transfers.Lookup(z.SecondaryIdentity())
			if z == nil || !z.AvailableAt(time.Now()) {
				continue
			}
		}
		switch z.Type() {
		case v1alpha1.ZoneTypePrimary, v1alpha1.ZoneTypeSecondary:
			out = append(out, z)
		}
	}
	return out
}

// Metrics label values of kudurru_zone_records{kind}.
const (
	// RecordsKindRecords counts resource records.
	RecordsKindRecords = "records"
	// RecordsKindNames counts owner names.
	RecordsKindNames = "names"
)

// Collector exports kudurru_zone_serial{zone} (the §11.6 serial-divergence
// panel and KudurruSerialDivergence alert compare it across nodes) and
// kudurru_zone_records{zone,kind} for the zones Served returns, read from
// the current snapshot on every scrape.
type Collector struct {
	cache     *zonecache.Cache
	secondary *secondary.Manager
	serial    *prometheus.Desc
	records   *prometheus.Desc
}

// NewCollector returns the collector over c; register it once.
func NewCollector(c *zonecache.Cache, copies ...*secondary.Manager) *Collector {
	collector := &Collector{
		cache:   c,
		serial:  prometheus.NewDesc("kudurru_zone_serial", "SOA serial this node serves, per authoritative zone.", []string{"zone"}, nil),
		records: prometheus.NewDesc("kudurru_zone_records", "Size of each authoritative zone served: resource records (kind=records) and owner names (kind=names).", []string{"zone", "kind"}, nil),
	}
	if len(copies) > 0 {
		collector.secondary = copies[0]
	}
	return collector
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.serial
	ch <- c.records
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	for _, z := range Served(c.cache, c.secondary) {
		ch <- prometheus.MustNewConstMetric(c.serial, prometheus.GaugeValue, float64(z.Serial()), z.Name())
		ch <- prometheus.MustNewConstMetric(c.records, prometheus.GaugeValue, float64(z.RecordCount()), z.Name(), RecordsKindRecords)
		ch <- prometheus.MustNewConstMetric(c.records, prometheus.GaugeValue, float64(z.Names()), z.Name(), RecordsKindNames)
	}
}
