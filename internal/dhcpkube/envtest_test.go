package dhcpkube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	dhcpv1 "github.com/robertherbaugh/kudurru/api/dhcp/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

// The envtest cases run the runtime against a real apiserver: the
// DHCPScope and DHCPLease CRDs, the kube store driver and its Lease
// elector. They skip when `make envtest` has not installed the binaries.
var env struct {
	cfg    *rest.Config
	client client.WithWatch
	skip   string
}

const envNamespace = "kudurru-system"

var nsSeq atomic.Int64

func TestMain(m *testing.M) {
	ctrllog.SetLogger(logr.Discard())
	klog.SetLogger(logr.Discard())
	os.Exit(runEnv(m))
}

func runEnv(m *testing.M) int {
	assets := assetsDir()
	if assets == "" || os.Getenv("KUDURRU_SKIP_ENVTEST") != "" {
		env.skip = "envtest binaries not found: run `make envtest` or set KUBEBUILDER_ASSETS"
		return m.Run()
	}
	_, file, _, _ := goruntime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	var crds []*apiextensionsv1.CustomResourceDefinition
	for _, dir := range []string{"bases", "dhcp"} {
		paths, _ := filepath.Glob(filepath.Join(root, "config", "crd", dir, "*.yaml"))
		for _, p := range paths {
			b, err := os.ReadFile(p) //nolint:gosec // the repository's CRDs
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			crd := &apiextensionsv1.CustomResourceDefinition{}
			if err := yaml.Unmarshal(b, crd); err != nil {
				fmt.Fprintln(os.Stderr, p, err)
				return 1
			}
			crds = append(crds, crd)
		}
	}
	e := &envtest.Environment{CRDs: crds, BinaryAssetsDirectory: assets}
	args := e.ControlPlane.GetAPIServer().Configure()
	args.Set("advertise-address", "127.0.0.1")
	args.Set("endpoint-reconciler-type", "none")
	cfg, err := e.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "dhcpkube: envtest:", err)
		return 1
	}
	defer func() { _ = e.Stop() }()
	c, err := client.NewWithWatch(cfg, client.Options{Scheme: envScheme()})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := c.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: envNamespace}}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	env.cfg, env.client = cfg, c
	return m.Run()
}

func assetsDir() string {
	if d := os.Getenv("KUBEBUILDER_ASSETS"); d != "" {
		return d
	}
	_, file, _, _ := goruntime.Caller(0)
	dirs, _ := filepath.Glob(filepath.Join(filepath.Dir(file), "..", "..", "bin", "k8s", "*-"+goruntime.GOOS+"-"+goruntime.GOARCH))
	sort.Strings(dirs)
	if len(dirs) == 0 {
		return ""
	}
	return dirs[len(dirs)-1]
}

func envScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	_ = dhcpv1.AddToScheme(s)
	return s
}

func requireEnv(t *testing.T) {
	t.Helper()
	if env.skip != "" {
		t.Skip(env.skip)
	}
}

func TestClientLeasesOnAPIServer(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	leases := &ClientLeases{Client: env.client, Namespace: envNamespace}
	lease := dhcp.ExternalLease{Record: dhcp.LeaseRecord{Address: netip.MustParseAddr("192.0.2.10"),
		Identity: dhcp.LeaseIdentity{Kind: "mac", Value: "02:00:00:00:00:01"}, MAC: "02:00:00:00:00:01",
		ExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Second), State: "bound"},
		Claim: dhcp.DNSClaim{Name: "laptop.home.arpa.", Epoch: 1, Ticket: 9}}
	_, rv, err := leases.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w, err := leases.Watch(ctx, rv)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	obj := leaseObject("", "objects", lease)
	if err := leases.Create(ctx, obj); err != nil {
		t.Fatal(err)
	}
	first := obj.ResourceVersion
	// The object name makes a second owner of the address impossible.
	if err := leases.Create(ctx, leaseObject("", "objects", lease)); !errors.Is(err, ErrExists) {
		t.Fatalf("second create = %v", err)
	}
	obj.Spec.State = "declined"
	if err := leases.Update(ctx, obj); err != nil {
		t.Fatal(err)
	}
	stale := leaseObject("", "objects", lease)
	stale.ResourceVersion = first
	if err := leases.Update(ctx, stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update = %v", err)
	}
	if err := leases.Delete(ctx, obj.Name, first); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale delete = %v", err)
	}
	got, err := leases.Get(ctx, obj.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, back, err := leaseFromObject(got); err != nil || back.Claim != lease.Claim || back.Record.ExpiresAt != lease.Record.ExpiresAt {
		t.Fatalf("stored lease = %+v, %v", back, err)
	}
	if err := leases.Delete(ctx, obj.Name, obj.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := leases.Get(ctx, obj.Name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete = %v", err)
	}
	var types []watch.EventType
	timeout := time.After(10 * time.Second)
	for len(types) < 3 {
		select {
		case ev := <-w.ResultChan():
			if ev.Type != watch.Bookmark {
				types = append(types, ev.Type)
			}
		case <-timeout:
			t.Fatalf("events = %v", types)
		}
	}
	if fmt.Sprint(types) != "[ADDED MODIFIED DELETED]" {
		t.Fatalf("events = %v", types)
	}
}

func TestRuntimesFailOverOnTheKubeStore(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("DHCPKUBE_DEBUG") != "" {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	// A namespace per run: -count reruns share the control plane.
	ns := fmt.Sprintf("dhcp-%d", nsSeq.Add(1))
	if err := env.client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	pods := &ClientPods{Client: env.client, Namespace: ns}
	for _, name := range []string{"kudurru-a", "kudurru-b"} {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "kudurru", Image: "kudurru"}}}}
		if err := env.client.Create(ctx, pod); err != nil {
			t.Fatal(err)
		}
	}
	type replica struct {
		rt     *Runtime
		cancel context.CancelFunc
		done   chan struct{}
	}
	start := func(name, ip string) replica {
		st, err := kube.New(env.cfg, kube.Options{Namespace: ns, Identity: name, Logger: logger,
			LeaseDuration: 2 * time.Second, RenewDeadline: 1500 * time.Millisecond, RetryPeriod: 200 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		rt, err := New(Options{Store: st, Leases: &ClientLeases{Client: env.client, Namespace: ns}, Pods: pods,
			Elector: st.Elector(ElectionName), PodName: name, ServerIdentifier: netip.MustParseAddr("198.51.100.67"),
			DataDir: t.TempDir(), Logger: logger, Interval: 200 * time.Millisecond,
			LocalAddress: func(string) (netip.Addr, error) { return netip.MustParseAddr(ip), nil },
			Listen:       func(*dhcp.Service) error { return nil },
			Serve:        func(ctx context.Context, _ *dhcp.Service) error { <-ctx.Done(); return nil }})
		if err != nil {
			t.Fatal(err)
		}
		rctx, rcancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); _ = rt.Run(rctx) }()
		t.Cleanup(func() { rcancel(); <-done })
		return replica{rt, rcancel, done}
	}
	a := start("kudurru-a", "10.0.0.1")
	b := start("kudurru-b", "10.0.0.2")

	scope := testScope()
	scope.ResourceVersion = ""
	creator, err := kube.New(env.cfg, kube.Options{Namespace: ns, Identity: "writer", Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = creator.Close() }()
	if _, err := creator.Apply(ctx, scope, store.ApplyOptions{Manager: "gitops:argocd"}); err != nil {
		t.Fatal(err)
	}

	slow := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	var leader, follower replica
	slow("one serving replica", func() bool {
		switch {
		case a.rt.Leader() && len(a.rt.Status().Scopes) == 1:
			leader, follower = a, b
		case b.rt.Leader() && len(b.rt.Status().Scopes) == 1:
			leader, follower = b, a
		default:
			return false
		}
		return true
	})
	slow("the endpoint label", func() bool {
		active, _ := pods.ActivePods(ctx)
		return len(active) == 1 && active[0] == leader.rt.opts.PodName
	})
	lease, err := bind(t, leader.rt, "02:00:00:00:00:01", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	slow("the follower's copy", func() bool { return len(listed(follower.rt, "lan")) == 1 })
	// Short test Leases may move under load: follow whichever replica serves.
	current := func() (replica, replica, bool) {
		switch {
		case a.rt.Leader():
			return a, b, true
		case b.rt.Leader():
			return b, a, true
		}
		return replica{}, replica{}, false
	}
	slow("the scope status", func() bool {
		var s v1alpha1.DHCPScope
		if err := creator.Get(ctx, store.Key{Kind: v1alpha1.KindDHCPScope, Name: "lan"}, &s); err != nil {
			return false
		}
		serving, _, ok := current()
		return ok && s.Status.ServingPod == serving.rt.opts.PodName && s.Status.ActiveLeases == 1
	})

	// The serving replica stops; the other takes over with the lease intact.
	slow("a serving replica", func() bool {
		var ok bool
		leader, follower, ok = current()
		return ok
	})
	leader.cancel()
	<-leader.done
	slow("the takeover", func() bool { return follower.rt.Leader() })
	slow("the moved label", func() bool {
		active, _ := pods.ActivePods(ctx)
		return len(active) == 1 && active[0] == follower.rt.opts.PodName
	})
	second, err := bind(t, follower.rt, "02:00:00:00:00:02", "phone")
	if err != nil {
		t.Fatal(err)
	}
	if second.Address == lease.Address {
		t.Fatalf("the new replica reused %s", lease.Address)
	}
	var objs dhcpv1.DHCPLeaseList
	if err := env.client.List(ctx, &objs, client.InNamespace(ns), client.MatchingLabels{dhcpv1.LabelScope: "lan"}); err != nil {
		t.Fatal(err)
	}
	if len(objs.Items) != 2 {
		t.Fatalf("lease objects = %d", len(objs.Items))
	}
}
