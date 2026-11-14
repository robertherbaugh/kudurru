package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

// kubeIntegrationMount starts one runServe on the Kubernetes driver and
// returns a stop function that waits for it to return.
func kubeIntegrationMount(t *testing.T, cfg *config.Config) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error("server stopped with", err)
				}
			case <-time.After(30 * time.Second):
				t.Error("server did not return")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// TestRunServeKubeCloudflarePublicationAndRefusals mounts the Kubernetes
// integration host (D384) through runServe against envtest and a local TLS
// Cloudflare peer. It proves: Cloudflare publishes through real ledger,
// operation and budget Secrets with exactly one mutation; a suspended
// instance makes no provider request even when its source changes, and
// resuming publishes the change once; UniFi record publication and webhook
// instances, and an instance outside the release namespace, are refused on
// their objects with no provider request; an ambiguous effect is sent exactly
// once, is not replayed while its peer cannot be read, and a restart resumes
// it through fresh-reread recovery without sending it again.
func TestRunServeKubeCloudflarePublicationAndRefusals(t *testing.T) {
	control := &envtest.Environment{BinaryAssetsDirectory: mountedKubeTestAssets(t), CRDDirectoryPaths: []string{"../../deploy/kustomize/crd/bases"}, ErrorIfCRDPathMissing: true}
	control.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1").Set("endpoint-reconciler-type", "none")
	restConfig, err := control.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := control.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, public.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	var identity corev1.Namespace
	if err := admin.Get(t.Context(), client.ObjectKey{Name: "kube-system"}, &identity); err != nil {
		t.Fatal(err)
	}
	sign := trustTestKey(t)
	t.Setenv(license.EnvKey, sign(license.Claims{Plan: license.PlanPersonal, Nodes: 3, Features: []license.Feature{license.FeatureIntegrations}}))
	t.Setenv(license.EnvKeyFile, "")
	// The peer replaces http.DefaultTransport with one that refuses every
	// origin but Cloudflare's. The Kubernetes clients of runServe use the
	// default transport for the plain-HTTP API gate, so route only the
	// Cloudflare origin to the peer.
	peer := integrationCloudflareTLS(t)
	fixture := http.DefaultTransport.(*http.Transport)
	routed := fixture.Clone()
	toPeer := fixture.DialContext
	routed.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "api.cloudflare.com:443" {
			return toPeer(ctx, network, address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	http.DefaultTransport = routed
	t.Cleanup(func() { http.DefaultTransport = fixture; routed.CloseIdleConnections() })
	credential := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(credential, []byte("test-only-private-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	gate := newPartitionGate(t, restConfig)
	cfg := offlineServeConfig(t, gate.server.URL)
	cfg.Node.Name = "integration-a"
	cfg.Store.Kube.ExpectedClusterID = string(identity.UID)
	cfg.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	cfg.Credentials.AllowedFiles = []string{credential}

	ctx := t.Context()
	for _, object := range []client.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "example.", Nameservers: []string{"ns.example."}}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "published", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: "web.example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.80"}}}}},
	} {
		if err := admin.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	await := func(d time.Duration, what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(d)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatal("never converged:", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	counts := func() (requests, writes int) {
		peer.mu.Lock()
		defer peer.mu.Unlock()
		return peer.requests, peer.writes
	}
	target := func() string {
		peer.mu.Lock()
		defer peer.mu.Unlock()
		for _, row := range peer.rows {
			if row["name"] == "web.example" || row["name"] == "web.example." {
				content, _ := row["content"].(string)
				return content
			}
		}
		return ""
	}
	readyIn := func(key string) (*metav1.Condition, *public.Integration) {
		namespace, name, _ := strings.Cut(key, "/")
		var d public.Integration
		if err := admin.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &d); err != nil {
			return nil, nil
		}
		return meta.FindStatusCondition(d.Status.Conditions, "Ready"), &d
	}
	ready := func(name string) (*metav1.Condition, *public.Integration) { return readyIn("default/" + name) }
	carriers := func(kind string) []corev1.Secret {
		var list corev1.SecretList
		if err := admin.List(ctx, &list, client.InNamespace("default"), client.MatchingLabels{kube.InternalKindLabel: kind}); err != nil {
			t.Fatal(err)
		}
		return list.Items
	}
	pending := func() int {
		n := 0
		for _, sec := range carriers(kinds.KindIntegrationLedger) {
			var row kinds.IntegrationLedger
			if err := json.Unmarshal(sec.Data[kube.InternalObjectKey], &row); err != nil {
				t.Fatal(err)
			}
			if row.Spec.Pending != nil {
				n++
			}
		}
		return n
	}

	stop := kubeIntegrationMount(t, cfg)
	publisher := &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: "publisher", Namespace: "default"}, Spec: public.IntegrationSpec{Type: "cloudflare", Resync: metav1.Duration{Duration: time.Hour}, Credentials: map[string]public.CredentialReference{"apiToken": {File: credential}}, Cloudflare: &public.CloudflareIntegration{Zones: []public.CloudflareZone{{Zone: "example", ZoneID: integrationCloudflareZone}}}}}
	if err := admin.Create(ctx, publisher); err != nil {
		t.Fatal(err)
	}
	await(60*time.Second, "the Cloudflare instance publishes and reports Ready", func() bool {
		c, _ := ready("publisher")
		return target() == "192.0.2.80" && c != nil && c.Status == metav1.ConditionTrue
	})
	_, writes := counts()
	if writes != 1 {
		t.Fatal("first publication was not exactly one mutation", writes)
	}
	if len(carriers(kinds.KindIntegrationLedger)) != 1 || len(carriers(kinds.KindIntegrationOperation)) != 1 || len(carriers(kinds.KindIntegrationBudget)) != 1 {
		t.Fatal("ledger, operation and budget were not written as private Secrets", len(carriers(kinds.KindIntegrationLedger)), len(carriers(kinds.KindIntegrationOperation)), len(carriers(kinds.KindIntegrationBudget)))
	}
	if pending() != 0 {
		t.Fatal("a confirmed publication left a pending intent")
	}
	// No further mutation without a change: the status write does not wake
	// or replace the instance.
	time.Sleep(5 * time.Second)
	if _, again := counts(); again != 1 {
		t.Fatal("an unchanged source mutated the provider again", again)
	}

	// Refused instances: no provider request, a clear reason on the object.
	// An Integration outside the release namespace naming the allowlisted
	// credential is refused too: the allowlist is not lent to other
	// namespaces (D384).
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant"}}); err != nil {
		t.Fatal(err)
	}
	requestsBefore, _ := counts()
	// UniFi record publication (spec.unifi.inventory) and webhook instances
	// do not run on this store; UniFi inventory and groups alone do (D384).
	unifi := &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: "inventory", Namespace: "default"}, Spec: public.IntegrationSpec{Type: "unifi", Credentials: map[string]public.CredentialReference{"apiKey": {File: credential}}, UniFi: &public.UniFiIntegration{Endpoint: "https://unifi.invalid", Site: "default", Poll: metav1.Duration{Duration: time.Minute}, Inventory: &public.UniFiInventory{ZoneRef: public.ObjectRef{Name: "home"}}}}}
	hook := &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: "hook", Namespace: "default"}, Spec: public.IntegrationSpec{Type: "webhook", DeletionPolicy: public.IntegrationDeletionDelete, Webhook: &public.WebhookIntegration{Endpoint: "https://webhook.invalid"}}}
	foreign := publisher.DeepCopy()
	foreign.ObjectMeta = metav1.ObjectMeta{Name: "foreign", Namespace: "tenant"}
	for _, d := range []*public.Integration{unifi, hook, foreign} {
		if err := admin.Create(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	for name, reason := range map[string]string{"default/inventory": reasonRecordsUnsupported, "default/hook": reasonUnsupportedOnKubernetes, "tenant/foreign": reasonNamespaceNotAllowed} {
		await(60*time.Second, name+" refused on its object", func() bool {
			c, _ := readyIn(name)
			return c != nil && c.Status == metav1.ConditionFalse && c.Reason == reason
		})
	}
	if requests, _ := counts(); requests != requestsBefore {
		t.Fatal("a refused instance reached the provider", requests-requestsBefore)
	}

	// Suspended: a source change makes no provider request; resuming
	// publishes it exactly once.
	var current public.Integration
	if err := admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: "publisher"}, &current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Suspend = true
	if err := admin.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	requestsBefore, writesBefore := counts()
	var record public.DNSRecordSet
	if err := admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: "published"}, &record); err != nil {
		t.Fatal(err)
	}
	record.Spec.Endpoints[0].Targets = []string{"192.0.2.81"}
	if err := admin.Update(ctx, &record); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Second)
	if requests, w := counts(); requests != requestsBefore || w != writesBefore {
		t.Fatal("a suspended instance reached the provider", requests-requestsBefore, w-writesBefore)
	}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: "publisher"}, &current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Suspend = false
	if err := admin.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	await(60*time.Second, "the resumed instance publishes the change", func() bool { return target() == "192.0.2.81" })
	time.Sleep(3 * time.Second)
	if _, w := counts(); w != writesBefore+1 {
		t.Fatal("resuming did not publish exactly one mutation", w-writesBefore)
	}

	// An ambiguous effect: the peer applies the batch but answers 503 and
	// then cannot be read. The intent stays pending; nothing is replayed.
	peer.mu.Lock()
	peer.ambiguous = true
	peer.mu.Unlock()
	_, writesBefore = counts()
	if err := admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: "published"}, &record); err != nil {
		t.Fatal(err)
	}
	record.Spec.Endpoints[0].Targets = []string{"192.0.2.82"}
	if err := admin.Update(ctx, &record); err != nil {
		t.Fatal(err)
	}
	await(60*time.Second, "the ambiguous effect is sent and leaves a pending intent", func() bool {
		_, w := counts()
		return pending() == 1 && w-writesBefore >= 1
	})
	// The controller's status writes on the changed record set and zone
	// change no published input: they neither fence the reserved intent
	// before its request nor make the host send it again (D384).
	time.Sleep(5 * time.Second)
	if _, w := counts(); w-writesBefore != 1 || pending() != 1 {
		t.Fatal("the ambiguous effect was not sent exactly once before recovery", w-writesBefore, pending())
	}
	// Restart with the peer healthy: the new process resumes the pending
	// intent by a fresh complete reread and does not send it again.
	stop()
	peer.mu.Lock()
	peer.ambiguous, peer.unreadable = false, false
	peer.mu.Unlock()
	cfg2 := offlineServeConfig(t, gate.server.URL)
	cfg2.Node.Name = "integration-a"
	cfg2.Store.Kube.ExpectedClusterID = string(identity.UID)
	cfg2.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
	cfg2.Blocking.Enabled = false
	cfg2.DNSSEC.Enabled = false
	cfg2.Credentials.AllowedFiles = []string{credential}
	_ = kubeIntegrationMount(t, cfg2)
	await(90*time.Second, "the restarted host resolves the pending intent", func() bool {
		c, _ := ready("publisher")
		return pending() == 0 && target() == "192.0.2.82" && c != nil && c.Status == metav1.ConditionTrue
	})
	if _, w := counts(); w != writesBefore+1 {
		t.Fatal("recovery replayed the ambiguous effect", w-writesBefore)
	}
	// Every private carrier the host wrote decodes strictly.
	for _, kind := range []string{kinds.KindIntegrationLedger, kinds.KindIntegrationOperation, kinds.KindIntegrationBudget} {
		for _, sec := range carriers(kind) {
			if sec.Type != kube.InternalSecretType || !bytes.Contains(sec.Data[kube.InternalObjectKey], []byte(`"kind":"`+kind+`"`)) {
				t.Fatal("unexpected carrier", kind, sec.Name)
			}
		}
	}
}

// The Kubernetes ledger seam writes only private integration kinds by one
// create-only or exact-resourceVersion write, never forced, and never
// forgets the shared budget (D384). These refusals precede any fence or I/O.
func TestKubeIntegrationWriteSeamRefusals(t *testing.T) {
	r := &kubeIntegrationRuntime{}
	ledgerRow := &kinds.IntegrationLedger{ObjectMeta: metav1.ObjectMeta{Name: "il-x"}}
	for name, tc := range map[string]struct {
		obj store.Object
		opt store.ApplyOptions
	}{
		"public kind":         {&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"}}, store.ApplyOptions{CreateOnly: true}},
		"source head":         {&kinds.IntegrationSourceHead{ObjectMeta: metav1.ObjectMeta{Name: "h"}}, store.ApplyOptions{CreateOnly: true}},
		"force":               {ledgerRow, store.ApplyOptions{CreateOnly: true, Force: true}},
		"no CAS":              {ledgerRow, store.ApplyOptions{}},
		"create and expected": {ledgerRow, store.ApplyOptions{CreateOnly: true, ExpectedResourceVersion: "1"}},
	} {
		if _, err := r.write(t.Context(), ledger.Guard{}, tc.obj, tc.opt); !errors.Is(err, ih.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, request := range map[string]store.DeleteRequest{
		"budget":     {Key: store.Key{Kind: kinds.KindIntegrationBudget, Name: "b"}, Options: store.DeleteOptions{ExpectedUID: "u", ExpectedResourceVersion: "1"}},
		"public":     {Key: store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "r"}, Options: store.DeleteOptions{ExpectedUID: "u", ExpectedResourceVersion: "1"}},
		"unguarded":  {Key: store.Key{Kind: kinds.KindIntegrationLedger, Name: "l"}},
		"force":      {Key: store.Key{Kind: kinds.KindIntegrationLedger, Name: "l"}, Options: store.DeleteOptions{ExpectedUID: "u", ExpectedResourceVersion: "1", Force: true}},
		"namespaced": {Key: store.Key{Kind: kinds.KindIntegrationLedger, Namespace: "default", Name: "l"}, Options: store.DeleteOptions{ExpectedUID: "u", ExpectedResourceVersion: "1"}},
	} {
		if err := r.forget(t.Context(), ledger.Guard{}, []store.DeleteRequest{request}); !errors.Is(err, ih.ErrInvalid) {
			t.Errorf("forget %s: %v", name, err)
		}
	}
	release := metav1.ObjectMeta{Namespace: "kudurru"}
	for d, want := range map[*public.Integration]string{
		{ObjectMeta: release, Spec: public.IntegrationSpec{Type: "cloudflare"}}:                                                                  "",
		{ObjectMeta: release, Spec: public.IntegrationSpec{Type: "cloudflare", DeletionPolicy: public.IntegrationDeletionDelete}}:                "",
		{ObjectMeta: release, Spec: public.IntegrationSpec{Type: "unifi", UniFi: &public.UniFiIntegration{}}}:                                    "",
		{ObjectMeta: release, Spec: public.IntegrationSpec{Type: "unifi", UniFi: &public.UniFiIntegration{Inventory: &public.UniFiInventory{}}}}: reasonRecordsUnsupported,
		{ObjectMeta: release, Spec: public.IntegrationSpec{Type: "webhook"}}:                                                                     reasonUnsupportedOnKubernetes,
		// The credential allowlist is never lent to another namespace.
		{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant"}, Spec: public.IntegrationSpec{Type: "cloudflare"}}: reasonNamespaceNotAllowed,
	} {
		if got := kubeIntegrationRefusal("kudurru", d); got != want {
			t.Errorf("refusal of %s %s/%s = %q, want %q", d.Namespace, d.Spec.Type, d.Spec.DeletionPolicy, got, want)
		}
	}
}
