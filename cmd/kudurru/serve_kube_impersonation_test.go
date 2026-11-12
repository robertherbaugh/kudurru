package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
)

// impersonationAuditPolicy records every write of a DNSZone.
const impersonationAuditPolicy = `apiVersion: audit.k8s.io/v1
kind: Policy
omitStages: ["RequestReceived"]
rules:
- level: Metadata
  verbs: ["create", "patch", "update", "delete"]
  resources:
  - group: kudurru.io
    resources: ["dnszones"]
- level: None
`

type impersonationAuditEvent struct {
	Verb string `json:"verb"`
	User struct {
		Username string `json:"username"`
	} `json:"user"`
	ImpersonatedUser *struct {
		Username string   `json:"username"`
		Groups   []string `json:"groups"`
	} `json:"impersonatedUser"`
	ObjectRef struct {
		Resource string `json:"resource"`
		Name     string `json:"name"`
	} `json:"objectRef"`
	ResponseStatus struct {
		Code int `json:"code"`
	} `json:"responseStatus"`
}

// impersonationAudit returns the successful apiserver write of the zone,
// waiting for the audit backend to flush it.
func impersonationAudit(t *testing.T, path, zone string) impersonationAuditEvent {
	t.Helper()
	return impersonationAuditVerb(t, path, zone, "")
}

// impersonationAuditVerb is impersonationAudit for one verb ("" is any
// write).
func impersonationAuditVerb(t *testing.T, path, zone, verb string) impersonationAuditEvent {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		f, err := os.Open(path) //nolint:gosec // test-owned temporary file
		if err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 0, 1<<16), 1<<22)
			for sc.Scan() {
				var ev impersonationAuditEvent
				if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.ObjectRef.Resource == "dnszones" && ev.ObjectRef.Name == zone &&
					ev.ResponseStatus.Code >= 200 && ev.ResponseStatus.Code < 300 && ev.Verb != "get" && (verb == "" || ev.Verb == verb) {
					_ = f.Close()
					return ev
				}
			}
			_ = f.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("no successful apiserver %q write of dnszones/%s in the audit log", verb, zone)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (b *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// TestRunServeKubeImpersonation proves D386 and D387 through the mounted
// server on a real apiserver. In sar mode (the default) kudurru writes as
// itself: a service account that only has a kudurru RoleBinding may write, a
// local admin needs no Kubernetes RBAC, and the apiserver audit log names
// kudurru's identity. In impersonate mode the same writes are sent as the
// principal: Kubernetes RBAC refuses them (403 forbidden, nothing written)
// until it grants the principal, and the audit log names the principal.
// The Kubernetes runtime also reports, before any attempt, that it offers no
// authenticator or local-user commands, and refuses them with their keys.
func TestRunServeKubeImpersonation(t *testing.T) {
	licenseKey := zoneGroupLicense(t)
	auditPath := filepath.Join(t.TempDir(), "audit.log")
	policyPath := filepath.Join(t.TempDir(), "audit-policy.yaml")
	if err := os.WriteFile(policyPath, []byte(impersonationAuditPolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	control := &envtest.Environment{BinaryAssetsDirectory: mountedKubeTestAssets(t), CRDDirectoryPaths: []string{"../../deploy/kustomize/crd/bases"}, ErrorIfCRDPathMissing: true}
	control.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1").Set("endpoint-reconciler-type", "none").
		Set("audit-policy-file", policyPath).Set("audit-log-path", auditPath).Set("audit-log-format", "json")
	actual, err := control.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := control.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeAdmin, err := client.New(actual, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	clientset, err := kubernetes.NewForConfig(actual)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	var kubeSystem corev1.Namespace
	if err := kubeAdmin.Get(ctx, client.ObjectKey{Name: "kube-system"}, &kubeSystem); err != nil {
		t.Fatal(err)
	}
	create := func(objects ...client.Object) {
		t.Helper()
		for _, o := range objects {
			if err := kubeAdmin.Create(ctx, o); err != nil {
				t.Fatal(err)
			}
		}
	}
	// writer: only a kudurru RoleBinding. editor: Kubernetes RBAC on
	// dnszones, no kudurru binding. nobody: neither.
	zoneRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "zone-editor"}, Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{public.Group}, Resources: []string{"dnszones"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
	}}
	create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "dns"}}, zoneRole,
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "writer", Namespace: "dns"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "editor", Namespace: "dns"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "nobody", Namespace: "dns"}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "editor-zones"}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: zoneRole.Name},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "editor", Namespace: "dns"}}},
		&public.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "writer-admin", Namespace: "default"},
			Spec: public.RoleBindingSpec{Role: "admin", Subjects: []public.BindingSubject{{Kind: "ServiceAccount", Name: "writer", Namespace: "dns"}}}})
	token := func(name string) string {
		t.Helper()
		expiry := int64(600)
		tr, err := clientset.CoreV1().ServiceAccounts("dns").CreateToken(ctx, name, &authenticationv1.TokenRequest{
			Spec: authenticationv1.TokenRequestSpec{Audiences: []string{"kudurru"}, ExpirationSeconds: &expiry}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return tr.Status.Token
	}
	bearer := func(base, name string) *zoneGroupClient {
		transport := &http.Transport{DisableKeepAlives: true}
		t.Cleanup(transport.CloseIdleConnections)
		return &zoneGroupClient{t: t, base: base, c: &http.Client{Timeout: 10 * time.Second, Transport: &bearerTransport{token: token(name), next: transport}}}
	}

	target, err := url.Parse(actual.Host)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	if proxy.Transport, err = rest.TransportFor(actual); err != nil {
		t.Fatal(err)
	}
	gate := httptest.NewServer(proxy)
	t.Cleanup(gate.Close)
	mount := func(mode string) (string, func()) {
		t.Helper()
		cfg := offlineServeConfig(t, gate.URL)
		cfg.Store.Kube.ExpectedClusterID = string(kubeSystem.UID)
		cfg.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
		cfg.Blocking.Enabled = false
		cfg.DNSSEC.Enabled = false
		cfg.Cache.MaxEntries = 0
		cfg.Auth.Session.Secure = "never"
		cfg.Auth.Kubernetes.Mode = mode
		stop := startLicensedMount(t, cfg, licenseKey)
		base := "http://" + cfg.Listeners.API.Address + api.BasePath
		return base, stop
	}
	zoneAbsent := func(name string) {
		t.Helper()
		if err := kubeAdmin.Get(ctx, client.ObjectKey{Name: name}, &public.DNSZone{}); !apierrors.IsNotFound(err) {
			t.Fatalf("refused write of zone %s left an object: %v", name, err)
		}
	}
	envelopeKey := func(raw []byte) string {
		var e struct {
			Error struct {
				Key string `json:"key"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return e.Error.Key
	}
	put := func(c *zoneGroupClient, zone string, want int) []byte {
		t.Helper()
		status, body := c.call(http.MethodPut, "/objects/dnszones/"+zone, zoneGroupZoneDoc(zone, ""))
		if status != want {
			t.Fatalf("PUT zone %s: HTTP %d, want %d: %s", zone, status, want, body)
		}
		return body
	}

	// sar (default): writes run as kudurru.
	base, stop := mount(config.KubernetesModeSAR)
	admin := newZoneGroupClient(t, base, "admin", "offline-test-password-1")
	put(admin, "sar-admin", http.StatusCreated)
	put(bearer(base, "writer"), "sar-writer", http.StatusCreated)
	if body := put(bearer(base, "nobody"), "sar-nobody", http.StatusForbidden); envelopeKey(body) != api.KeyForbidden {
		t.Fatalf("SAR denial key: %s", body)
	}
	zoneAbsent("sar-nobody")
	for _, zone := range []string{"sar-admin", "sar-writer"} {
		if ev := impersonationAudit(t, auditPath, zone); ev.ImpersonatedUser != nil {
			t.Fatalf("sar mode impersonated %s for zone %s", ev.ImpersonatedUser.Username, zone)
		}
	}
	serverUser := impersonationAudit(t, auditPath, "sar-admin").User.Username
	stop()

	// impersonate: the same principals, now decided by Kubernetes RBAC.
	base, stop = mount(config.KubernetesModeImpersonate)
	defer stop()
	admin = newZoneGroupClient(t, base, "admin", "offline-test-password-1")
	if body := put(admin, "imp-admin", http.StatusForbidden); envelopeKey(body) != api.KeyForbidden || !bytes.Contains(body, []byte("kudurru:user:admin")) {
		t.Fatalf("impersonated refusal: %s", body)
	}
	zoneAbsent("imp-admin")
	if body := put(bearer(base, "writer"), "imp-writer", http.StatusForbidden); envelopeKey(body) != api.KeyForbidden ||
		!bytes.Contains(body, []byte("system:serviceaccount:dns:writer")) {
		t.Fatalf("a kudurru binding alone must not pass Kubernetes RBAC: %s", body)
	}
	zoneAbsent("imp-writer")
	put(bearer(base, "nobody"), "imp-nobody", http.StatusForbidden)
	zoneAbsent("imp-nobody")

	put(bearer(base, "editor"), "imp-editor", http.StatusCreated)
	if ev := impersonationAudit(t, auditPath, "imp-editor"); ev.ImpersonatedUser == nil || ev.ImpersonatedUser.Username != "system:serviceaccount:dns:editor" || ev.User.Username != serverUser {
		t.Fatalf("audit of the service account write: %+v", ev)
	}

	// Grant the local admin's Kubernetes name: the write now passes.
	create(&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "kudurru-admin-zones"}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: zoneRole.Name},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "kudurru:user:admin"}}})
	deadline := time.Now().Add(15 * time.Second)
	for {
		status, body := admin.call(http.MethodPut, "/objects/dnszones/imp-admin", zoneGroupZoneDoc("imp-admin", ""))
		if status == http.StatusCreated {
			break
		}
		if status != http.StatusForbidden || time.Now().After(deadline) {
			t.Fatalf("PUT after the Kubernetes grant: HTTP %d %s", status, body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ev := impersonationAudit(t, auditPath, "imp-admin"); ev.ImpersonatedUser == nil || ev.ImpersonatedUser.Username != "kudurru:user:admin" || ev.User.Username != serverUser {
		t.Fatalf("audit of the impersonated admin write: %+v", ev)
	}
	// Deletes are impersonated too: zone-editor grants the admin delete.
	if status, body := admin.call(http.MethodDelete, "/objects/dnszones/imp-editor", nil); status != http.StatusNoContent && status != http.StatusOK {
		t.Fatalf("impersonated delete: HTTP %d %s", status, body)
	}
	if ev := impersonationAuditVerb(t, auditPath, "imp-editor", "delete"); ev.ImpersonatedUser == nil || ev.ImpersonatedUser.Username != "kudurru:user:admin" || ev.User.Username != serverUser {
		t.Fatalf("audit of the impersonated delete: %+v", ev)
	}

	// D387: the Kubernetes runtime announces and refuses the account commands.
	var status api.AuthStatus
	if err := json.Unmarshal(admin.await("/auth/status", http.StatusOK), &status); err != nil {
		t.Fatal(err)
	}
	if status.Capabilities.FactorManagement || status.Capabilities.LocalUserCommands {
		t.Fatalf("Kubernetes capabilities %+v, want none", status.Capabilities)
	}
	for _, tc := range []struct {
		path string
		body any
		key  string
	}{
		{"/auth/totp/setup", api.TOTPProofRequest{CurrentPassword: "offline-test-password-1"}, "factor_management_unavailable"},
		{"/iam/users", api.CreateLocalUserRequest{Name: "second", Password: "second-password-1"}, "user_management_unavailable"},
	} {
		code, body := admin.call(http.MethodPost, tc.path, tc.body)
		if code != http.StatusServiceUnavailable || envelopeKey(body) != tc.key {
			t.Fatalf("POST %s: HTTP %d %s, want 503 %s", tc.path, code, body, tc.key)
		}
	}
}
