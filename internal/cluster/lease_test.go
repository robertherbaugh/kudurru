package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationv1client "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

// envCfg is the shared control plane; nil when envtest is unavailable.
var (
	envCfg  *rest.Config
	envSkip string
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" || os.Getenv("KUDURRU_SKIP_ENVTEST") != "" {
		envSkip = "KUBEBUILDER_ASSETS is not set (or KUDURRU_SKIP_ENVTEST is); skipping the Lease handoff test"
		return m.Run()
	}
	for _, b := range []string{"kube-apiserver", "etcd"} {
		if _, err := os.Stat(filepath.Join(assets, b)); err != nil { //nolint:gosec // assets is KUBEBUILDER_ASSETS, set by the developer
			envSkip = fmt.Sprintf("KUBEBUILDER_ASSETS=%s has no %s", assets, b)
			return m.Run()
		}
	}
	if os.Getenv("KUDURRU_TEST_LOGS") == "" {
		ctrllog.SetLogger(logr.Discard())
		klog.SetLogger(logr.Discard())
	}
	env := &envtest.Environment{BinaryAssetsDirectory: assets}
	// See internal/store/kube: loopback advertise address for hosts
	// without a default route.
	args := env.ControlPlane.GetAPIServer().Configure()
	args.Set("advertise-address", "127.0.0.1")
	args.Set("endpoint-reconciler-type", "none")
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cluster: envtest failed to start: %v\n", err)
		return 1
	}
	defer func() { _ = env.Stop() }()
	envCfg = cfg
	return m.Run()
}

const (
	leaseNS       = "default"
	testLease     = 2 * time.Second
	testRenew     = 1500 * time.Millisecond
	testRetry     = 200 * time.Millisecond
	leaseScenario = 30 * time.Second
)

func leases(t *testing.T) coordinationv1client.LeasesGetter {
	t.Helper()
	c, err := coordinationv1client.NewForConfig(envCfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func leaseElector(t *testing.T, lease, id string) *kube.LeaseElector {
	t.Helper()
	e, err := kube.NewLeaseElector(leases(t), kube.ElectorConfig{
		Namespace: leaseNS, Name: lease, Identity: id,
		LeaseDuration: testLease, RenewDeadline: testRenew, RetryPeriod: testRetry,
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// node is one Runner with an always-on and a leader-only runnable that
// count their lifecycles; overlap tracks concurrent leader terms across
// nodes.
type node struct {
	h            *harness
	leaderStarts atomic.Int32
	leaderStops  atomic.Int32
}

func startNode(t *testing.T, lease, id string, overlap, maxOverlap *atomic.Int32) *node {
	t.Helper()
	n := &node{}
	n.h = startHarness(t, Options{Elector: leaseElector(t, lease, id)}, func(h *harness) {
		mustAdd(t, h.r, "dns", false, blocker(h.rec, "dns"))
		mustAdd(t, h.r, "controller", true, func(ctx context.Context) error {
			n.leaderStarts.Add(1)
			c := overlap.Add(1)
			for {
				m := maxOverlap.Load()
				if c <= m || maxOverlap.CompareAndSwap(m, c) {
					break
				}
			}
			<-ctx.Done()
			overlap.Add(-1)
			n.leaderStops.Add(1)
			return nil
		})
	})
	return n
}

// TestLeaseHandoff runs two Runners on one Lease in a real apiserver:
// exactly one runs the leader group; a stolen lease stops only the leader
// group (the Runner and its always-on runnable keep running) and the group
// restarts once the lease can be re-acquired; a graceful shutdown hands
// the lease to the other node without overlapping terms.
func TestLeaseHandoff(t *testing.T) {
	if envCfg == nil {
		t.Skip(envSkip)
	}
	lease := fmt.Sprintf("handoff-%d", time.Now().UnixNano())
	var overlap, maxOverlap atomic.Int32
	a := startNode(t, lease, "pod-a", &overlap, &maxOverlap)
	eventually2(t, "pod-a leads", func() bool { return a.h.r.IsLeader() && a.leaderStarts.Load() == 1 })
	b := startNode(t, lease, "pod-b", &overlap, &maxOverlap)
	eventually2(t, "pod-b dns", func() bool { return b.h.rec.count("dns:start") == 1 })
	time.Sleep(3 * testRetry)
	if b.h.r.IsLeader() || b.leaderStarts.Load() != 0 {
		t.Fatal("follower started the leader group")
	}

	// Steal the lease: an intruder holds it for a long time. pod-a's
	// renewals fail; after RenewDeadline its term ends.
	lc := leases(t).Leases(leaseNS)
	steal := func(holder string, seconds int32) {
		t.Helper()
		for range 10 {
			l, err := lc.Get(context.Background(), lease, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			now := metav1.NewMicroTime(time.Now())
			l.Spec = coordinationv1.LeaseSpec{HolderIdentity: ptr.To(holder), LeaseDurationSeconds: ptr.To(seconds), AcquireTime: &now, RenewTime: &now}
			if _, err = lc.Update(context.Background(), l, metav1.UpdateOptions{}); err == nil {
				return
			}
		}
		t.Fatal("could not update the lease")
	}
	steal("intruder", 3600)
	eventually2(t, "pod-a loses the term", func() bool { return !a.h.r.IsLeader() && a.leaderStops.Load() == 1 })
	select {
	case err := <-a.h.done:
		t.Fatalf("pod-a Runner returned on lease loss: %v", err)
	default:
	}
	if a.h.rec.count("dns:stop") != 0 {
		t.Fatal("pod-a always-on runnable stopped on lease loss")
	}
	if b.h.r.IsLeader() {
		t.Fatal("pod-b leads while the intruder holds the lease")
	}

	// Free the lease: one of the nodes re-acquires and restarts its group.
	steal("", 1)
	eventually2(t, "a new leader term", func() bool { return a.h.r.IsLeader() || b.h.r.IsLeader() })
	leader, follower := a, b
	if b.h.r.IsLeader() {
		leader, follower = b, a
	}
	eventually2(t, "leader group restarted", func() bool { return overlap.Load() == 1 })

	// Graceful shutdown of the leader releases the lease; the follower
	// takes over quickly (well under LeaseDuration after release).
	t0 := time.Now()
	if err := leader.h.stop(t); err != nil {
		t.Fatal(err)
	}
	eventually2(t, "follower takes over", func() bool { return follower.h.r.IsLeader() && overlap.Load() == 1 })
	t.Logf("graceful handoff: leader stop → successor leader group running in %s", time.Since(t0).Round(time.Millisecond))
	if maxOverlap.Load() != 1 {
		t.Fatalf("leader groups overlapped: max %d", maxOverlap.Load())
	}
}

func eventually2(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(leaseScenario)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
