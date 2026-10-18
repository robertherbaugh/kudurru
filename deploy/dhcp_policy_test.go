package deploy_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestDHCPPodLabelPolicy installs the chart's DHCP RBAC and admission policy
// on a real apiserver and acts as the kudurru ServiceAccount: it may set and
// clear kudurru.io/dhcp-active on its release's pods, and nothing else.
func TestDHCPPodLabelPolicy(t *testing.T) {
	assets := envtestAssets(t)
	out, err := renderChart(t, "dhcp: {enabled: true, serverAddress: 192.0.2.67}")
	if err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{BinaryAssetsDirectory: assets}
	args := env.ControlPlane.GetAPIServer().Configure()
	args.Set("advertise-address", "127.0.0.1")
	args.Set("endpoint-reconciler-type", "none")
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: releaseNS}}); err != nil {
		t.Fatal(err)
	}
	wanted := map[string]bool{"ServiceAccount": true, "Role": true, "RoleBinding": true,
		"ValidatingAdmissionPolicy": true, "ValidatingAdmissionPolicyBinding": true}
	for _, doc := range decodeDocs(t, out) {
		if !wanted[str(doc["kind"])] {
			continue
		}
		obj := &unstructured.Unstructured{Object: doc}
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatalf("create %s %s: %v", obj.GetKind(), obj.GetName(), err)
		}
	}
	container := []corev1.Container{{Name: "kudurru", Image: "kudurru"}}
	ours := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "kudurru-5d9f-abcde", Namespace: releaseNS,
		Labels: map[string]string{"app.kubernetes.io/name": "kudurru", "app.kubernetes.io/instance": "kudurru", "app.kubernetes.io/component": "dns"}},
		Spec: corev1.PodSpec{Containers: container}}
	other := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: releaseNS, Labels: map[string]string{"app": "web"}},
		Spec: corev1.PodSpec{Containers: container}}
	for _, p := range []*corev1.Pod{ours, other} {
		if err := admin.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	as := rest.CopyConfig(cfg)
	as.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + releaseNS + ":kudurru",
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + releaseNS, "system:authenticated"}}
	sa, err := client.New(as, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	patch := func(name, body string) error {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: releaseNS}}
		return sa.Patch(ctx, pod, client.RawPatch(types.MergePatchType, []byte(body)))
	}
	denied := func(err error) bool { return err != nil && strings.Contains(err.Error(), "ValidatingAdmissionPolicy") }
	// The policy takes effect a moment after it is created. Each attempt
	// writes a new value: repeating an applied patch would change nothing.
	deadline := time.Now().Add(30 * time.Second)
	for attempt := 0; !denied(patch(ours.Name, fmt.Sprintf(`{"metadata":{"labels":{"app":"hijacked-%d"}}}`, attempt))); attempt++ {
		if time.Now().After(deadline) {
			t.Fatal("the ServiceAccount could change another label")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := patch(ours.Name, `{"metadata":{"labels":{"kudurru.io/dhcp-active":"true"}}}`); err != nil {
		t.Fatalf("setting the DHCP label: %v", err)
	}
	if err := patch(ours.Name, `{"metadata":{"labels":{"kudurru.io/dhcp-active":null}}}`); err != nil {
		t.Fatalf("clearing the DHCP label: %v", err)
	}
	for name, body := range map[string]string{
		other.Name: `{"metadata":{"labels":{"kudurru.io/dhcp-active":"true"}}}`,
		ours.Name:  `{"metadata":{"annotations":{"note":"x"}}}`,
	} {
		if err := patch(name, body); !denied(err) {
			t.Errorf("patch %s %s = %v, want denied by the policy", name, body, err)
		}
	}
	if err := patch(ours.Name, `{"metadata":{"labels":{"kudurru.io/dhcp-active":"yes"}}}`); !denied(err) {
		t.Errorf("a label value other than true = %v, want denied", err)
	}
}

// envtestAssets is $KUBEBUILDER_ASSETS or the newest bin/k8s that `make
// envtest` installed; it skips the test without them.
func envtestAssets(t *testing.T) string {
	t.Helper()
	if os.Getenv("KUDURRU_SKIP_ENVTEST") != "" {
		t.Skip("KUDURRU_SKIP_ENVTEST is set")
	}
	if d := os.Getenv("KUBEBUILDER_ASSETS"); d != "" {
		return d
	}
	dirs, _ := filepath.Glob(filepath.Join(repoRoot(t), "bin", "k8s", "*-"+goruntime.GOOS+"-"+goruntime.GOARCH))
	sort.Strings(dirs)
	if len(dirs) == 0 {
		t.Skip("envtest binaries not found: run `make envtest` or set KUBEBUILDER_ASSETS")
	}
	return dirs[len(dirs)-1]
}
