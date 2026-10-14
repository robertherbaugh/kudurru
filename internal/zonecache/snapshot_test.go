package zonecache

import (
	"testing"
	"time"
)

func TestSnapshotAge(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		snap *Snapshot
		want string
	}{
		{name: "no snapshot"},
		{name: "unsynced", snap: &Snapshot{State: Unsynced, SnapshotWrittenAt: now.Add(-time.Minute)}},
		{name: "live", snap: &Snapshot{State: Live, SnapshotWrittenAt: now.Add(-time.Minute)}},
		{name: "unknown write time", snap: &Snapshot{State: FromSnapshot}},
		{name: "round down", snap: &Snapshot{State: FromSnapshot, SnapshotWrittenAt: now.Add(-time.Minute - 400*time.Millisecond)}, want: "1m0s"},
		{name: "round up", snap: &Snapshot{State: FromSnapshot, SnapshotWrittenAt: now.Add(-time.Minute - 600*time.Millisecond)}, want: "1m1s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.snap.SnapshotAge(now); got != tc.want {
				t.Fatalf("SnapshotAge = %q, want %q", got, tc.want)
			}
		})
	}
	snap := &Snapshot{State: FromSnapshot, SnapshotWrittenAt: now.Add(-time.Minute)}
	if got := snap.SnapshotAge(now.Add(time.Hour)); got != "1h1m0s" {
		t.Fatalf("SnapshotAge must use the caller's clock: %q", got)
	}
}
