package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

// TestRunServeKubeUniFiCommandOutcomesAndRefusedCleanup mounts one replica
// on envtest with the UniFi HTTPS double (F09b-K4, K6, DEL-8, D384). It
// shows: the CLI lists, checks, lists actions and runs a reviewed observe
// sync on Kubernetes, and a viewer token is refused the sync; a reviewed
// sync whose head write reached the API server but whose answer was lost is
// resolved by rereading the head and reported committed, with exactly one
// new head; one whose head write never arrived is answered
// integration_uncertain with the intent pending, the head unchanged, and is
// not replayed; a command to a suspended instance names no holder and says
// why it does not run; and a Delete instance that was armed and then became
// refused (it gained spec.unifi.inventory) still purges its source carriers
// and is removed when deleted.
func TestRunServeKubeUniFiCommandOutcomesAndRefusedCleanup(t *testing.T) {
	env := newKubeIntegrationEnv(t)
	ctx := t.Context()
	server, controllerCalls, _ := kubeUniFiController(t, "127.0.0.9")
	credential := kubeTestCredential(t)
	meta0 := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "default"} }
	if err := env.admin.Create(ctx, &public.ClientGroup{ObjectMeta: meta0("devices"), Spec: public.ClientGroupSpec{Sources: &public.ClientGroupSources{IntegrationRef: &public.ObjectRef{Name: "inventory"}}}}); err != nil {
		t.Fatal(err)
	}
	gate := newPartitionGate(t, env.rest)
	cfg := env.replica(t, "outcome-a", gate, credential)
	_ = kubeIntegrationMount(t, cfg)
	kubeAwait(t, 60*time.Second, "the replica answers the API", func() bool {
		status, _ := env.api(t, cfg, http.MethodGet, "/integrations?namespace=default", nil)
		return status == http.StatusOK
	})
	sum := sha256.Sum256(server.Certificate().Raw)
	// An hour's poll: only reviewed syncs commit while the test runs.
	inventory := &public.Integration{ObjectMeta: meta0("inventory"), Spec: public.IntegrationSpec{Type: "unifi", DeletionPolicy: public.IntegrationDeletionDelete, Resync: metav1.Duration{Duration: time.Hour}, Credentials: map[string]public.CredentialReference{"apiKey": {File: credential}}, UniFi: &public.UniFiIntegration{Endpoint: server.URL, TLSFingerprint: "sha256:" + hex.EncodeToString(sum[:]), Site: "default", Poll: metav1.Duration{Duration: time.Hour}, Groups: public.UniFiGroups{ByNetwork: map[string]string{"LAN": "devices"}}}}}
	if err := env.admin.Create(ctx, inventory); err != nil {
		t.Fatal(err)
	}
	carriers := func(kind string) []corev1.Secret {
		var list corev1.SecretList
		if err := env.admin.List(ctx, &list, client.InNamespace("default"), client.MatchingLabels{kube.InternalKindLabel: kind}); err != nil {
			t.Fatal(err)
		}
		return list.Items
	}
	sequence := func() uint64 {
		heads := carriers(kinds.KindIntegrationSourceHead)
		if len(heads) != 1 {
			return 0
		}
		var head kinds.IntegrationSourceHead
		if err := json.Unmarshal(heads[0].Data[kube.InternalObjectKey], &head); err != nil {
			t.Fatal(err)
		}
		return head.Spec.Sequence
	}
	var current public.Integration
	read := func() {
		t.Helper()
		if err := env.admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: "inventory"}, &current); err != nil {
			t.Fatal(err)
		}
	}
	kubeAwait(t, 90*time.Second, "the armed instance commits a head and is Ready", func() bool {
		if err := env.admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: "inventory"}, &current); err != nil {
			return false
		}
		c := meta.FindStatusCondition(current.Status.Conditions, "Ready")
		return store.HasFinalizer(&current, store.IntegrationCleanupFinalizer) && sequence() >= 1 && c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == current.Generation
	})
	// sync sends a reviewed observe sync with the current revision and
	// retries only a conflict: the job's own status writes move the
	// revision, and a conflict made no effect.
	sync := func(token string) (int, map[string]any) {
		t.Helper()
		for attempt := 0; ; attempt++ {
			read()
			saved := env.token
			env.token = token
			status, body := env.api(t, cfg, http.MethodPost, "/integrations/default/inventory/sync", map[string]any{"uid": string(current.UID), "revision": current.ResourceVersion, "mode": "observe"})
			env.token = saved
			if status != http.StatusConflict || attempt == 20 {
				return status, body
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	// The CLI on the Kubernetes store, and a viewer refused the sync.
	cliConfig := filepath.Join(t.TempDir(), "contexts.yaml")
	run := func(args ...string) (string, error) {
		o := newCLIOpts()
		o.getenv = func(string) string { return "" }
		root := newRootWith(o)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(append([]string{"--cli-config", cliConfig, "--server", "http://" + cfg.Listeners.API.Address, "--token", env.token}, args...))
		err := root.ExecuteContext(ctx)
		return out.String(), err
	}
	out, err := run("integrations", "list")
	if err != nil || !strings.Contains(out, "inventory") || !strings.Contains(out, "unifi") || !strings.Contains(out, public.IntegrationDeletionDelete) {
		t.Fatalf("CLI list: %v\n%s", err, out)
	}
	// reviewed runs one reviewed CLI command against the current revision,
	// retrying only a conflict (the job's status writes move the revision).
	reviewed := func(command string, extra ...string) (string, error) {
		t.Helper()
		for attempt := 0; ; attempt++ {
			read()
			out, err := run(append([]string{"-o", "json", "integrations", command, "inventory", "--uid", string(current.UID), "--revision", current.ResourceVersion}, extra...)...)
			if err == nil || attempt == 20 || !strings.Contains(err.Error(), "conflict") {
				return out, err
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	out, err = reviewed("check")
	var checked map[string]any
	if err != nil || json.Unmarshal([]byte(out), &checked) != nil || checked["reachable"] != true || checked["authenticated"] != true {
		t.Fatalf("CLI check: %v\n%s", err, out)
	}
	out, err = reviewed("actions")
	if err != nil || json.Unmarshal([]byte(out), &checked) != nil || checked["completed"] != true {
		t.Fatalf("CLI actions: %v\n%s", err, out)
	}
	before := sequence()
	var synced map[string]any
	out, err = reviewed("sync", "--mode", "observe")
	if err != nil || json.Unmarshal([]byte(out), &synced) != nil || synced["completed"] != true || synced["sourceCommitted"] != true || sequence() != before+1 {
		t.Fatalf("CLI sync: %v\n%s (sequence %d after %d)", err, out, sequence(), before)
	}
	status, body := env.api(t, cfg, http.MethodPost, "/auth/tokens", map[string]any{"description": "viewer", "expiresIn": "1h", "scopes": []string{"viewer"}})
	viewer, _ := body["token"].(string)
	if status != http.StatusCreated || viewer == "" {
		t.Fatal("minting a viewer token", status, body)
	}
	before = sequence()
	if status, body := sync(viewer); status != http.StatusForbidden {
		t.Fatal("a viewer ran a sync", status, body)
	}
	if sequence() != before {
		t.Fatal("a refused viewer sync committed")
	}

	// The host's own status write keeps a review current (D384): once the
	// host has read its write back, a check reviewed one status write
	// earlier runs; a label change makes every earlier review stale.
	summary := func() (string, bool) {
		t.Helper()
		status, body := env.api(t, cfg, http.MethodGet, "/integrations/default/inventory", nil)
		revision, _ := body["revision"].(string)
		known, _ := body["statusKnown"].(bool)
		return revision, status == http.StatusOK && known
	}
	var reviewedRevision string
	kubeAwait(t, 30*time.Second, "the summary knows the status of the stored revision", func() bool {
		read()
		revision, known := summary()
		reviewedRevision = revision
		return known && revision == current.ResourceVersion
	})
	if status, body := sync(env.token); status != http.StatusOK {
		t.Fatal("observe sync", status, body)
	}
	// check sends a reviewed check at revision. A status write that lands
	// during the call is the window docs/deploy/integrations.md names; only
	// then is the check sent again, once the host has read the write back.
	check := func(revision string) (int, map[string]any) {
		t.Helper()
		for attempt := 0; ; attempt++ {
			kubeAwait(t, 30*time.Second, "the host reads the stored revision back", func() bool {
				read()
				r, known := summary()
				return known && r == current.ResourceVersion
			})
			stored := current.ResourceVersion
			status, body := env.api(t, cfg, http.MethodPost, "/integrations/default/inventory/check", map[string]any{"uid": string(current.UID), "revision": revision})
			read()
			if status != http.StatusConflict || current.ResourceVersion == stored || attempt == 5 {
				return status, body
			}
		}
	}
	read()
	if current.ResourceVersion == reviewedRevision {
		t.Fatal("the sync wrote no status")
	}
	if status, body := check(reviewedRevision); status != http.StatusOK || body["reachable"] != true {
		t.Fatalf("check reviewed before the host's status write at %s: %d %v", current.ResourceVersion, status, body)
	}
	read()
	statusRevision := current.ResourceVersion
	current.Labels = map[string]string{"team": "dns"}
	if err := env.admin.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	for _, revision := range []string{reviewedRevision, statusRevision} {
		if status, body := check(revision); status != http.StatusConflict {
			t.Fatalf("check reviewed before a label change at %s: %d %v", revision, status, body)
		}
	}

	// A head write that reached the API server but whose answer was lost
	// is resolved by one read of the head: committed, exactly once.
	heads := carriers(kinds.KindIntegrationSourceHead)
	if len(heads) != 1 {
		t.Fatal("heads", len(heads))
	}
	headWrite := func(r *http.Request) bool {
		return r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/secrets/"+heads[0].Name)
	}
	before = sequence()
	gate.loseAnswer(headWrite, true)
	status, body = sync(env.token)
	if gate.faultPending() {
		t.Fatal("the sync never wrote the head", status, body)
	}
	if status != http.StatusOK || body["completed"] != true || body["sourceCommitted"] != true || body["uncertain"] != false || sequence() != before+1 {
		t.Fatal("a landed head write with a lost answer was not resolved as committed", status, body, before, sequence())
	}

	// A head write that never arrived is uncertain, not a refusal, and is
	// not replayed.
	before = sequence()
	gate.loseAnswer(headWrite, false)
	status, body = sync(env.token)
	if gate.faultPending() {
		t.Fatal("the sync never wrote the head", status, body)
	}
	problem, _ := body["error"].(map[string]any)
	hint, _ := problem["hint"].(map[string]any)
	if status != http.StatusServiceUnavailable || problem["key"] != "integration_uncertain" || hint["uncertain"] != true || hint["sourceCommitted"] != false || hint["pending"] != float64(1) {
		t.Fatal("a head write with an unknown outcome was not reported uncertain", status, body)
	}
	time.Sleep(3 * time.Second)
	if sequence() != before {
		t.Fatal("the uncertain head write was replayed or landed", before, sequence())
	}

	// A suspended instance runs no job: a command names no holder and says
	// why.
	read()
	current.Spec.Suspend = true
	if err := env.admin.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	kubeAwait(t, 30*time.Second, "a command to the suspended instance says why it does not run", func() bool {
		read()
		status, body := env.api(t, cfg, http.MethodPost, "/integrations/default/inventory/sync", map[string]any{"uid": string(current.UID), "revision": current.ResourceVersion, "mode": "observe"})
		problem, _ := body["error"].(map[string]any)
		hint, _ := problem["hint"].(map[string]any)
		if status == http.StatusServiceUnavailable && hint["holder"] != nil {
			t.Fatal("a suspended instance's command named a holder", body)
		}
		return status == http.StatusServiceUnavailable && hint["reason"] == "Suspended"
	})

	// Armed, then refused: the instance gains spec.unifi.inventory. It keeps
	// the finalizer and its head; deleting it purges both and removes it.
	read()
	current.Spec.Suspend = false
	current.Spec.UniFi.Inventory = &public.UniFiInventory{ZoneRef: public.ObjectRef{Name: "home"}}
	if err := env.admin.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	kubeAwait(t, 60*time.Second, "the instance reports its refusal", func() bool {
		read()
		c := meta.FindStatusCondition(current.Status.Conditions, "Ready")
		return c != nil && c.Reason == reasonRecordsUnsupported && c.ObservedGeneration == current.Generation
	})
	if !store.HasFinalizer(&current, store.IntegrationCleanupFinalizer) || len(carriers(kinds.KindIntegrationSourceHead)) != 1 {
		t.Fatal("the refused instance lost its finalizer or its head")
	}
	if err := env.admin.Delete(ctx, &current); err != nil {
		t.Fatal(err)
	}
	kubeAwait(t, 60*time.Second, "the refused armed instance purges its source and is removed", func() bool {
		err := env.admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: "inventory"}, &public.Integration{})
		return err != nil && strings.Contains(err.Error(), "not found") && len(carriers(kinds.KindIntegrationSourceHead)) == 0
	})
	if chunks := carriers(kinds.KindIntegrationSourceChunk); len(chunks) != 0 {
		t.Fatal("the purge left source chunks", len(chunks))
	}

	// Armed, then refused for a UniFi push this root does not run (D384).
	// clientActions makes no instance a publisher: deleting it purges its
	// carriers and removes it with no controller request. The record mirror
	// does: that cleanup needs the provider, so the object stays Terminating
	// until the switch is off.
	armedThenRefused := func(push func(*public.UniFiPush)) {
		t.Helper()
		again := &public.Integration{ObjectMeta: meta0("inventory"), Spec: *inventory.Spec.DeepCopy()}
		if err := env.admin.Create(ctx, again); err != nil {
			t.Fatal(err)
		}
		kubeAwait(t, 90*time.Second, "the recreated instance commits a head and is Ready", func() bool {
			if err := env.admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: "inventory"}, &current); err != nil {
				return false
			}
			c := meta.FindStatusCondition(current.Status.Conditions, "Ready")
			return current.UID == again.UID && store.HasFinalizer(&current, store.IntegrationCleanupFinalizer) && sequence() >= 1 && c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == current.Generation
		})
		push(&current.Spec.UniFi.Push)
		if err := env.admin.Update(ctx, &current); err != nil {
			t.Fatal(err)
		}
		kubeAwait(t, 60*time.Second, "the instance reports its push refusal", func() bool {
			read()
			c := meta.FindStatusCondition(current.Status.Conditions, "Ready")
			return c != nil && c.Reason == reasonPushUnsupported && c.ObservedGeneration == current.Generation && c.Message == kubeRefusalMessage(reasonPushUnsupported, "")
		})
		if !store.HasFinalizer(&current, store.IntegrationCleanupFinalizer) || len(carriers(kinds.KindIntegrationSourceHead)) != 1 {
			t.Fatal("the refused instance lost its finalizer or its head")
		}
	}
	gone := func() bool {
		err := env.admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: "inventory"}, &public.Integration{})
		return err != nil && strings.Contains(err.Error(), "not found") && len(carriers(kinds.KindIntegrationSourceHead)) == 0
	}
	armedThenRefused(func(p *public.UniFiPush) { p.ClientActions = true })
	requests := controllerCalls.Load()
	if err := env.admin.Delete(ctx, &current); err != nil {
		t.Fatal(err)
	}
	kubeAwait(t, 60*time.Second, "the instance refused for clientActions purges its source and is removed", gone)
	if got := controllerCalls.Load(); got != requests {
		t.Fatal("the cleanup of a refused non-publisher reached the controller", got-requests)
	}

	armedThenRefused(func(p *public.UniFiPush) {
		p.MirrorRecords.Enabled, p.MirrorRecords.Domains = true, []string{"example"}
	})
	if err := env.admin.Delete(ctx, &current); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Second)
	read()
	if current.DeletionTimestamp.IsZero() || !store.HasFinalizer(&current, store.IntegrationCleanupFinalizer) || len(carriers(kinds.KindIntegrationSourceHead)) != 1 {
		t.Fatal("the cleanup of a refused record mirror ran without its provider")
	}
	current.Spec.UniFi.Push.MirrorRecords.Enabled = false
	if err := env.admin.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	kubeAwait(t, 60*time.Second, "the instance whose mirror was turned off purges its source and is removed", gone)
}
