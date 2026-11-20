package dnsdist

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
)

const (
	restoreTestPoll = 5 * time.Second
	restoreTestHold = 5 * time.Minute
)

var restoreTestEpoch = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

type restoreTestPeer struct {
	list  atomic.Pointer[internalapi.EdgeBlockList]
	hits  atomic.Int64
	delay atomic.Int64
}

func (p *restoreTestPeer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet || req.URL.Path != internalapi.PathEdgeBlocks {
		http.NotFound(w, req)
		return
	}
	time.Sleep(time.Duration(p.delay.Load()))
	list := p.list.Load()
	p.hits.Add(1)
	if list == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	_ = json.NewEncoder(w).Encode(list)
}

type restoreTestHarness struct {
	*harness
	ticks chan time.Time
	now   atomic.Pointer[time.Time]
}

type restoreTestBackends struct {
	peers  []*restoreTestPeer
	source *chanSource
	client *internalapi.Client
}

func newRestoreTestBackends(t *testing.T, count int) *restoreTestBackends {
	t.Helper()
	backends := &restoreTestBackends{source: &chanSource{}}
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	for i := range count {
		peer := new(restoreTestPeer)
		server := httptest.NewTLSServer(peer)
		t.Cleanup(server.Close)
		if i == 0 {
			backends.client = internalapi.NewClient(creds, internalapi.ClientOptions{Transport: server.Client().Transport})
		}
		endpoint := ep([]string{"first", "second"}[i], []string{"10.0.0.11", "10.0.0.12"}[i])
		endpoint.InternalAddress = strings.TrimPrefix(server.URL, "https://")
		backends.source.initial = append(backends.source.initial, endpoint)
		backends.peers = append(backends.peers, peer)
	}
	return backends
}

func (b *restoreTestBackends) start(t *testing.T, at time.Duration, dir string) *restoreTestHarness {
	t.Helper()
	h := &restoreTestHarness{ticks: make(chan time.Time)}
	now := restoreTestEpoch.Add(at)
	h.now.Store(&now)
	h.harness = newHarness(t, func(cfg *Config) {
		cfg.Blocks, cfg.Source = b.client, b.source
		cfg.blockNow = func() time.Time { return *h.now.Load() }
		cfg.blockPollTicks = h.ticks
		if dir != "" {
			cfg.ConfigDir = dir
		}
	})
	if h.sup.cfg.BlockRestoreHold != restoreTestHold || h.sup.cfg.BlockPoll != restoreTestPoll {
		t.Fatal("unexpected production poll or hold default")
	}
	h.waitFor("ready", h.ready)
	return h
}

func (h *restoreTestHarness) pull(at time.Duration, backends *restoreTestBackends) {
	h.t.Helper()
	now := restoreTestEpoch.Add(at)
	h.now.Store(&now)
	previous := make([]int64, len(backends.peers))
	var success bool
	for i, peer := range backends.peers {
		previous[i] = peer.hits.Load()
		success = success || peer.list.Load() != nil
	}
	select {
	case h.ticks <- now:
	case <-time.After(10 * time.Second):
		h.t.Fatal("supervisor did not receive the poll tick")
	}
	h.waitFor("edge block pull", func() bool {
		if success {
			h.sup.mu.Lock()
			defer h.sup.mu.Unlock()
			return h.sup.blocks.pulled.Equal(now)
		}
		for i, peer := range backends.peers {
			if peer.hits.Load() <= previous[i] {
				return false
			}
		}
		return true
	})
}

func (h *restoreTestHarness) expect(owner string, revision uint64, blocks ...internalapi.EdgeBlock) {
	h.t.Helper()
	h.sup.mu.Lock()
	defer h.sup.mu.Unlock()
	state := h.sup.blocks
	if got, ok := state.owners[owner]; !ok || got.Revision != revision || !slices.Equal(state.desired, blocks) {
		h.t.Fatalf("owner %s revision = %d, desired = %v; want revision %d and %v", owner, got.Revision, state.desired, revision, blocks)
	}
}

func (h *restoreTestHarness) waitInstalled(blocks ...internalapi.EdgeBlock) {
	h.t.Helper()
	h.waitFor("installed blocks", func() bool {
		h.sup.mu.Lock()
		defer h.sup.mu.Unlock()
		state := h.sup.blocks
		if state.installedChild == nil || state.unknown || len(state.installed) != len(blocks) {
			return false
		}
		for _, block := range blocks {
			if !state.installed[block.ID].Equal(block.Until) {
				return false
			}
		}
		return true
	})
}

func (h *restoreTestHarness) waitSaved(at time.Duration) {
	h.t.Helper()
	root, err := os.OpenRoot(h.dir)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	h.waitFor("pull checkpoint saved", func() bool {
		data, err := root.ReadFile(edgeBlocksFile)
		var doc edgeBlocksDocument
		return err == nil && json.Unmarshal(data, &doc) == nil && doc.SavedAt.Equal(restoreTestEpoch.Add(at))
	})
}

func restoreTestList(owner string, revision uint64, blocks ...internalapi.EdgeBlock) *internalapi.EdgeBlockList {
	return &internalapi.EdgeBlockList{Owner: owner, Revision: revision, ObservedAt: restoreTestEpoch, Blocks: blocks}
}

func TestSupervisorEdgeBlockRestoreWithSlowPeer(t *testing.T) {
	backends := newRestoreTestBackends(t, 2)
	owner, slowPeer := backends.peers[0], backends.peers[1]
	owner.list.Store(restoreTestList("owner", 10))
	slowPeer.delay.Store(int64(120 * time.Millisecond))
	h := newHarness(t, func(cfg *Config) {
		cfg.Blocks, cfg.Source = backends.client, backends.source
		cfg.BlockPoll, cfg.BlockRestoreHold = 25*time.Millisecond, 350*time.Millisecond
	})
	hasRevision := func(revision uint64) func() bool {
		return func() bool {
			h.sup.mu.Lock()
			defer h.sup.mu.Unlock()
			return h.sup.blocks.owners["owner"].Revision == revision
		}
	}
	h.waitFor("initial block revision", hasRevision(10))
	owner.list.Store(restoreTestList("owner", 2))
	h.waitFor("restored revision despite the slow peer", hasRevision(2))
}

func TestSupervisorEdgeBlockRestoreContinuity(t *testing.T) {
	current := internalapi.EdgeBlock{ID: "00000000000000a1", Prefix: "192.0.2.0/24", Until: restoreTestEpoch.Add(time.Hour)}
	stale := internalapi.EdgeBlock{ID: "00000000000000b2", Prefix: "2001:db8::/48", Until: current.Until}
	for _, interruption := range []string{"sparse pulls", "failed pull", "failed pull then restart", "backward clock", "long restart", "v2 upgrade"} {
		t.Run(interruption, func(t *testing.T) {
			backends := newRestoreTestBackends(t, 1)
			peer := backends.peers[0]
			peer.list.Store(restoreTestList("owner", 10, current))
			h := backends.start(t, 0, "")
			h.pull(0, backends)
			h.waitInstalled(current)
			lower := restoreTestList("owner", 2, stale)
			peer.list.Store(lower)
			firstLower := restoreTestPoll
			h.pull(firstLower, backends)
			h.expect("owner", 10, current)
			end := firstLower + restoreTestHold
			switch interruption {
			case "failed pull", "failed pull then restart", "backward clock":
				for at := firstLower + restoreTestPoll; at < end-restoreTestPoll; at += restoreTestPoll {
					h.pull(at, backends)
				}
				if interruption == "backward clock" {
					h.pull(end-3*restoreTestPoll, backends)
				} else {
					peer.list.Store(nil)
					h.pull(end-restoreTestPoll, backends)
					if interruption == "failed pull then restart" {
						h.waitSaved(end - restoreTestPoll)
						h.stop()
						h = backends.start(t, end, h.dir)
						h.waitInstalled(current)
					}
					peer.list.Store(lower)
				}
			case "long restart", "v2 upgrade":
				h.stop()
				dir := h.dir
				if interruption == "v2 upgrade" {
					const oldCache = `{"version":2,"savedAt":"2026-11-20T12:04:55Z","blocks":[{"id":"00000000000000a1","prefix":"192.0.2.0/24","until":"2026-11-20T13:00:00Z"}],"owners":{"owner":{"revision":"10","acceptedAt":"2026-11-20T12:00:00Z","lowerSince":"2026-11-20T12:00:05Z","blocks":[{"id":"00000000000000a1","prefix":"192.0.2.0/24","until":"2026-11-20T13:00:00Z"}]}}}`
					if err := os.WriteFile(filepath.Join(dir, edgeBlocksFile), []byte(oldCache), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				h = backends.start(t, end, dir)
				h.waitInstalled(current)
			}
			h.pull(end, backends)
			h.expect("owner", 10, current)
		})
	}
}

func TestSupervisorEdgeBlockMissingOwner(t *testing.T) {
	backends := newRestoreTestBackends(t, 2)
	first := internalapi.EdgeBlock{ID: "00000000000000a1", Prefix: "192.0.2.0/24", Until: restoreTestEpoch.Add(time.Hour)}
	second := internalapi.EdgeBlock{ID: "00000000000000b2", Prefix: "2001:db8::/48", Until: first.Until}
	firstList := restoreTestList("first", 1, first)
	backends.peers[0].list.Store(firstList)
	secondList := restoreTestList("second", 2, second)
	backends.peers[1].list.Store(secondList)
	h := backends.start(t, 0, "")
	h.pull(0, backends)
	h.waitInstalled(first, second)

	backends.peers[1].list.Store(nil)
	h.pull(restoreTestPoll, backends)
	h.sup.mu.Lock()
	afterMiss := slices.Clone(h.sup.blocks.desired)
	h.sup.mu.Unlock()
	h.waitInstalled(afterMiss...)
	backends.peers[1].list.Store(secondList)
	h.pull(2*restoreTestPoll, backends)
	h.waitInstalled(first, second)
	if h.count("dynblock clear") != 0 || h.count("dynblock add 2001:db8::/48") != 1 {
		t.Fatalf("transient miss cleared and reinstalled blocks:\n%s", h.log())
	}
	if !slices.Equal(afterMiss, []internalapi.EdgeBlock{first, second}) {
		t.Fatalf("transient miss changed desired blocks: %v", afterMiss)
	}

	backends.peers[1].list.Store(nil)
	miss := 3 * restoreTestPoll
	h.pull(miss, backends)
	h.expect("second", 2, first, second)
	sparse := miss + restoreTestHold
	h.pull(sparse, backends)
	h.expect("second", 2, first, second)

	failed := sparse + restoreTestPoll
	backends.peers[0].list.Store(nil)
	h.pull(failed, backends)
	h.waitSaved(failed)
	h.stop()
	begin := failed + restoreTestPoll
	h = backends.start(t, begin, h.dir)
	h.waitInstalled(first, second)
	backends.peers[0].list.Store(firstList)
	for at := begin; at < begin+restoreTestHold; at += restoreTestPoll {
		h.pull(at, backends)
		h.expect("second", 2, first, second)
	}
	if h.count("dynblock clear") != 0 {
		t.Fatalf("absence cleared blocks before the hold:\n%s", h.log())
	}
	h.pull(begin+restoreTestHold, backends)
	h.expect("second", 2, first)
	h.waitInstalled(first)
	if h.count("dynblock clear") != 1 {
		t.Fatalf("confirmed absence did not clear the departed owner's blocks:\n%s", h.log())
	}
	backends.peers[1].list.Store(restoreTestList("second", 1, second))
	h.pull(begin+restoreTestHold+restoreTestPoll, backends)
	h.expect("second", 2, first)
}

func TestSupervisorEdgeBlockRestoredOwnerNewBlock(t *testing.T) {
	backends := newRestoreTestBackends(t, 1)
	peer := backends.peers[0]
	restored := internalapi.EdgeBlock{ID: "00000000000000a1", Prefix: "192.0.2.0/24", Until: restoreTestEpoch.Add(time.Hour)}
	newBlock := internalapi.EdgeBlock{ID: "00000000000000b2", Prefix: "2001:db8::/48", Until: restored.Until}
	peer.list.Store(restoreTestList("owner", 10))
	h := backends.start(t, 0, "")
	h.pull(0, backends)
	firstLower := restoreTestPoll
	peer.list.Store(restoreTestList("owner", 2, restored))
	h.pull(firstLower, backends)
	h.expect("owner", 10)
	h.stop()

	peer.list.Store(restoreTestList("owner", 3, restored, newBlock))
	h = backends.start(t, 2*restoreTestPoll, h.dir)
	end := firstLower + restoreTestHold
	for at := 2 * restoreTestPoll; at < end; at += restoreTestPoll {
		h.pull(at, backends)
		h.expect("owner", 10)
	}
	if h.count("dynblock add") != 0 {
		t.Fatalf("lower set was installed before the hold:\n%s", h.log())
	}
	h.pull(end, backends)
	h.expect("owner", 3, restored, newBlock)
	h.waitInstalled(restored, newBlock)

	postRestore := internalapi.EdgeBlock{ID: "00000000000000c3", Prefix: "198.51.100.7/32", Until: restored.Until}
	peer.list.Store(restoreTestList("owner", 4, restored, newBlock, postRestore))
	h.pull(end+restoreTestPoll, backends)
	h.expect("owner", 4, restored, newBlock, postRestore)
	h.waitInstalled(restored, newBlock, postRestore)
}
