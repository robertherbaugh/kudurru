package driver

import (
	"fmt"
	"sync"
	"testing"
	"time"

	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
)

// A new child zone's lifecycle steps run back to back (D404): each step
// that commits a lifecycle change and needs another (creation, the parent
// fence, the Preparing-to-Fencing CAS) ends the pass, and the next pass
// resumes at the same zone. Before, every one of those steps waited behind a
// step of every other zone, and each such step reconciles that zone and
// waits for its publication. On a mounted node under load a new child took
// up to 12 s to answer without the race detector, longer with more zones.
//
// The test counts the publication waits of unrelated active zones (two per
// step: the republish and the acknowledgment wait) from the moment the
// child's lifecycle exists until the child is active. With the focus the
// child's remaining steps (the parent fence, the Preparing-to-Fencing CAS,
// then activation) run back to back and no other zone is stepped between
// them; and the first pass on a roster with the new zone steps it first,
// so only the pass already running when it was created can come first.
// Without this each of those steps waited behind a full pass: 72 waits of
// 8 zones after the child's lifecycle existed (focus04-before.log).
func TestNewChildStepsAreNotQueuedBehindEveryZone(t *testing.T) {
	const others = 8
	f := setup(t)
	f.zone("family-parent", "family.example.")
	for i := range others {
		f.zone(fmt.Sprintf("other-%d", i), fmt.Sprintf("other%d.example.", i))
	}
	_, stop := f.start()
	defer stop()
	awaitConverged(t, func() bool {
		if !f.active("family.example.", 1) {
			return false
		}
		for i := range others {
			if !f.active(fmt.Sprintf("other%d.example.", i), 1) {
				return false
			}
		}
		return true
	})

	var (
		mu        sync.Mutex
		counting  bool
		lifecycle bool
		waits     int
		total     int
	)
	f.pub.setObserve(func(m pr.Member) {
		mu.Lock()
		defer mu.Unlock()
		if !counting || m.Zone.Apex == "family.example." || m.Zone.Apex == "child.family.example." {
			return
		}
		total++
		if !lifecycle {
			lifecycle = f.ledger("family-child") != nil
		}
		if lifecycle {
			waits++
		}
	})
	mu.Lock()
	counting = true
	mu.Unlock()
	started := time.Now()
	f.zone("family-child", "child.family.example.")
	awaitConverged(t, func() bool { return f.active("child.family.example.", 1) })
	mu.Lock()
	counting = false
	got, all := waits, total
	mu.Unlock()
	t.Logf("child active %s after its creation; %d publication waits of the %d other zones after its DNSZone was created, %d after its lifecycle existed", time.Since(started).Round(time.Millisecond), all, others, got)
	// From the DNSZone's creation: at most the rest of the pass that was
	// already running on the roster read before it (at most three waits
	// per zone: an activation's publication, the republish and the
	// acknowledgment wait); the next pass steps the new zone first.
	if all > 3*others {
		t.Fatalf("the new child waited behind %d publication waits of %d other zones; want at most %d", all, others, 3*others)
	}
	// At most the one step that was already waiting when the lifecycle was
	// committed.
	if got > 2 {
		t.Fatalf("the child's steps waited behind %d publication waits of %d other zones; want at most 2", got, others)
	}
}
