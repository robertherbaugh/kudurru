package dhcpha

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
	"modernc.org/sqlite"
)

type deadlineFuture struct {
	done     <-chan struct{}
	err      error
	response any
}

func (f deadlineFuture) Error() error {
	if f.done != nil {
		<-f.done
	}
	return f.err
}
func (f deadlineFuture) Index() uint64 { return 1 }
func (f deadlineFuture) Response() any { return f.response }
func (f deadlineFuture) Open() (*raft.SnapshotMeta, io.ReadCloser, error) {
	return nil, nil, errors.New("test snapshot has no payload")
}

type deadlineRaft struct {
	*raft.Raft
	apply    func([]byte, time.Duration) raft.ApplyFuture
	verify   func() raft.Future
	snapshot func() raft.SnapshotFuture
	shutdown func()
}

func (*deadlineRaft) State() raft.RaftState { return raft.Leader }
func (r *deadlineRaft) Apply(b []byte, timeout time.Duration) raft.ApplyFuture {
	return r.apply(b, timeout)
}
func (r *deadlineRaft) VerifyLeader() raft.Future { return r.verify() }
func (r *deadlineRaft) Snapshot() raft.SnapshotFuture {
	return r.snapshot()
}
func (r *deadlineRaft) Shutdown() raft.Future {
	if r.shutdown != nil {
		r.shutdown()
	}
	return deadlineFuture{}
}

func deadlineNode(t *testing.T, r *deadlineRaft, timeout time.Duration) *Node {
	t.Helper()
	n := &Node{cfg: Config{RequestTimeout: timeout}, raft: r, fsm: &machine{}, proposals: make(chan struct{}, 1), management: make(chan struct{}, 2), done: make(chan struct{})}
	n.fsm.initialized.Store(true)
	t.Cleanup(func() {
		if err := n.Close(); err != nil {
			t.Error(err)
		}
	})
	return n
}

func deadlineResult(t *testing.T, results <-chan error) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-time.After(time.Second):
		t.Fatal("operation exceeded its request budget")
		return nil
	}
}

func TestDeadlineProposalUsesRemainingEnqueueBudget(t *testing.T) {
	entered := make(chan time.Duration, 1)
	r := &deadlineRaft{apply: func(_ []byte, timeout time.Duration) raft.ApplyFuture {
		entered <- timeout
		<-time.After(timeout)
		return deadlineFuture{err: raft.ErrEnqueueTimeout}
	}}
	n := deadlineNode(t, r, time.Second)
	n.proposals <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := n.propose(ctx, command{}, false); result <- err }()
	// Spend part of the same caller budget waiting for the prior proposal.
	<-time.After(100 * time.Millisecond)
	<-n.proposals
	select {
	case timeout := <-entered:
		deadline, _ := ctx.Deadline()
		if timeout <= 0 || timeout > 250*time.Millisecond || timeout > time.Until(deadline)+20*time.Millisecond {
			t.Fatalf("enqueue received a fresh timeout: %v", timeout)
		}
	case <-time.After(time.Second):
		t.Fatal("proposal never reached enqueue")
	}
	if err := deadlineResult(t, result); !errors.Is(err, ErrNoQuorum) {
		t.Fatal("stalled enqueue succeeded", err)
	}
}

func TestDeadlineCancellationRetainsOneWorkerThroughEnqueueAndFuture(t *testing.T) {
	enqueue, future := make(chan struct{}), make(chan struct{})
	releaseEnqueue := sync.OnceFunc(func() { close(enqueue) })
	releaseFuture := sync.OnceFunc(func() { close(future) })
	entered, waiting := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	r := &deadlineRaft{apply: func(_ []byte, _ time.Duration) raft.ApplyFuture {
		calls.Add(1)
		close(entered)
		<-enqueue
		close(waiting)
		return deadlineFuture{done: future, response: applyResult{}}
	}}
	n := deadlineNode(t, r, time.Second)
	t.Cleanup(releaseFuture)
	t.Cleanup(releaseEnqueue)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := n.propose(ctx, command{}, false); result <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("proposal never reached enqueue")
	}
	cancel()
	if err := deadlineResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatal("enqueue prevented caller cancellation", err)
	}
	// Repeated callers must wait on the occupied slot, rather than creating
	// another goroutine/future for every timeout while Raft remains stalled.
	const callers = 20
	results := make(chan error, callers)
	for range callers {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			_, err := n.propose(ctx, command{}, false)
			results <- err
		}()
	}
	for range callers {
		if err := deadlineResult(t, results); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("blocked caller did not exhaust its own budget", err)
		}
	}
	if calls.Load() != 1 || len(n.proposals) != 1 {
		t.Fatal("timed-out enqueue released its guard", calls.Load(), len(n.proposals))
	}
	releaseEnqueue()
	select {
	case <-waiting:
	case <-time.After(time.Second):
		t.Fatal("enqueue did not return a future")
	}
	if len(n.proposals) != 1 {
		t.Fatal("guard was released before the future resolved")
	}
	releaseFuture()
	eventually(t, time.Second, func() bool { return len(n.proposals) == 0 }, "worker cleanup after late future completion")
	if n.lastSuccess.Load() != 0 {
		t.Fatal("late completion marked an expired request ready")
	}
}

func TestDeadlineCapsFutureVerificationAndSnapshotWaits(t *testing.T) {
	for _, operation := range []string{"apply", "verify", "snapshot"} {
		t.Run(operation, func(t *testing.T) {
			blocked := make(chan struct{})
			release := sync.OnceFunc(func() { close(blocked) })
			var calls atomic.Int32
			r := &deadlineRaft{
				apply: func(_ []byte, _ time.Duration) raft.ApplyFuture {
					calls.Add(1)
					return deadlineFuture{done: blocked, response: applyResult{}}
				},
				verify: func() raft.Future { calls.Add(1); return deadlineFuture{done: blocked} },
				snapshot: func() raft.SnapshotFuture {
					calls.Add(1)
					return deadlineFuture{done: blocked}
				},
				shutdown: release,
			}
			n := deadlineNode(t, r, 40*time.Millisecond)
			t.Cleanup(release)
			call := func() error {
				switch operation {
				case "verify":
					return n.Ready(context.Background())
				case "snapshot":
					return n.Snapshot(context.Background())
				default:
					_, err := n.propose(context.Background(), command{}, false)
					return err
				}
			}
			result := make(chan error, 1)
			go func() { result <- call() }()
			if err := deadlineResult(t, result); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("unbounded request did not use configured timeout", err)
			}
			if err := call(); !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
				t.Fatal("blocked future spawned another worker", err, calls.Load())
			}
			if err := n.Close(); err != nil || len(n.proposals) != 0 {
				t.Fatal("shutdown failed to join outstanding worker", err)
			}
		})
	}
}

func TestDeadlineCancellationBeforeAndAfterProposalSlot(t *testing.T) {
	n := deadlineNode(t, &deadlineRaft{}, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 50 {
		if _, err := n.propose(ctx, command{}, false); !errors.Is(err, context.Canceled) {
			t.Fatal("canceled proposal accepted", err)
		}
		if err := n.Ready(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal("canceled verification accepted", err)
		}
	}
	if err := n.acquireProposal(context.Background()); err != nil {
		t.Fatal(err)
	}
	called := false
	r := n.awaitProposal(ctx, func() applyResult { called = true; return applyResult{} })
	if called || !errors.Is(r.Err, context.Canceled) || len(n.proposals) != 0 {
		t.Fatal("cancellation after slot acquisition started work", called, r.Err)
	}
}

var deadlineFunctionID atomic.Uint64

// stalledReplica pauses an actual SQL transaction while it owns the lease
// mutex. Reading ReplicaInfo here would hang until release, past a request's
// deadline, while the atomic index must keep reporting the prior commit.
func stalledReplica(t *testing.T) (*dhcp.LeaseStore, func()) {
	t.Helper()
	entered, blocked := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(blocked) })
	function := fmt.Sprintf("ha_deadline_hold_%d", deadlineFunctionID.Add(1))
	if err := sqlite.RegisterScalarFunction(function, 0, func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
		close(entered)
		<-blocked
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	c := clusterFixture(t)
	s, err := dhcp.OpenReplicaLeases(c.paths[0], c.leaseConfigs[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	at := time.Now().UTC().Truncate(time.Second)
	if _, err := s.ApplyReplica(context.Background(), 1, dhcp.ReplicaCommand{Operation: "noop", At: at}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", c.paths[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("CREATE TRIGGER deadline_hold AFTER UPDATE ON lease_replica BEGIN SELECT " + function + "(); END"); err != nil { // #nosec G202 -- name is a fixed prefix followed by an atomic test counter.
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	t.Cleanup(func() {
		release()
		if err := deadlineResult(t, finished); err != nil {
			t.Error(err)
		}
	})
	go func() {
		_, err := s.ApplyReplica(context.Background(), 2, dhcp.ReplicaCommand{Operation: "noop", At: at})
		finished <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("replica did not reach blocked transaction")
	}
	return s, release
}

func TestDeadlineFollowerBarrierDoesNotWaitOnSQLite(t *testing.T) {
	s, release := stalledReplica(t)
	n := deadlineNode(t, &deadlineRaft{}, time.Second)
	n.fsm.stores = map[string]*dhcp.LeaseStore{"default": s}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- n.waitApplied(ctx, "default", 2) }()
	if err := deadlineResult(t, result); !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrUnavailable) {
		t.Fatal("follower barrier ignored deadline during SQL apply", err)
	}
	if s.ReplicaAppliedIndex() != 1 {
		t.Fatal("barrier observed an uncommitted index")
	}
	release()
	ctx, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := n.waitApplied(ctx, "default", 2); err != nil {
		t.Fatal("follower barrier failed after durable commit", err)
	}
	n.fsm.failed.Store(&failure{err: errors.New("local storage failed")})
	if err := n.waitApplied(ctx, "default", 2); !errors.Is(err, ErrUnavailable) {
		t.Fatal("failed replica bypassed the barrier", err)
	}
}

func TestDeadlineManagementResponseDoesNotWaitOnSQLite(t *testing.T) {
	s, _ := stalledReplica(t)
	r := &deadlineRaft{apply: func(_ []byte, _ time.Duration) raft.ApplyFuture {
		return deadlineFuture{response: applyResult{}}
	}}
	n := deadlineNode(t, r, 50*time.Millisecond)
	n.cfg.Peers = []Peer{{ID: "node1"}}
	n.fsm.stores = map[string]*dhcp.LeaseStore{"default": s}
	server, caller := net.Pipe()
	defer func() { _ = caller.Close() }()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer func() { _ = server.Close() }()
		n.serveManagement(server, "node2")
	}()
	if err := caller.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	request := managementRequest{Command: command{Scope: "default", Lease: dhcp.ReplicaCommand{Operation: "options"}}}
	if err := writeFrame(caller, request); err != nil {
		t.Fatal(err)
	}
	var response managementResponse
	if err := readFrame(caller, &response, maxCommandBytes); err != nil || response.Index != 1 || response.Error != "" {
		t.Fatal("management response waited behind another SQL transaction", response, err)
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("management response goroutine leaked")
	}
}
