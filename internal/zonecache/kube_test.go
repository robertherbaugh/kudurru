package zonecache

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// startEnvtest starts kube-apiserver and etcd with the embedded kudurru
// CRDs, or skips when KUBEBUILDER_ASSETS is unset (or KUDURRU_SKIP_ENVTEST
// is set).
func startEnvtest(t *testing.T) (*rest.Config, client.Client) {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	switch {
	case os.Getenv("KUDURRU_SKIP_ENVTEST") != "":
		t.Skip("KUDURRU_SKIP_ENVTEST is set")
	case assets == "":
		t.Skip("KUBEBUILDER_ASSETS is unset; run `make envtest` and export it to run the kube cold-start test")
	}
	for _, b := range []string{"kube-apiserver", "etcd"} {
		if _, err := os.Stat(filepath.Join(assets, b)); err != nil { //nolint:gosec // KUBEBUILDER_ASSETS is the test operator's own setting
			t.Skipf("KUBEBUILDER_ASSETS=%s has no %s", assets, b)
		}
	}
	if os.Getenv("KUDURRU_TEST_LOGS") == "" {
		ctrllog.SetLogger(logr.Discard())
		klog.SetLogger(logr.Discard())
	}
	files, err := validator.EmbeddedCRDs()
	if err != nil {
		t.Fatal(err)
	}
	var crds []*apiextensionsv1.CustomResourceDefinition
	for name, b := range files {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.UnmarshalStrict(b, crd); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		crds = append(crds, crd)
	}
	env := &envtest.Environment{CRDs: crds, BinaryAssetsDirectory: assets}
	// Loopback advertise address and no endpoint reconciler: works on hosts
	// without a default route (as in the kube driver tests).
	args := env.ControlPlane.GetAPIServer().Configure()
	args.Set("advertise-address", "127.0.0.1")
	args.Set("endpoint-reconciler-type", "none")
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"apps", "dns", "kudurru-system"} {
		if err := admin.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	return cfg, admin
}

// partitioned returns a copy of cfg whose requests fail while the flag is
// set: a replica cut off from the apiserver.
func partitioned(cfg *rest.Config) (*rest.Config, *atomic.Bool) {
	down := &atomic.Bool{}
	c := rest.CopyConfig(cfg)
	c.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if down.Load() {
				return nil, errors.New("test partition: apiserver unreachable")
			}
			return rt.RoundTrip(r)
		})
	})
	return c, down
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newKubeStore(t *testing.T, cfg *rest.Config, identity, expected string) *kube.Store {
	t.Helper()
	st, err := kube.New(cfg, kube.Options{Namespace: "kudurru-system", Identity: identity, ExpectedClusterID: expected, Logger: quietLogger(), StartupTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestKubeColdStart is the §4.4 proof on a real apiserver:
//
//  1. a live Cache over the kube driver writes local.db after its sync and
//     after a change;
//  2. a restarted replica that cannot reach the apiserver falls back to
//     local.db after StartupTimeout, builds its views from it, reports
//     ready and Degraded{ServingFromSnapshot} with the snapshot's age;
//  3. a change is made while it is cut off; when the partition heals the
//     informers sync and the snapshot is swapped for the live set.
func TestKubeColdStart(t *testing.T) {
	cfg, admin := startEnvtest(t)
	ctx := context.Background()
	var ks corev1.Namespace
	if err := admin.Get(ctx, client.ObjectKey{Name: "kube-system"}, &ks); err != nil {
		t.Fatal(err)
	}
	db, _ := openDB(t)

	st := newKubeStore(t, cfg, "zonecache-live", string(ks.UID))
	a := newCache(t, st, Options{LocalDB: db, PersistInterval: 100 * time.Millisecond, Debounce: 10 * time.Millisecond})
	if err := a.Register(zonesBuilder()); err != nil {
		t.Fatal(err)
	}
	stopA := startCache(t, a)
	eventually(t, 30*time.Second, "the live cache over the kube driver", func() bool { return a.Snapshot().State == Live })
	objs := []store.Object{
		zone("home", "home.example."),
		rrset("apps", "nas", "home", map[string]string{"team": "infra"}),
		rrset("dns", "ns1", "home", nil),
		policy("apps", "kids", "ads"),
		clientgroup("apps", "kids"),
		blocklist("apps", "ads"),
	}
	for _, o := range objs {
		if _, err := st.Apply(ctx, o, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	want := zoneSummary{"home": {"apps/nas", "dns/ns1"}}
	eventually(t, 30*time.Second, "the live view", func() bool {
		zs, _ := View[zoneSummary](a.Snapshot(), "zones")
		return reflect.DeepEqual(zs, want)
	})
	eventually(t, 30*time.Second, "local.db with every object and the cluster ID", func() bool {
		snap, err := db.Load(ctx)
		return err == nil && len(snap.Objects) == len(objs) && snap.ClusterID == string(ks.UID)
	})
	persisted := load(t, db)
	for _, k := range DefaultKinds() {
		if persisted.ResourceVersions[k] == "" {
			t.Errorf("no resourceVersion for %s in local.db", k)
		}
	}
	stopA()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// The restarted replica: the apiserver is unreachable.
	pcfg, down := partitioned(cfg)
	down.Store(true)
	cold := newKubeStore(t, pcfg, "zonecache-cold", string(ks.UID))
	reg := prometheus.NewRegistry()
	b := newCache(t, cold, Options{LocalDB: db, ExpectedClusterID: string(ks.UID), Registerer: reg, StartupTimeout: time.Second, Debounce: 10 * time.Millisecond, PersistInterval: 100 * time.Millisecond})
	if err := b.Register(zonesBuilder()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	startCache(t, b)
	eventually(t, 15*time.Second, "serving from local.db", func() bool { return b.Snapshot().State == FromSnapshot })
	fellBack := time.Since(start)
	snap := b.Snapshot()
	if zs, ok := View[zoneSummary](snap, "zones"); !ok || !reflect.DeepEqual(zs, want) {
		t.Fatalf("view from local.db %v, want %v", zs, want)
	}
	if snap.Objects.Len() != len(objs) || len(snap.Objects.Policies()) != 1 || len(snap.Objects.ClientGroups()) != 1 {
		t.Fatalf("snapshot objects: %d", snap.Objects.Len())
	}
	if ok, msgs := b.Ready(); !ok || len(msgs) != 1 {
		t.Fatalf("Ready() from a fresh snapshot = %v %v", ok, msgs)
	}
	age := b.snapshotAgeSeconds()
	if age <= 0 || age > 120 || !snap.Authoritative() || testutil.ToFloat64(b.m.serving) != 1 {
		t.Fatalf("age %v, authoritative %v, serving %v", age, snap.Authoritative(), testutil.ToFloat64(b.m.serving))
	}
	t.Logf("fell back to local.db after %s; snapshot age %.1fs, %d objects", fellBack.Round(time.Millisecond), age, snap.Objects.Len())

	// A write while the replica is cut off.
	writer := newKubeStore(t, cfg, "zonecache-writer", string(ks.UID))
	if _, err := writer.Apply(ctx, rrset("apps", "late", "home", nil), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if b.State() != FromSnapshot {
		t.Fatalf("state %s while partitioned", b.State())
	}

	healed := time.Now()
	down.Store(false)
	eventually(t, 60*time.Second, "the swap to the live set", func() bool { return b.Snapshot().State == Live })
	eventually(t, 30*time.Second, "the late record set", func() bool {
		zs, _ := View[zoneSummary](b.Snapshot(), "zones")
		return reflect.DeepEqual(zs, zoneSummary{"home": {"apps/late", "apps/nas", "dns/ns1"}})
	})
	if testutil.ToFloat64(b.m.serving) != 0 || len(b.Degraded()) != 0 {
		t.Fatalf("after the swap: serving %v, degraded %v", testutil.ToFloat64(b.m.serving), b.Degraded())
	}
	t.Logf("swapped to live %s after the partition healed", time.Since(healed).Round(time.Millisecond))
	eventually(t, 30*time.Second, "local.db with the late record set", func() bool { return loadCount(db) == len(objs)+1 })
}
