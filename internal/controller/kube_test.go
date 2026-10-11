package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

// The kube driver tests run the controller against a real apiserver with
// the kudurru CRDs (status subresource, CEL, optimistic concurrency). They
// skip without KUBEBUILDER_ASSETS.
var (
	kubeCfg  *rest.Config
	kubeSkip string
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" || os.Getenv("KUDURRU_SKIP_ENVTEST") != "" {
		kubeSkip = "KUBEBUILDER_ASSETS is not set (or KUDURRU_SKIP_ENVTEST is)"
		return m.Run()
	}
	if os.Getenv("KUDURRU_TEST_LOGS") == "" {
		ctrllog.SetLogger(logr.Discard())
		klog.SetLogger(logr.Discard())
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	args := env.ControlPlane.GetAPIServer().Configure()
	args.Set("advertise-address", "127.0.0.1")
	args.Set("endpoint-reconciler-type", "none")
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "controller: envtest failed to start: %v\n", err)
		return 1
	}
	defer func() { _ = env.Stop() }()
	core, err := corev1client.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, ns := range []string{"apps", "team-a", "team-b", "home", "kudurru-system"} {
		if _, err := core.Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	kubeCfg = cfg
	return m.Run()
}

// kubeEnv returns an env over a fresh kube driver whose objects are
// isolated by a unique zone name prefix.
func kubeEnv(t *testing.T, mutate ...func(*Options)) *env {
	t.Helper()
	if kubeCfg == nil {
		t.Skip(kubeSkip)
	}
	s, err := kube.New(kubeCfg, kube.Options{Namespace: "kudurru-system", Identity: "test", Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !s.WaitForSync(ctx) {
		t.Fatal("informers did not sync")
	}
	return newEnvOver(t, s, mutate...)
}

func uniq(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%1_000_000_000)
}

// TestKubeDriver runs the zone, duplicate and policy paths against the
// apiserver: the conditions pass CRD validation, status writes use the
// status subresource with optimistic concurrency, and nothing loops.
func TestKubeDriver(t *testing.T) {
	e := kubeEnv(t)
	e.start()
	zn := uniq("z")
	origin := zn + ".example."
	e.apply(zoneObj(zn, origin))
	first := rrsetObj("team-a", uniq("web"), zn, 0, ep("www."+origin, "A", "192.0.2.1"), ep("www.elsewhere.org", "A", "192.0.2.5"))
	e.apply(first)
	// The apiserver stamps creationTimestamp with one-second resolution;
	// make the second object unambiguously later.
	time.Sleep(1100 * time.Millisecond)
	second := rrsetObj("team-b", uniq("web"), zn, 0, ep("www."+origin, "A", "192.0.2.99"), ep("b."+origin, "A", "192.0.2.2"))
	e.apply(second)

	want := ContentHash([]RecordTuple{
		{Name: "www." + origin, Type: "A", TTL: 300, Targets: []string{"192.0.2.1"}},
		{Name: "b." + origin, Type: "A", TTL: 300, Targets: []string{"192.0.2.2"}},
	})
	e.eventually("zone converged", func() bool {
		z := e.zone(zn)
		return z.Status.ContentHash == want && z.Status.RecordCount == 2 && z.Status.Serial != 0
	})
	e.eventually("record set conditions", func() bool {
		a := e.rrset("team-a", first.Name)
		b := e.rrset("team-b", second.Name)
		return cond(a.Status.Conditions, v1alpha1.ConditionDegraded) == "True/OutsideZone" &&
			cond(b.Status.Conditions, v1alpha1.ConditionDegraded) == "True/Duplicated" &&
			cond(b.Status.Conditions, v1alpha1.ConditionReady) == "True/Serving"
	})
	pol := &v1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Namespace: "home", Name: uniq("p")}, Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"nope"}}}
	e.apply(pol)
	e.eventually("policy degraded", func() bool {
		return cond(e.policy("home", pol.Name).Status.Conditions, v1alpha1.ConditionDegraded) == "True/MissingReference"
	})
	e.settle()
	writes := e.st.Writes("")
	serial := e.zone(zn).Status.Serial

	// Re-applying identical objects (no generation change) writes nothing.
	for _, o := range []store.Object{zoneObj(zn, origin), first, second, pol} {
		e.apply(o.DeepCopyObject().(store.Object))
	}
	time.Sleep(500 * time.Millisecond)
	e.settle()
	if n := e.st.Writes(""); n != writes {
		t.Fatalf("%d status writes after identical re-applies", n-writes)
	}
	if s := e.zone(zn).Status.Serial; s != serial {
		t.Fatalf("serial moved %d -> %d", serial, s)
	}

	// A content change bumps the serial once.
	changed := second.DeepCopy()
	changed.Spec.Endpoints[1].Targets = []string{"192.0.2.3"}
	e.apply(changed)
	e.eventually("serial bumped", func() bool { return e.zone(zn).Status.Serial == serial+1 })
	if n := e.eventCount(EventDuplicated, store.KeyOf(second)); n != 1 {
		t.Fatalf("Duplicated events %d", n)
	}
	var stamps []string
	for _, ev := range e.events.Events() {
		if ev.Reason == EventSerialBumped && ev.Key.Name == zn {
			stamps = append(stamps, ev.Message)
		}
	}
	// One event per stamped serial (the zone was stamped as its record
	// sets arrived), the last one for the change above. The exact count
	// depends on wall-clock seconds elapsed (serials are max(prev+1, now)),
	// so only require distinct stamps and the final transition.
	if len(stamps) == 0 || !strings.Contains(stamps[len(stamps)-1], fmt.Sprintf("%d -> %d:", serial, serial+1)) {
		t.Fatalf("SerialBumped events %q", stamps)
	}
	seen := map[string]bool{}
	for _, st := range stamps {
		if seen[st] {
			t.Fatalf("duplicate SerialBumped event %q", st)
		}
		seen[st] = true
	}
}
