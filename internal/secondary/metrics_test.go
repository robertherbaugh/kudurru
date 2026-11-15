package secondary

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestCollectorExpiryWithoutSchedulerTick(t *testing.T) {
	clock := newManagerClock()
	m, _, dz := managerFor(t, clock, &managerReceiver{serial: 7}, managerDB(t))
	stop := runManager(t, m)
	initial := awaitManager(t, m, dz, func(st Status) bool { return st.Available })
	stop()
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(m))
	value := func(name string) float64 {
		t.Helper()
		families, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range families {
			if f.GetName() == name {
				return f.Metric[0].GetGauge().GetValue()
			}
		}
		t.Fatalf("missing metric %s", name)
		return 0
	}
	if got := value("kudurru_secondary_available"); got != 1 {
		t.Fatalf("availability %v", got)
	}
	if got := value("kudurru_secondary_expires_timestamp_seconds"); got != float64(initial.ExpiresAt.Unix()) {
		t.Fatalf("expiry %v", got)
	}
	clock.advance(30 * time.Second)
	if got := value("kudurru_secondary_available"); got != 0 {
		t.Fatalf("expired availability %v", got)
	}
}
