package main

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/zoneview"
)

// capturedAuthoritativeZones is only for internal/metrics callbacks. Public
// requests use Diagnostics with their original HTTP lease instead of recapture.
func capturedAuthoritativeZones(ctx context.Context, source *servingbuild.Diagnostics) []servingbuild.ZoneDiagnostic {
	cut, err := source.Capture(ctx)
	if err != nil {
		return nil
	}
	defer cut.Release()
	zones, err := cut.Zones()
	if err != nil {
		return nil
	}
	var out []servingbuild.ZoneDiagnostic
	for _, z := range zones {
		if z.Available && (z.Type == public.ZoneTypePrimary || z.Type == public.ZoneTypeSecondary) {
			out = append(out, z)
		}
	}
	return out
}

type servingZoneCollector struct {
	source  *servingbuild.Diagnostics
	serial  *prometheus.Desc
	records *prometheus.Desc
}

func newServingZoneCollector(source *servingbuild.Diagnostics) *servingZoneCollector {
	return &servingZoneCollector{
		source:  source,
		serial:  prometheus.NewDesc("kudurru_zone_serial", "SOA serial this node serves, per authoritative zone.", []string{"zone"}, nil),
		records: prometheus.NewDesc("kudurru_zone_records", "Size of each authoritative zone served: resource records (kind=records) and owner names (kind=names).", []string{"zone", "kind"}, nil),
	}
}

func (c *servingZoneCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.serial
	ch <- c.records
}

func (c *servingZoneCollector) Collect(ch chan<- prometheus.Metric) {
	for _, z := range capturedAuthoritativeZones(context.Background(), c.source) {
		ch <- prometheus.MustNewConstMetric(c.serial, prometheus.GaugeValue, float64(z.Serial), z.Apex)
		ch <- prometheus.MustNewConstMetric(c.records, prometheus.GaugeValue, float64(z.RecordCount), z.Apex, zoneview.RecordsKindRecords)
		ch <- prometheus.MustNewConstMetric(c.records, prometheus.GaugeValue, float64(z.Names), z.Apex, zoneview.RecordsKindNames)
	}
}
