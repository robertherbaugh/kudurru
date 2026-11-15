package stub

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestCollectorExpiresWithoutSchedulerAndHasOnlyZoneLabels(t *testing.T) {
	c := clock()
	m, _, dz := setupManager(t, c, &fakeDiscovery{clock: c}, db(t, filepath.Join(t.TempDir(), "local.db")))
	stop := run(t, m)
	initial := await(t, m, dz, func(s Status) bool { return s.Available })
	stop()
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(m))
	value := func(name string) float64 {
		t.Helper()
		families, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, family := range families {
			for _, metric := range family.Metric {
				if len(metric.Label) != 1 || metric.Label[0].GetName() != "zone" || metric.Label[0].GetValue() != dz.Name {
					t.Fatal("endpoint label/cardinality leak")
				}
			}
			if family.GetName() == name {
				return family.Metric[0].GetGauge().GetValue()
			}
		}
		t.Fatalf("missing metric %s", name)
		return 0
	}
	for name, want := range map[string]float64{"kudurru_stub_available": 1, "kudurru_stub_expires_timestamp_seconds": float64(initial.ExpiresAt.Unix()), "kudurru_stub_confirmed_timestamp_seconds": float64(initial.ConfirmedAt.Unix()), "kudurru_stub_next_attempt_timestamp_seconds": float64(initial.NextRefresh.Unix()), "kudurru_stub_discovered_servers": 1} {
		if got := value(name); got != want {
			t.Fatalf("%s=%v want %v", name, got, want)
		}
	}
	c.advance(20 * time.Second)
	if got := value("kudurru_stub_available"); got != 0 {
		t.Fatal("expiry missing at scrape time")
	}
}
