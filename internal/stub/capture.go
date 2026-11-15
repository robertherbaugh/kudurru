package stub

import (
	"context"
	"maps"
	"slices"

	"github.com/robertherbaugh/kudurru/internal/snapshotowner"
)

// Cut retains one actual immutable Snapshot and its local installation identity.
// It needs no Release; references retain the Go-owned copy after replacement.
type Cut struct{ value snapshotowner.Cut[Snapshot] }

// Capture reads a single installation without locks, clocks or I/O.
func (m *Manager) Capture() Cut {
	if m == nil {
		return Cut{}
	}
	return Cut{value: m.current.Capture()}
}

// Snapshot returns this cut's actual snapshot, never a live recapture.
func (c Cut) Snapshot() *Snapshot { return c.value.Value() }

// Identity names a local installation, not a durable credential/data version.
func (c Cut) Identity() string { return c.value.Identity() }

// Current checks exact installation equality in memory, independent of expiry.
// Captured copies keep their original absolute deadlines and acquired provenance.
func (c Cut) Current() bool { return c.value.Current() }

// Changes broadcasts real installations. Subscribe before Capture and resubscribe
// after each close. Identical polling snapshots do not invalidate current cuts.
func (m *Manager) Changes() <-chan struct{} {
	if m == nil {
		return nil
	}
	return m.current.Changes()
}

// WithCurrent pins the exact expected installation for one full synchronous
// callback. The same gate excludes real manager publication. Root owns global
// order: publication -> providers -> host/license -> keyring -> secondary -> stub
// -> RPZ -> SQLite. Do not reenter this gate or do acquisition/network work here.
func (m *Manager) WithCurrent(ctx context.Context, expected Cut, fn func() error) error {
	if m == nil {
		return snapshotowner.ErrObsolete
	}
	return m.current.WithCurrent(ctx, expected.value, fn)
}

// Zone maps compare POINTERS only; no traversal of zone objects or lazy caches.
func sameSnapshot(a, b *Snapshot) bool {
	if !maps.Equal(a.zones, b.zones) || len(a.status) != len(b.status) {
		return false
	}
	for i := range a.status {
		x, y := a.status[i], b.status[i]
		if x.Identity != y.Identity || x.Zone != y.Zone || x.Name != y.Name || x.Serial != y.Serial ||
			x.ConfirmedAt != y.ConfirmedAt || x.ExpiresAt != y.ExpiresAt || x.NextRefresh != y.NextRefresh ||
			x.Available != y.Available || x.ConfigurationError != y.ConfigurationError || x.Error != y.Error ||
			!slices.Equal(x.Servers, y.Servers) {
			return false
		}
	}
	return true
}
