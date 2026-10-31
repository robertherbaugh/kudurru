package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/cli"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func runKubeCLI(ctx context.Context, t *testing.T, env map[string]string, stdin string, args ...string) (string, error) {
	t.Helper()
	o := newCLIOpts()
	o.getenv = func(k string) string { return env[k] }
	o.stdin = strings.NewReader(stdin)
	root := newRootWith(o)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err != nil && errOut.Len() != 0 {
		return out.String(), &cli.KubernetesError{Cause: cliTestError{message: err.Error() + ": " + errOut.String()}}
	}
	return out.String(), err
}

type cliTestError struct{ message string }

func (e cliTestError) Error() string { return e.message }

func TestKubernetesTransportSelectionRefusals(t *testing.T) {
	for _, args := range [][]string{
		{"--transport", "typo", "get", "zones"},
		{"--kubeconfig", "absent", "get", "zones"},
		{"--transport", "kubernetes", "--server", "https://api.invalid", "get", "zones"},
		{"--transport", "kubernetes", "--token", "fixture", "get", "zones"},
		{"--transport", "kubernetes", "--cli-config", "absent", "get", "zones"},
		{"--transport", "kubernetes", "--insecure-skip-tls-verify", "get", "zones"},
		{"--transport", "kubernetes", "--ca-file", "absent", "get", "zones"},
		{"--transport", "kubernetes", "login"},
		{"--transport", "kubernetes", "serve"},
		{"--transport", "kubernetes", "api", "/nodes"},
	} {
		if _, err := runKubeCLI(t.Context(), t, map[string]string{}, "", args...); err == nil {
			t.Fatal("ambiguous or unsupported transport accepted")
		}
	}
	for _, env := range []map[string]string{{cli.EnvToken: "fixture"}, {cli.EnvServer: "https://api.invalid"}, {"KUBECONFIG": filepath.Join(t.TempDir(), "absent")}} {
		if _, err := runKubeCLI(t.Context(), t, env, "", "--transport", "kubernetes", "get", "zones"); err == nil {
			t.Fatal("missing source or ambient API override accepted")
		}
	}
}

func TestKubernetesCLIActualContextCRUDAndRBAC(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for actual direct CLI acceptance")
	}
	control := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{filepath.Join("..", "..", "deploy", "kustomize", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	control.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1").Set("endpoint-reconciler-type", "none")
	installed, err := control.Start()
	if err != nil {
		t.Fatal("direct CLI envtest failed to start")
	}
	t.Cleanup(func() {
		if err := control.Stop(); err != nil {
			t.Error("direct CLI envtest failed to stop")
		}
	})
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, rbacv1.AddToScheme, public.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := client.New(installed, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	for _, ns := range []string{"direct-a", "direct-b"} {
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "direct-reader"}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{public.Group}, Resources: []string{"dnszones"}, Verbs: []string{"get"}}}}
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "direct-reader"}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name}, Subjects: []rbacv1.Subject{{Kind: "User", Name: "direct-reader"}}}
	for _, obj := range []client.Object{role, binding} {
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	user := &clientcmdapi.AuthInfo{ClientCertificateData: installed.CertData, ClientKeyData: installed.KeyData}
	reader := user.DeepCopy()
	reader.Impersonate = "direct-reader"
	reader.ImpersonateGroups = []string{"system:authenticated"}
	cfg := clientcmdapi.Config{Clusters: map[string]*clientcmdapi.Cluster{"lab": {Server: installed.Host, CertificateAuthorityData: installed.CAData}}, AuthInfos: map[string]*clientcmdapi.AuthInfo{"admin": user, "reader": reader}, Contexts: map[string]*clientcmdapi.Context{"one": {Cluster: "lab", AuthInfo: "admin", Namespace: "direct-a"}, "two": {Cluster: "lab", AuthInfo: "admin", Namespace: "direct-b"}, "reader": {Cluster: "lab", AuthInfo: "reader", Namespace: "direct-a"}}, CurrentContext: "one"}
	configPath := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(cfg, configPath); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"KUBECONFIG": configPath}
	run := func(stdin string, args ...string) (string, error) {
		return runKubeCLI(ctx, t, env, stdin, append([]string{"--transport", "kubernetes", "--request-timeout", "10s"}, args...)...)
	}
	must := func(stdin string, args ...string) string {
		t.Helper()
		out, err := run(stdin, args...)
		if err != nil {
			t.Fatal("direct CLI failed", err)
		}
		return out
	}
	zone := `{"apiVersion":"kudurru.io/v1alpha1","kind":"DNSZone","metadata":{"name":"direct-cli"},"spec":{"zoneName":"direct.example.","type":"primary"}}`
	must(zone, "apply", "-f", "-")
	group := `{"apiVersion":"kudurru.io/v1alpha1","kind":"ClientGroup","metadata":{"name":"phone","labels":{"team":"blue"}},"spec":{"clients":[{"hostname":"phone.lan"}]}}`
	must(group, "apply", "-f", "-")
	must(group, "--context", "two", "apply", "-f", "-")
	var obj struct {
		Metadata metav1.ObjectMeta `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(must("", "get", "cg", "phone", "-o", "json")), &obj); err != nil || obj.Metadata.Namespace != "direct-a" {
		t.Fatal("kube context namespace not used")
	}
	if err := json.Unmarshal([]byte(must("", "--context", "two", "-n", "direct-a", "get", "cg", "phone", "-o", "json")), &obj); err != nil || obj.Metadata.Namespace != "direct-a" {
		t.Fatal("explicit namespace not applied")
	}
	listed := must("", "get", "cg", "-A", "-l", "team in (blue,green)", "-o", "json")
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(listed), &list); err != nil || len(list.Items) != 2 {
		t.Fatal("all namespaces or set-based selector lost")
	}
	if !strings.Contains(must("", "describe", "zone", "direct-cli"), "direct.example.") {
		t.Fatal("direct describe lost spec")
	}
	must("", "--context", "reader", "get", "zone", "direct-cli", "-o", "name")
	if _, err := run("", "--context", "reader", "get", "zones"); err == nil {
		t.Fatal("get-only user listed CRDs")
	}
	if _, err := run(zone, "--context", "reader", "apply", "-f", "-"); err == nil {
		t.Fatal("get-only user wrote CRD")
	}
	if _, err := run("", "--context", "missing", "get", "zones"); err == nil {
		t.Fatal("missing context fell back")
	}
	foreign := &public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "direct-a", Annotations: map[string]string{public.ManagedByAnnotation: "file:owned.yaml"}}, Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{IP: "192.0.2.1"}}}}
	if err := admin.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(string(raw), "apply", "-f", "-"); err == nil {
		t.Fatal("direct apply stole file ownership")
	}
	if _, err := run("", "delete", "cg", "foreign"); err == nil {
		t.Fatal("direct delete stole file ownership")
	}
	must("", "delete", "cg", "phone")
	must("", "delete", "cg", "phone", "--ignore-not-found")
	must("", "delete", "zone", "direct-cli")
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := runKubeCLI(canceled, t, env, "", "--transport", "kubernetes", "get", "zones"); err == nil {
		t.Fatal("canceled command accessed cluster")
	}
}
