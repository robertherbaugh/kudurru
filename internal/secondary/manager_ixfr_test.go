package secondary

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

type managerIncrementalReceiver struct {
	managerReceiver
	incremental int
	deltaError  error
	baseSerial  uint32
}

func (r *managerIncrementalReceiver) IXFR(_ context.Context, _ *v1alpha1.DNSZone, _ string, base []dns.RR) ([]dns.RR, error) {
	r.incremental++
	r.baseSerial = base[0].(*dns.SOA).Serial
	if r.deltaError != nil {
		return nil, r.deltaError
	}
	return managerRecords(r.serial), nil
}
func TestManagerIncrementalUsesOnlyCurrentCredentialAndLiveBase(t *testing.T) {
	for _, tc := range []struct {
		name         string
		age          time.Duration
		rotate, fail bool
		wantIXFR     int
	}{
		{name: "live incremental", age: time.Second, wantIXFR: 1},
		{name: "expired bootstraps AXFR", age: 30 * time.Second},
		{name: "rotated key bootstraps AXFR", age: time.Second, rotate: true},
		{name: "corrupt delta does not fall back", age: time.Second, fail: true, wantIXFR: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newManagerClock()
			recv := &managerIncrementalReceiver{managerReceiver: managerReceiver{serial: 9}}
			m, _, dz := managerFor(t, clock, recv, managerDB(t))
			e := &entry{dz: dz, id: zone.SecondaryIdentity(dz), credential: "unsigned"}
			initial := m.refresh(t.Context(), e)
			if initial.err != nil {
				t.Fatal(initial.err)
			}
			if recv.count() != 1 || recv.incremental != 0 {
				t.Fatal("initial copy was not AXFR")
			}
			e.z, e.data = initial.z, initial.data
			clock.advance(tc.age)
			recv.set(10, nil)
			if tc.rotate {
				e.credential = "previous-key"
			}
			if tc.fail {
				recv.deltaError = errors.New("bad delta")
			}
			next := m.refresh(t.Context(), e)
			if (next.err != nil) != tc.fail {
				t.Fatalf("refresh: %v", next.err)
			}
			if recv.incremental != tc.wantIXFR {
				t.Fatalf("IXFR calls %d want %d", recv.incremental, tc.wantIXFR)
			}
			if tc.wantIXFR == 1 && (recv.baseSerial != 9 || recv.count() != 1) {
				t.Fatal("IXFR used wrong base or retried an invalid delta")
			}
			if tc.wantIXFR == 0 && recv.count() != 2 {
				t.Fatal("obsolete/rotated base was reused")
			}
			if !tc.fail && next.z.Serial() != 10 {
				t.Fatal("complete replacement was not built")
			}
			if e.z.Serial() != 9 {
				t.Fatal("working delta mutated published base")
			}
		})
	}
}
