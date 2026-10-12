package zonecache

import (
	"slices"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// State is where the served objects come from.
type State int32

// States, in the order a healthy start passes through them.
const (
	// Unsynced: the store has not synced and no local.db snapshot is being
	// served. The object set is empty; the authoritative stage answers
	// SERVFAIL for primary zones.
	Unsynced State = iota
	// FromSnapshot: the store did not sync within StartupTimeout and the
	// objects come from local.db (Degraded{ServingFromSnapshot}).
	FromSnapshot
	// Live: every watched kind delivered its initial list and Bookmark;
	// the objects follow the store.
	Live
)

// String returns "Unsynced", "FromSnapshot" or "Live".
func (s State) String() string {
	switch s {
	case Unsynced:
		return "Unsynced"
	case FromSnapshot:
		return "FromSnapshot"
	case Live:
		return "Live"
	}
	return "State(?)"
}

// Degraded condition reasons.
const (
	// ReasonServingFromSnapshot: objects come from local.db (§4.4).
	ReasonServingFromSnapshot = v1alpha1.ReasonFromSnapshot
	// ReasonSnapshotStale: the served local.db snapshot is older than
	// MaxAge; primary zones answer SERVFAIL.
	ReasonSnapshotStale = "SnapshotStale"
	// ReasonBuildFailed: a builder failed; its previous view is served.
	ReasonBuildFailed = "BuildFailed"
	// ReasonSnapshotWriteFailed: the last local.db write failed; it is
	// retried on the next persist tick.
	ReasonSnapshotWriteFailed = "SnapshotWriteFailed"
)

// Condition is one active Degraded condition (type v1alpha1.ConditionDegraded).
type Condition struct {
	// Reason is one of the Reason* constants.
	Reason string
	// Builder names the failed builder for ReasonBuildFailed.
	Builder string
	// Message is a human-readable explanation.
	Message string
	// Since is when the condition was first raised.
	Since time.Time
}

// String renders the condition for logs and readiness messages.
func (c Condition) String() string {
	if c.Message == "" {
		return c.Reason
	}
	return c.Reason + ": " + c.Message
}

// Snapshot is one immutable published state. Readers load it once per
// query with Cache.Snapshot and never lock.
type Snapshot struct {
	// Generation increases by one with every published snapshot; the
	// initial empty snapshot is generation 0.
	Generation uint64
	// State is where Objects come from.
	State State
	// Objects is the object set the views were built from.
	Objects ObjectSet
	// BuiltAt is when the snapshot was published.
	BuiltAt time.Time
	// SnapshotWrittenAt is when the served local.db snapshot was written;
	// zero unless State is FromSnapshot.
	SnapshotWrittenAt time.Time
	// Stale is true when State is FromSnapshot and the snapshot was older
	// than MaxAge at BuiltAt. The Cache republishes when the snapshot
	// crosses MaxAge, so Stale flips without a store change.
	Stale bool

	views    map[string]any
	degraded []Condition
}

// View returns the value builder name produced for this snapshot (or the
// last value it produced successfully), and whether there is one.
func (s *Snapshot) View(name string) (any, bool) {
	v, ok := s.views[name]
	return v, ok
}

// View returns the view of builder name as a T. ok is false when the
// builder has never built successfully or its value is not a T.
func View[T any](s *Snapshot, name string) (T, bool) {
	v, ok := s.views[name]
	if !ok {
		var zero T
		return zero, false
	}
	t, ok := v.(T)
	return t, ok
}

// Authoritative reports whether primary zones may be answered from this
// snapshot. It is false while Unsynced and while serving a local.db
// snapshot older than MaxAge; the authoritative stage then answers
// SERVFAIL (never NXDOMAIN) for names in primary zones while forwarding
// continues (§4.4).
func (s *Snapshot) Authoritative() bool {
	switch s.State {
	case Live:
		return true
	case FromSnapshot:
		return !s.Stale
	}
	return false
}

// Degraded returns the Degraded conditions active when the snapshot was
// published.
func (s *Snapshot) Degraded() []Condition { return slices.Clone(s.degraded) }
