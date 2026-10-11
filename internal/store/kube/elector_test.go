package kube

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationv1client "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/utils/ptr"
)

// Short timings keep the envtest elector tests fast while preserving the
// ordering the production 15 s / 10 s / 2 s timings rely on: a leader that
// cannot renew gives up (≤ RetryPeriod + RenewDeadline = 2.25 s after its
// last renewal) well before a candidate may take an unrenewed lease
// (LeaseDuration = 4 s after it last saw it change).
const (
	testLeaseDuration = 4 * time.Second
	testRenewDeadline = 2 * time.Second
	testRetryPeriod   = 250 * time.Millisecond
)

const electorNamespace = "kudurru-system"

func TestNewLeaseElectorValidation(t *testing.T) {
	leases := coordinationv1client.New(nil)
	ok := ElectorConfig{Namespace: "ns", Name: "kudurru-controller", Identity: "pod-a",
		LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second}
	tests := []struct {
		name   string
		mutate func(*ElectorConfig)
		want   string // substring of the error; "" means valid
	}{
		{"production timings", func(*ElectorConfig) {}, ""},
		{"no namespace", func(c *ElectorConfig) { c.Namespace = "" }, "namespace"},
		{"no identity", func(c *ElectorConfig) { c.Identity = "" }, "identity"},
		{"invalid name", func(c *ElectorConfig) { c.Name = "Kudurru_Controller" }, "invalid lease name"},
		{"lease not above renew", func(c *ElectorConfig) { c.LeaseDuration = 10 * time.Second }, "must exceed renew deadline"},
		{"renew not above jittered retry", func(c *ElectorConfig) { c.RetryPeriod = 9 * time.Second }, "retry period"},
		{"zero retry", func(c *ElectorConfig) { c.RetryPeriod = 0 }, "retry period"},
		{"sub-second lease", func(c *ElectorConfig) {
			c.LeaseDuration, c.RenewDeadline, c.RetryPeriod = 900*time.Millisecond, 500*time.Millisecond, 100*time.Millisecond
		}, "one-second resolution"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ok
			tt.mutate(&cfg)
			_, err := NewLeaseElector(leases, cfg)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %v, want it to mention %q", err, tt.want)
			}
		})
	}
	if _, err := NewLeaseElector(nil, ok); err == nil {
		t.Fatal("nil Leases client must be rejected")
	}
}

// candidate is one elector under test with its own (optionally
// partitionable) connection to the apiserver.
type candidate struct {
	id      string
	e       *LeaseElector
	down    *atomic.Bool
	started atomic.Int32
	stopped atomic.Int32
	runDone chan error
	cancel  context.CancelFunc
}

// arena tracks how many terms are active at once across candidates.
type arena struct {
	active    atomic.Int32
	maxActive atomic.Int32
}

func (a *arena) enter() {
	n := a.active.Add(1)
	for {
		m := a.maxActive.Load()
		if n <= m || a.maxActive.CompareAndSwap(m, n) {
			return
		}
	}
}

func (a *arena) leave() { a.active.Add(-1) }

func newCandidate(t *testing.T, lease, id string, timings ...time.Duration) *candidate {
	t.Helper()
	cfg, down := partitionedConfig(shared.cfg)
	leases, err := coordinationv1client.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ld, rd, rp := testLeaseDuration, testRenewDeadline, testRetryPeriod
	if len(timings) == 3 {
		ld, rd, rp = timings[0], timings[1], timings[2]
	}
	e, err := NewLeaseElector(leases, ElectorConfig{
		Namespace: electorNamespace, Name: lease, Identity: id,
		LeaseDuration: ld, RenewDeadline: rd, RetryPeriod: rp,
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &candidate{id: id, e: e, down: down, runDone: make(chan error, 1)}
}

// run starts the elector; onStarted/onStopped count terms and feed the
// arena. extra, if set, runs inside onStarted.
func (c *candidate) run(t *testing.T, a *arena, extra func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	go func() {
		c.runDone <- c.e.Run(ctx, func() {
			c.started.Add(1)
			if a != nil {
				a.enter()
			}
			if extra != nil {
				extra()
			}
		}, func() {
			if a != nil {
				a.leave()
			}
			c.stopped.Add(1)
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-c.runDone:
		case <-time.After(30 * time.Second):
			t.Errorf("%s: Run did not return after cancel", c.id)
		}
	})
}

// running reports whether Run has not returned.
func (c *candidate) running() bool {
	select {
	case err := <-c.runDone:
		c.runDone <- err // keep it for the cleanup
		return false
	default:
		return true
	}
}

func leaseClient(t *testing.T) coordinationv1client.LeaseInterface {
	t.Helper()
	cs, err := coordinationv1client.NewForConfig(shared.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cs.Leases(electorNamespace)
}

func uniqueLease() string {
	return fmt.Sprintf("test-%d", time.Now().UnixNano())
}

// waitLeader waits until exactly one of cs leads and returns it.
func waitLeader(t *testing.T, timeout time.Duration, cs ...*candidate) *candidate {
	t.Helper()
	var leader *candidate
	eventually(t, timeout, "a single leader", func() bool {
		leader = nil
		n := 0
		for _, c := range cs {
			if c.e.IsLeader() {
				leader = c
				n++
			}
		}
		return n == 1
	})
	return leader
}

// TestLeaseElectorSingleLeader: two candidates, exactly one leads, both
// agree on who, and the follower never starts a term.
func TestLeaseElectorSingleLeader(t *testing.T) {
	requireEnv(t)
	lease := uniqueLease()
	var a arena
	ca, cb := newCandidate(t, lease, "pod-a"), newCandidate(t, lease, "pod-b")
	ca.run(t, &a, nil)
	cb.run(t, &a, nil)
	leader := waitLeader(t, 10*time.Second, ca, cb)
	follower := cb
	if leader == cb {
		follower = ca
	}
	eventually(t, 5*time.Second, "both to observe the leader", func() bool {
		return ca.e.Leader() == leader.id && cb.e.Leader() == leader.id
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ca.e.IsLeader() == cb.e.IsLeader() {
			t.Fatal("both or neither candidate leads")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if follower.started.Load() != 0 || a.maxActive.Load() != 1 {
		t.Fatalf("follower started %d terms; max concurrent terms %d", follower.started.Load(), a.maxActive.Load())
	}
	if err := follower.e.Run(context.Background(), nil, nil); err == nil {
		t.Fatal("a second concurrent Run must be rejected")
	}
}

// TestLeaseElectorFailoverOnExpiry is the D44 gate: the leader is cut off
// from the apiserver, its renewals fail, it stops leading after
// RenewDeadline without Run returning, and the other candidate takes the
// lease once it expires. Terms never overlap. After the partition heals the
// former leader stays a follower and keeps running.
func TestLeaseElectorFailoverOnExpiry(t *testing.T) {
	requireEnv(t)
	lease := uniqueLease()
	var a arena
	ca, cb := newCandidate(t, lease, "pod-a"), newCandidate(t, lease, "pod-b")
	ca.run(t, &a, nil)
	waitLeader(t, 10*time.Second, ca)
	cb.run(t, &a, nil)
	time.Sleep(2 * testRetryPeriod) // let pod-b observe the lease

	ca.down.Store(true)
	cut := time.Now()
	eventually(t, testRenewDeadline+3*time.Second, "pod-a's onStopped", func() bool { return ca.stopped.Load() == 1 })
	stoppedAfter := time.Since(cut)
	if ca.e.IsLeader() {
		t.Fatal("pod-a still reports leadership after onStopped")
	}
	eventually(t, testLeaseDuration+5*time.Second, "pod-b to lead", func() bool { return cb.started.Load() == 1 })
	failover := time.Since(cut)
	t.Logf("partition → pod-a onStopped: %s; partition → pod-b onStarted: %s", stoppedAfter.Round(time.Millisecond), failover.Round(time.Millisecond))
	if !ca.running() {
		t.Fatal("pod-a's Run returned on lease loss")
	}
	if got := a.maxActive.Load(); got != 1 {
		t.Fatalf("terms overlapped: %d concurrent", got)
	}

	ca.down.Store(false)
	time.Sleep(2 * testLeaseDuration)
	if !cb.e.IsLeader() || ca.e.IsLeader() || ca.started.Load() != 1 {
		t.Fatalf("after healing: pod-b leader=%v pod-a leader=%v pod-a terms=%d", cb.e.IsLeader(), ca.e.IsLeader(), ca.started.Load())
	}
	if !ca.running() || !cb.running() {
		t.Fatal("an elector's Run returned without its context being cancelled")
	}

	// Shutdown: Run returns nil, onStopped fires, the lease is released.
	cb.cancel()
	select {
	case err := <-cb.runDone:
		cb.runDone <- err
		if err != nil {
			t.Fatalf("Run returned %v on shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pod-b's Run did not return after cancel")
	}
	if cb.stopped.Load() != 1 {
		t.Fatalf("pod-b onStopped fired %d times", cb.stopped.Load())
	}
	eventually(t, testLeaseDuration, "pod-a to take the released lease", func() bool { return ca.started.Load() == 2 })
	if got := a.maxActive.Load(); got != 1 {
		t.Fatalf("terms overlapped: %d concurrent", got)
	}
}

// TestLeaseElectorLeaseDeleted: deleting the Lease while the leader is cut
// off lets the candidate create a new one at once; the old leader's term
// ends after RenewDeadline and its Run keeps going.
func TestLeaseElectorLeaseDeleted(t *testing.T) {
	requireEnv(t)
	lease := uniqueLease()
	ca, cb := newCandidate(t, lease, "pod-a"), newCandidate(t, lease, "pod-b")
	ca.run(t, nil, nil)
	waitLeader(t, 10*time.Second, ca)
	cb.run(t, nil, nil)

	ca.down.Store(true)
	if err := leaseClient(t).Delete(context.Background(), lease, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	eventually(t, testLeaseDuration, "pod-b to lead after the lease was deleted", func() bool { return cb.started.Load() == 1 })
	t.Logf("lease deleted → pod-b onStarted: %s (no expiry wait)", time.Since(start).Round(time.Millisecond))
	eventually(t, testRenewDeadline+3*time.Second, "pod-a's onStopped", func() bool { return ca.stopped.Load() == 1 })
	if !ca.running() {
		t.Fatal("pod-a's Run returned on lease loss")
	}
	ca.down.Store(false)
	time.Sleep(testLeaseDuration)
	if !cb.e.IsLeader() || ca.e.IsLeader() || !ca.running() {
		t.Fatalf("pod-b leader=%v pod-a leader=%v pod-a running=%v", cb.e.IsLeader(), ca.e.IsLeader(), ca.running())
	}
}

// TestLeaseElectorHijack: another holder writes itself into the Lease (as
// an operator or a split-brain peer might); the leader notices on its next
// renewal, ends its term and re-enters the election without Run returning.
func TestLeaseElectorHijack(t *testing.T) {
	requireEnv(t)
	lease := uniqueLease()
	ca := newCandidate(t, lease, "pod-a")
	ca.run(t, nil, nil)
	waitLeader(t, 10*time.Second, ca)

	lc := leaseClient(t)
	err := retryOnConflict(func() error {
		l, err := lc.Get(context.Background(), lease, metav1.GetOptions{})
		if err != nil {
			return err
		}
		now := metav1.NewMicroTime(time.Now())
		l.Spec.HolderIdentity = ptr.To("intruder")
		l.Spec.RenewTime = &now
		l.Spec.LeaseDurationSeconds = ptr.To[int32](int32(testLeaseDuration / time.Second))
		_, err = lc.Update(context.Background(), l, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, testRenewDeadline+3*time.Second, "pod-a's onStopped", func() bool { return ca.stopped.Load() == 1 })
	if !ca.running() {
		t.Fatal("Run returned after the lease was taken")
	}
	eventually(t, 2*time.Second, "pod-a to observe the intruder", func() bool { return ca.e.Leader() == "intruder" })
	// The intruder never renews; pod-a takes the lease back after expiry.
	eventually(t, testLeaseDuration+5*time.Second, "pod-a to lead again", func() bool { return ca.started.Load() == 2 && ca.e.IsLeader() })
}

// TestLeaseElectorPanicStepsDown: a panic in leader-only work is contained,
// the lease is released so another replica takes over, and Run continues.
func TestLeaseElectorPanicStepsDown(t *testing.T) {
	requireEnv(t)
	lease := uniqueLease()
	var panics atomic.Int32
	ca, cb := newCandidate(t, lease, "pod-a"), newCandidate(t, lease, "pod-b")
	ca.run(t, nil, func() {
		if panics.Add(1) == 1 {
			panic("leader-only work exploded")
		}
	})
	eventually(t, 10*time.Second, "pod-a's first term", func() bool { return ca.started.Load() == 1 })
	cb.run(t, nil, nil)
	eventually(t, testLeaseDuration, "pod-b to lead after pod-a stepped down", func() bool { return cb.e.IsLeader() })
	if ca.stopped.Load() != 1 || !ca.running() {
		t.Fatalf("pod-a: onStopped %d, running %v", ca.stopped.Load(), ca.running())
	}
}

// TestLeaseElectorRunWithContext: the term context ends on lease loss and
// on shutdown, and onStopped runs after onStarted returned.
func TestLeaseElectorRunWithContext(t *testing.T) {
	requireEnv(t)
	lease := uniqueLease()
	ca := newCandidate(t, lease, "pod-a")
	var mu sync.Mutex
	var order []string
	record := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ca.e.RunWithContext(ctx, func(termCtx context.Context) {
			record("started")
			<-termCtx.Done()
			record("term-ctx-done")
		}, func() { record("stopped") })
	}()
	eventually(t, 10*time.Second, "leadership", ca.e.IsLeader)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunWithContext did not return")
	}
	mu.Lock()
	got := strings.Join(order, ",")
	mu.Unlock()
	if got != "started,term-ctx-done,stopped" {
		t.Fatalf("callback order %s", got)
	}
	l, err := leaseClient(t).Get(context.Background(), lease, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if h := ptr.Deref(l.Spec.HolderIdentity, ""); h != "" {
		t.Fatalf("lease still held by %q after shutdown", h)
	}
}

// TestStoreElector: the driver's Elector maps names to Leases under the
// prefix, returns the same elector per name, and Info reports the leader.
func TestStoreElector(t *testing.T) {
	requireEnv(t)
	s := newTestStore(t, Options{Identity: "pod-z", LeaseDuration: testLeaseDuration, RenewDeadline: testRenewDeadline, RetryPeriod: testRetryPeriod})
	e := s.Elector("controller")
	if s.Elector("controller") != e {
		t.Fatal("Elector must return the same instance per name")
	}
	if err := s.Elector("Not A Name").Run(context.Background(), nil, nil); err == nil {
		t.Fatal("an invalid elector name must surface from Run")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx, nil, nil) }()
	eventually(t, 10*time.Second, "leadership", e.IsLeader)
	if got := s.Info().Leader; got != "pod-z" {
		t.Fatalf("Info().Leader = %q", got)
	}
	if _, err := leaseClient(t).Get(ctx, "kudurru-controller", metav1.GetOptions{}); err != nil {
		t.Fatalf("lease kudurru-controller: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func retryOnConflict(f func() error) error {
	var err error
	for range 5 {
		if err = f(); err == nil || !apierrors.IsConflict(err) {
			return err
		}
	}
	return err
}
