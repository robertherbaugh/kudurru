package dnsdist

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
)

// fakeBlocks serves a per-address edge block list.
type fakeBlocks struct {
	mu    sync.Mutex
	lists map[string]internalapi.EdgeBlockList
	down  bool
	calls int
}

func (f *fakeBlocks) set(addr string, l internalapi.EdgeBlockList) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lists == nil {
		f.lists = map[string]internalapi.EdgeBlockList{}
	}
	f.lists[addr] = l
}

func (f *fakeBlocks) EdgeBlocks(_ context.Context, addr string) (internalapi.EdgeBlockList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.down {
		return internalapi.EdgeBlockList{}, errors.New("backend unreachable")
	}
	l, ok := f.lists[addr]
	if !ok {
		return internalapi.EdgeBlockList{}, internalapi.ErrEdgeBlocksUnsupported
	}
	return l, nil
}

func blockList(owner string, revision uint64, blocks ...internalapi.EdgeBlock) internalapi.EdgeBlockList {
	return internalapi.EdgeBlockList{Owner: owner, Revision: revision, ObservedAt: time.Now(), Blocks: blocks}
}

func edgeStatus(t *testing.T, s *Supervisor) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	var body struct {
		EdgeBlocks map[string]any `json:"edgeBlocks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.EdgeBlocks
}

// TestSupervisorEdgeBlocks drives the supervisor with the fake dnsdist:
// a pulled block is added through the console, a lagging replica cannot
// resurrect a revoked block, revoke clears and reinstalls the rest, a new
// child gets the whole set again, a supervisor restart with every backend
// down reapplies the persisted set, an unacknowledged update is resent and
// expiry removes a block from the desired set.
func TestSupervisorEdgeBlocks(t *testing.T) {
	blocks := &fakeBlocks{}
	a := internalapi.EdgeBlock{ID: "00000000000000a1", Prefix: "192.0.2.0/24", Until: time.Now().Add(time.Hour).Truncate(time.Second)}
	b := internalapi.EdgeBlock{ID: "00000000000000b2", Prefix: "2001:db8::/48", Until: time.Now().Add(30 * time.Minute).Truncate(time.Second)}
	blocks.set("10.0.0.11:8082", blockList("cluster-1", 1, a))
	mutate := func(c *Config) {
		c.Blocks, c.BlockPoll = blocks, 50*time.Millisecond
		c.Source = &chanSource{initial: []cluster.Endpoint{ep("node-a", "10.0.0.11"), ep("node-b", "10.0.0.12")}, ch: make(chan []cluster.Endpoint, 4)}
	}
	h := newHarness(t, mutate)
	h.waitFor("ready", h.ready)
	installed := func() bool { return edgeStatus(t, h.sup)["installed"] == true }
	h.waitFor("first block added", func() bool {
		return strings.Contains(h.log(), "dynblock add 192.0.2.0/24 id=00000000000000a1") && installed()
	})
	if h.count("dynblock clear") != 0 {
		t.Fatalf("a fresh child must not be cleared:\n%s", h.log())
	}
	line := ""
	for _, l := range strings.Split(h.log(), "\n") {
		if strings.HasPrefix(l, "dynblock add 192.0.2.0/24") {
			line = l
		}
	}
	if !strings.Contains(line, "seconds=3") { // about 3600 remaining seconds
		t.Fatalf("remaining lifetime not passed: %q", line)
	}
	st := edgeStatus(t, h.sup)
	if st["active"] != float64(1) || st["installed"] != true || st["source"] != "backends" || st["configured"] != true {
		t.Fatalf("status %+v", st)
	}
	if raw, _ := json.Marshal(st); strings.Contains(string(raw), "192.0.2") {
		t.Fatal("/status must not expose client prefixes")
	}

	// A second block is added without clearing the first.
	blocks.set("10.0.0.11:8082", blockList("cluster-1", 2, a, b))
	h.waitFor("second block added", func() bool {
		return strings.Contains(h.log(), "dynblock add 2001:db8::/48 id=00000000000000b2") && installed()
	})
	if h.count("dynblock clear") != 0 || h.count("dynblock add 192.0.2.0/24") != 1 {
		t.Fatalf("addition must not reinstall:\n%s", h.log())
	}

	// Two replicas: the primary revoked a (revision 3), a lagging follower
	// still has revision 2. The newest revision wins; the table is cleared
	// and b reinstalled.
	blocks.set("10.0.0.12:8082", blockList("cluster-1", 2, a, b))
	blocks.set("10.0.0.11:8082", blockList("cluster-1", 3, b))
	h.waitFor("revoke cleared and reinstalled", func() bool { return h.count("dynblock clear") == 1 && h.count("dynblock add 2001:db8::/48") == 2 })
	time.Sleep(300 * time.Millisecond)
	if h.count("dynblock add 192.0.2.0/24") != 1 || h.count("dynblock clear") != 1 {
		t.Fatalf("lagging replica resurrected a revoked block:\n%s", h.log())
	}
	persisted, err := os.ReadFile(filepath.Join(h.dir, edgeBlocksFile)) //nolint:gosec // test-owned path
	if err != nil || !strings.Contains(string(persisted), "2001:db8::/48") || strings.Contains(string(persisted), "192.0.2.0/24") {
		t.Fatalf("persisted set %s, %v", persisted, err)
	}
	if fi, _ := os.Stat(filepath.Join(h.dir, edgeBlocksFile)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("persisted mode %v", fi.Mode().Perm())
	}

	// A crashed child is replaced; the new child receives the set again.
	adds, starts := h.count("dynblock add 2001:db8::/48"), h.count("start")
	h.sup.mu.Lock()
	pid := h.sup.child.Pid()
	h.sup.mu.Unlock()
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	h.waitFor("reapplied to the new child", func() bool { return h.count("start") > starts && h.count("dynblock add 2001:db8::/48") > adds })

	// An unacknowledged update is reported and the whole set resent.
	if err := os.WriteFile(filepath.Join(h.dir, "fail-dynblock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c := internalapi.EdgeBlock{ID: "00000000000000c3", Prefix: "198.51.100.7/32", Until: time.Now().Add(time.Hour).Truncate(time.Second)}
	blocks.set("10.0.0.11:8082", blockList("cluster-1", 4, b, c))
	h.waitFor("unacknowledged update reported", func() bool { _, ok := edgeStatus(t, h.sup)["error"]; return ok })
	if ok, _ := h.sup.Ready(); !ok {
		t.Fatal("a failed edge block update must not make the edge unready")
	}
	clears := h.count("dynblock clear")
	_ = os.Remove(filepath.Join(h.dir, "fail-dynblock"))
	h.waitFor("resent with a clear", func() bool {
		_, failing := edgeStatus(t, h.sup)["error"]
		return !failing && h.count("dynblock clear") > clears
	})
	h.stop()

	// Supervisor restart with every backend unreachable: the persisted set
	// is applied to the first child.
	blocks.mu.Lock()
	blocks.down = true
	blocks.mu.Unlock()
	if err := os.Remove(filepath.Join(h.dir, "fake.log")); err != nil {
		t.Fatal(err)
	}
	h2 := newHarness(t, func(cfg *Config) {
		mutate(cfg)
		cfg.ConfigDir = h.dir
	})
	h2.waitFor("persisted blocks reapplied", func() bool {
		return strings.Contains(h2.log(), "dynblock add 2001:db8::/48") && strings.Contains(h2.log(), "dynblock add 198.51.100.7/32")
	})
	if st := edgeStatus(t, h2.sup); st["source"] != "persisted" {
		t.Fatalf("status after restart %+v", st)
	}
	// Expiry is enforced locally without any backend.
	h2.sup.mu.Lock()
	h2.sup.blocks.desired[0].Until = time.Now().Add(-time.Second)
	h2.sup.mu.Unlock()
	h2.waitFor("expired block dropped", func() bool {
		h2.sup.mu.Lock()
		defer h2.sup.mu.Unlock()
		return len(h2.sup.blocks.desired) == 1
	})
}

func TestEdgeBlockStatementsAndPlan(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	a := internalapi.EdgeBlock{ID: "00000000000000a1", Prefix: "192.0.2.0/24", Until: now.Add(90*time.Second + 500*time.Millisecond)}
	got, err := edgeBlockStatements([]internalapi.EdgeBlock{a}, true, now)
	if err != nil || len(got) != 1 || got[0] != `clearDynBlocks(); addDynamicBlock("192.0.2.0", "kudurru edge block 00000000000000a1", DNSAction.Drop, 91, 24)` {
		t.Fatalf("statements %q %v", got, err)
	}
	if _, err := edgeBlockStatements([]internalapi.EdgeBlock{{ID: "00000000000000a1", Prefix: `1"); os.exit(1) --`, Until: a.Until}}, false, now); err == nil {
		t.Fatal("unvalidated prefix rendered")
	}
	many := make([]internalapi.EdgeBlock, 0, 256)
	for i := range 256 {
		many = append(many, internalapi.EdgeBlock{ID: "00000000000000a1", Prefix: "ffff:ffff:ffff:ffff:ffff:ffff:ffff:" + strings.Repeat("f", 1+i%3) + "/128", Until: now.Add(7 * 24 * time.Hour)})
	}
	statements, err := edgeBlockStatements(many, true, now)
	if err != nil || len(statements) != 5 { // the clear takes one of the 64 slots of the first
		t.Fatalf("%d statements, %v", len(statements), err)
	}
	for _, s := range statements {
		if len(s) > maxConsoleCommand-128 {
			t.Fatalf("statement of %d bytes exceeds the console bound", len(s))
		}
	}
	installed := map[string]time.Time{a.ID: a.Until}
	if _, _, noop := planEdgeBlocks([]internalapi.EdgeBlock{a}, installed, false, false, now); !noop {
		t.Fatal("unchanged set must be a no-op")
	}
	longer := a
	longer.Until = a.Until.Add(time.Hour)
	if _, wipe, _ := planEdgeBlocks([]internalapi.EdgeBlock{longer}, installed, false, false, now); !wipe {
		t.Fatal("a changed expiry must reinstall")
	}
	if _, wipe, noop := planEdgeBlocks(nil, map[string]time.Time{a.ID: now.Add(-time.Second)}, false, false, now); wipe || !noop {
		t.Fatal("an entry dnsdist expired itself needs no clear")
	}
	if adds, wipe, _ := planEdgeBlocks([]internalapi.EdgeBlock{a}, nil, true, false, now); wipe || len(adds) != 1 {
		t.Fatal("a fresh child gets the whole set without a clear")
	}
	if adds, wipe, _ := planEdgeBlocks([]internalapi.EdgeBlock{a}, installed, false, true, now); !wipe || len(adds) != 1 {
		t.Fatal("an unknown table is cleared and reinstalled")
	}
	other := internalapi.EdgeBlock{ID: "00000000000000b2", Prefix: "192.0.2.0/24", Until: now.Add(2 * time.Hour)}
	combined := combineEdgeBlocks([]internalapi.EdgeBlockList{
		blockList("twin-a", 7, a), blockList("twin-b", 1, other), blockList("twin-a", 6),
		blockList("twin-c", 1, internalapi.EdgeBlock{ID: "00000000000000c3", Prefix: "198.51.100.0/24", Until: now.Add(-time.Second)}),
	}, now)
	if len(combined) != 1 || combined[0].ID != other.ID {
		t.Fatalf("combined %+v", combined)
	}
}
