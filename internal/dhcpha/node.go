package dhcpha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
	"go.etcd.io/bbolt"
)

// raftNode is the subset used by the deadline-bounded runtime. Keeping enqueue
// and completion separate also permits testing stalls at either boundary.
type raftNode interface {
	State() raft.RaftState
	LeaderWithID() (raft.ServerAddress, raft.ServerID)
	CommitIndex() uint64
	LastContact() time.Time
	Apply([]byte, time.Duration) raft.ApplyFuture
	VerifyLeader() raft.Future
	Snapshot() raft.SnapshotFuture
	Shutdown() raft.Future
}

// Node owns Raft resources, but does not own the caller's SQLite lease stores.
type Node struct {
	cfg           Config
	raft          raftNode
	stream        *peerStream
	transport     *raft.NetworkTransport
	disk          *raftboltdb.BoltStore
	fsm           *machine
	proposals     chan struct{}
	management    chan struct{}
	done          chan struct{}
	closeOnce     sync.Once
	closeErr      error
	wg            sync.WaitGroup
	proposalMu    sync.Mutex // prevents worker registration after Close starts waiting
	lastSuccess   atomic.Int64
	closed        atomic.Bool
	stopRequested chan struct{}
}

// Status reports local replication health. Ready is a recent observation;
// successful mutations always require a fresh committed log entry themselves.
type Status struct {
	Enabled       bool          `json:"enabled"`
	NodeID        string        `json:"nodeID"`
	ClusterID     string        `json:"clusterID"`
	Role          string        `json:"role"`
	LeaderID      string        `json:"leaderID,omitempty"`
	LeaderAddress string        `json:"leaderAddress,omitempty"`
	Ready         bool          `json:"ready"`
	Initialized   bool          `json:"initialized"`
	AppliedIndex  uint64        `json:"appliedIndex"`
	CommitIndex   uint64        `json:"commitIndex"`
	LastContact   time.Duration `json:"lastContact"`
	Error         string        `json:"error,omitempty"`
}

type manifest struct {
	Version                        int
	NodeID, ClusterID, Fingerprint string
	Peers                          []Peer
	Bootstrap                      bool
	Started                        bool
}

// Open starts one static voter. Only an explicit bootstrap node may seed
// existing standalone ownership; joining nodes must have empty local stores.
func Open(cfg Config, stores map[string]*dhcp.LeaseStore) (*Node, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if len(stores) == 0 || len(stores) > 256 {
		return nil, errors.New("dhcp HA: between 1 and 256 scope stores are required")
	}
	names := make([]string, 0, len(stores))
	for name, s := range stores {
		if s == nil {
			return nil, errors.New("dhcp HA: nil scope store")
		}
		scope := s.Configuration().Config
		if len(scope.FailoverServers) != 3 {
			return nil, fmt.Errorf("dhcp HA: scope %s requires all three failover server addresses", name)
		}
		mapped := map[string]bool{}
		for _, server := range scope.FailoverServers {
			mapped[server.NodeID] = true
			if server.NodeID == cfg.NodeID && server.Address != scope.ServerIP {
				return nil, fmt.Errorf("dhcp HA: scope %s server IP differs from this node's mapping", name)
			}
		}
		for _, peer := range cfg.Peers {
			if !mapped[peer.ID] {
				return nil, fmt.Errorf("dhcp HA: scope %s lacks failover server %s", name, peer.ID)
			}
		}
		names = append(names, name)
	}
	sort.Strings(names)
	fp, err := fingerprint(cfg, stores)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.DataDir, 0o700); err != nil { // #nosec G302 -- this is the private Raft directory, not a regular file.
		return nil, err
	}
	manifestPath := filepath.Join(cfg.DataDir, "identity.json")
	want := manifest{Version: 1, NodeID: cfg.NodeID, ClusterID: cfg.ClusterID, Fingerprint: fp, Peers: cfg.Peers, Bootstrap: cfg.Bootstrap}
	var previous manifest
	contents, err := os.ReadFile(manifestPath) // #nosec G304 -- operator-configured private Raft directory.
	existingManifest := err == nil
	if err == nil {
		if err = json.Unmarshal(contents, &previous); err != nil {
			return nil, fmt.Errorf("dhcp HA: corrupt identity: %w", err)
		}
		if previous.Version != 1 || previous.NodeID != want.NodeID || previous.ClusterID != want.ClusterID || previous.Fingerprint != want.Fingerprint {
			return nil, errors.New("dhcp HA: persisted cluster identity or scope topology differs; restore the original configuration")
		}
		// Bootstrap is an initial operator authorization, not a restart switch.
		want.Bootstrap = previous.Bootstrap
		want.Started = previous.Started
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	disk, err := raftboltdb.New(raftboltdb.Options{Path: filepath.Join(cfg.DataDir, "raft.db"), BoltOptions: &bbolt.Options{Timeout: time.Second}})
	if err != nil {
		return nil, fmt.Errorf("dhcp HA: open durable Raft store: %w", err)
	}
	n := &Node{cfg: cfg, disk: disk, proposals: make(chan struct{}, 1), management: make(chan struct{}, 2), done: make(chan struct{}), stopRequested: make(chan struct{}, 1)}
	fail := func(err error) (*Node, error) { _ = n.Close(); return nil, err }
	logger := hclog.New(&hclog.LoggerOptions{Name: "dhcp-raft", Output: io.Discard, Level: hclog.Warn})
	snaps, err := raft.NewFileSnapshotStore(cfg.DataDir, 3, io.Discard)
	if err != nil {
		return fail(err)
	}
	exists, err := raft.HasExistingState(disk, disk, snaps)
	if err != nil {
		return fail(err)
	}
	if exists && !existingManifest {
		return fail(errors.New("dhcp HA: Raft state has no cluster identity; refusing adoption"))
	}
	if !exists && want.Started {
		return fail(errors.New("dhcp HA: Raft state disappeared after initialization; restore a backup instead of bootstrapping again"))
	}
	if !exists && !want.Bootstrap {
		for _, name := range names {
			index, hasRows := stores[name].ReplicaInfo()
			if index != 0 || hasRows {
				return fail(fmt.Errorf("dhcp HA: joining scope %s contains local ownership; only the explicit bootstrap node may adopt leases", name))
			}
		}
	}
	if err := saveManifest(manifestPath, want); err != nil {
		return fail(err)
	}
	n.fsm = &machine{stores: stores, names: names, fingerprint: fp}
	n.fsm.markStarted = func() error { started := want; started.Started = true; return saveManifest(manifestPath, started) }
	n.stream, err = newPeerStream(cfg, fp)
	if err != nil {
		return fail(err)
	}
	n.stream.onManagement = n.serveManagement
	n.transport = raft.NewNetworkTransportWithConfig(&raft.NetworkTransportConfig{Stream: n.stream, MaxPool: 3, Timeout: time.Second, Logger: logger, MaxRPCsInFlight: 1})
	rcfg := raft.DefaultConfig()
	rcfg.LocalID = raft.ServerID(cfg.NodeID)
	rcfg.HeartbeatTimeout = time.Second
	rcfg.ElectionTimeout = time.Second
	rcfg.LeaderLeaseTimeout = 500 * time.Millisecond
	rcfg.CommitTimeout = 25 * time.Millisecond
	rcfg.SnapshotThreshold = 256
	rcfg.TrailingLogs = 64
	rcfg.SnapshotInterval = 30 * time.Second
	rcfg.Logger = logger
	n.fsm.stop = func(err error) {
		cfg.Logger.Error("DHCP replica stopped after storage failure; DNS remains available", "node", cfg.NodeID, "error", err)
		select {
		case n.stopRequested <- struct{}{}:
		default:
		}
	}
	if !exists && want.Bootstrap {
		want.Started = true
		if err := saveManifest(manifestPath, want); err != nil {
			return fail(err)
		}
		if err := raft.BootstrapCluster(rcfg, disk, disk, snaps, n.transport, n.configuration()); err != nil {
			return fail(err)
		}
	}
	r, err := raft.NewRaft(rcfg, n.fsm, disk, disk, snaps, n.transport)
	if err != nil {
		return fail(err)
	}
	n.raft = r
	n.stream.start()
	n.wg.Add(1)
	go n.monitor()
	if want.Bootstrap {
		n.wg.Add(1)
		go n.seedLoop()
	}
	return n, nil
}

func (n *Node) configuration() raft.Configuration {
	var c raft.Configuration
	for _, p := range n.cfg.Peers {
		c.Servers = append(c.Servers, raft.Server{Suffrage: raft.Voter, ID: raft.ServerID(p.ID), Address: raft.ServerAddress(p.Address)})
	}
	return c
}

func saveManifest(path string, m manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".identity-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

func (n *Node) seedLoop() {
	defer n.wg.Done()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-n.done:
			return
		case <-tick.C:
		}
		if n.fsm.initialized.Load() || n.fsm.failed.Load() != nil {
			return
		}
		leader, _ := n.raft.LeaderWithID()
		if leader == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), n.cfg.RequestTimeout)
		seed := map[string][]byte{}
		var err error
		for _, name := range n.fsm.names {
			seed[name], err = n.fsm.stores[name].CaptureReplica(ctx)
			if err != nil {
				break
			}
		}
		if err == nil {
			cmd := command{Seed: seed, Fingerprint: n.fsm.fingerprint, Lease: dhcp.ReplicaCommand{At: time.Now().UTC().Truncate(time.Second)}}
			if n.raft.State() == raft.Leader {
				_, err = n.propose(ctx, cmd, true)
			} else {
				_, err = n.remote(ctx, string(leader), cmd, true)
			}
		}
		cancel()
		if err != nil && n.fsm.failed.Load() == nil {
			n.cfg.Logger.Debug("DHCP initial quorum seed pending", "error", err)
		}
	}
}

func (n *Node) monitor() {
	defer n.wg.Done()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	var previous string
	for {
		select {
		case <-n.done:
			return
		case <-n.stopRequested:
			_ = n.raft.Shutdown().Error()
			return
		case <-tick.C:
			if n.raft.State() == raft.Leader && n.fsm.initialized.Load() && n.fsm.failed.Load() == nil {
				ctx, cancel := context.WithTimeout(context.Background(), n.cfg.RequestTimeout)
				_ = n.Ready(ctx)
				cancel()
			}
			s := n.Status()
			key := fmt.Sprintf("%s/%s/%t/%s", s.Role, s.LeaderID, s.Ready, s.Error)
			if key != previous {
				n.cfg.Logger.Info("DHCP quorum status", "role", s.Role, "leader", s.LeaderID, "ready", s.Ready, "error", s.Error)
				previous = key
			}
		}
	}
}

// Status returns the current local role, leader, durable progress, and health.
func (n *Node) Status() Status {
	s := Status{Enabled: true, NodeID: n.cfg.NodeID, ClusterID: n.cfg.ClusterID, Role: "stopped"}
	if n.raft == nil {
		return s
	}
	leader, id := n.raft.LeaderWithID()
	s.LeaderID = string(id)
	s.LeaderAddress = string(leader)
	s.Role = n.raft.State().String()
	s.Initialized = n.fsm.initialized.Load()
	s.AppliedIndex = n.fsm.appliedIndex.Load()
	s.CommitIndex = n.raft.CommitIndex()
	if last := n.raft.LastContact(); !last.IsZero() {
		s.LastContact = time.Since(last)
	}
	s.Ready = !n.closed.Load() && s.Initialized && n.raft.State() == raft.Leader && time.Since(time.Unix(0, n.lastSuccess.Load())) < n.cfg.RequestTimeout
	if f := n.fsm.failed.Load(); f != nil {
		s.Error = f.err.Error()
		s.Ready = false
	}
	return s
}

// Ready verifies current quorum authority. INFORM and other nonmutating DHCP
// responses use this fence; allocations already acquire one through Apply.
func (n *Node) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, n.cfg.RequestTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrNoQuorum, err)
	}
	if err := n.check(true); err != nil {
		return err
	}
	if err := n.acquireProposal(ctx); err != nil {
		return err
	}
	r := n.awaitProposal(ctx, func() applyResult {
		if err := n.check(true); err != nil {
			return applyResult{Err: err}
		}
		if err := ctx.Err(); err != nil {
			return applyResult{Err: errors.Join(ErrNoQuorum, err)}
		}
		if err := n.raft.VerifyLeader().Error(); err != nil {
			return applyResult{Err: errors.Join(ErrNoQuorum, err)}
		}
		return applyResult{}
	})
	if r.Err == nil {
		n.lastSuccess.Store(time.Now().UnixNano())
	} else {
		n.lastSuccess.Store(0)
	}
	return r.Err
}

func (n *Node) check(requireInit bool) error {
	if n.closed.Load() || n.fsm.failed.Load() != nil {
		return ErrUnavailable
	}
	if n.raft.State() != raft.Leader {
		return ErrNotLeader
	}
	if requireInit && !n.fsm.initialized.Load() {
		return ErrUnavailable
	}
	return nil
}

func (n *Node) propose(ctx context.Context, cmd command, seed bool) (dhcp.ReplicaResult, error) {
	ctx, cancel := context.WithTimeout(ctx, n.cfg.RequestTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return dhcp.ReplicaResult{}, errors.Join(ErrNoQuorum, err)
	}
	if err := n.check(!seed); err != nil {
		return dhcp.ReplicaResult{}, err
	}
	if err := n.acquireProposal(ctx); err != nil {
		return dhcp.ReplicaResult{}, err
	}
	// Serialize timestamp assignment with proposals, and never move the FSM
	// clock backward when a newly elected leader has a slower wall clock.
	now := time.Now().UTC().Truncate(time.Second)
	if last := n.fsm.lastTime.Load(); now.Unix() < last {
		now = time.Unix(last, 0).UTC()
	}
	cmd.Lease.At = now
	cmd.Fingerprint = n.fsm.fingerprint
	b, err := json.Marshal(cmd)
	limit := maxCommandBytes
	if seed {
		limit = maxSnapshotBytes
	}
	if err != nil || len(b) > limit {
		<-n.proposals
		return dhcp.ReplicaResult{}, errors.New("dhcp HA: command exceeds size limit")
	}
	r := n.awaitProposal(ctx, func() applyResult {
		if err := n.check(!seed); err != nil {
			return applyResult{Err: err}
		}
		if err := ctx.Err(); err != nil {
			return applyResult{Err: errors.Join(ErrNoQuorum, err)}
		}
		deadline, _ := ctx.Deadline()
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return applyResult{Err: errors.Join(ErrNoQuorum, context.DeadlineExceeded)}
		}
		// Apply's timeout limits enqueueing only. Both that blocking call and
		// future completion stay in this one worker, retaining the guard even
		// when the caller's budget expires before the outcome becomes known.
		future := n.raft.Apply(b, remaining)
		if err := future.Error(); err != nil {
			return applyResult{Err: errors.Join(ErrNoQuorum, err)}
		}
		result, ok := future.Response().(applyResult)
		if !ok {
			return applyResult{Err: ErrUnavailable}
		}
		if n.fsm.failed.Load() != nil {
			result.Err = ErrUnavailable
		}
		return result
	})
	if r.Err == nil {
		n.lastSuccess.Store(time.Now().UnixNano())
	} else if errors.Is(r.Err, ErrNoQuorum) || errors.Is(r.Err, ErrUnavailable) {
		n.lastSuccess.Store(0)
	}
	return r.Result, r.Err
}

// acquireProposal shares the caller's budget with enqueue and completion. The
// second context check prevents a canceled caller from winning an available
// slot and starting work anyway.
func (n *Node) acquireProposal(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrNoQuorum, err)
	}
	select {
	case n.proposals <- struct{}{}:
	case <-ctx.Done():
		return errors.Join(ErrNoQuorum, ctx.Err())
	case <-n.done:
		return ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		<-n.proposals
		return errors.Join(ErrNoQuorum, err)
	}
	if n.closed.Load() {
		<-n.proposals
		return ErrUnavailable
	}
	return nil
}

// awaitProposal owns an acquired slot until work really finishes. A timed-out
// operation may still commit, so releasing early would allow an unbounded
// backlog of unresolved futures. Close shuts Raft down and joins this worker.
func (n *Node) awaitProposal(ctx context.Context, work func() applyResult) applyResult {
	n.proposalMu.Lock()
	if err := ctx.Err(); err != nil {
		n.proposalMu.Unlock()
		<-n.proposals
		return applyResult{Err: errors.Join(ErrNoQuorum, err)}
	}
	if n.closed.Load() {
		n.proposalMu.Unlock()
		<-n.proposals
		return applyResult{Err: ErrUnavailable}
	}
	ch := make(chan applyResult, 1)
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		defer func() { <-n.proposals }()
		if err := ctx.Err(); err != nil {
			ch <- applyResult{Err: errors.Join(ErrNoQuorum, err)}
			// nosemgrep: missing-unlock-before-return -- this goroutine never holds proposalMu
			return
		}
		ch <- work()
	}()
	n.proposalMu.Unlock()
	select {
	case r := <-ch:
		if err := ctx.Err(); err != nil {
			return applyResult{Err: errors.Join(ErrNoQuorum, err)}
		}
		if n.closed.Load() {
			return applyResult{Err: ErrUnavailable}
		}
		return r
	case <-ctx.Done():
		return applyResult{Err: errors.Join(ErrNoQuorum, ctx.Err())}
	case <-n.done:
		return applyResult{Err: ErrUnavailable}
	}
}

// Backend adapts one scope to quorum-backed protocol and management operations.
func (n *Node) Backend(scope string) *Backend { return &Backend{node: n, scope: scope} }

// Snapshot durably captures the FSM and permits compaction of older Raft logs.
func (n *Node) Snapshot(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, n.cfg.RequestTimeout)
	defer cancel()
	if n.closed.Load() {
		return ErrUnavailable
	}
	if err := n.acquireProposal(ctx); err != nil {
		return err
	}
	return n.awaitProposal(ctx, func() applyResult { return applyResult{Err: n.raft.Snapshot().Error()} }).Err
}

// Close stops this voter and releases transport and Raft storage handles.
func (n *Node) Close() error {
	n.closeOnce.Do(func() {
		n.proposalMu.Lock()
		n.closed.Store(true)
		close(n.done)
		n.proposalMu.Unlock()
		if n.stream != nil {
			n.closeErr = errors.Join(n.closeErr, n.stream.Close())
		}
		if n.raft != nil {
			n.closeErr = errors.Join(n.closeErr, n.raft.Shutdown().Error())
		}
		if n.transport != nil {
			n.closeErr = errors.Join(n.closeErr, n.transport.Close())
		}
		n.wg.Wait()
		if n.disk != nil {
			n.closeErr = errors.Join(n.closeErr, n.disk.Close())
		}
	})
	return n.closeErr
}
