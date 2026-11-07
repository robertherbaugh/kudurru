package main

import (
	"time"

	"github.com/robertherbaugh/kudurru/internal/health"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// Human health observes the local controller elector and served snapshot. It
// never resolves a peer address or waits for the API server. A locally observed
// remote holder is diagnostic information, not proof of its current authority.
func registerHealthDetails(checker *health.Checker, elector store.Elector, self string, snapshot func() *zonecache.Snapshot) {
	checker.SetDetails(func() health.Details {
		leader := elector.Leader()
		if elector.IsLeader() {
			leader = self
		} else if leader == self {
			// The old self identity can outlive an active election term.
			leader = ""
		}
		return health.Details{Leader: leader, SnapshotAge: snapshot().SnapshotAge(time.Now())}
	})
}
