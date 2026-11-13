package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

// kubeUniFiController serves the official UniFi wire contract with one wired
// client at a loopback address, so DNS queries sent from that address are
// identified through the inventory. present toggles the client.
func kubeUniFiController(t *testing.T, ip string) (*httptest.Server, *atomic.Int32, *atomic.Bool) {
	t.Helper()
	var calls atomic.Int32
	var present atomic.Bool
	present.Store(true)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		page := func(rows []map[string]any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"offset": 0, "limit": 200, "count": len(rows), "totalCount": len(rows), "data": rows})
		}
		base := "/proxy/network/integration/v1"
		site := base + "/sites/11111111-1111-4111-8111-111111111111"
		switch r.URL.Path {
		case base + "/info":
			_ = json.NewEncoder(w).Encode(map[string]any{"applicationVersion": "10.6.106"})
		case base + "/sites":
			page([]map[string]any{{"id": "11111111-1111-4111-8111-111111111111", "internalReference": "default"}})
		case site + "/clients":
			if !present.Load() {
				page([]map[string]any{})
				return
			}
			page([]map[string]any{{"id": "33333333-3333-4333-8333-333333333333", "name": "Laptop", "type": "WIRED", "ipAddress": ip, "macAddress": "00:11:22:33:44:55", "connectedAt": time.Now().Add(-time.Hour)}})
		case site + "/networks":
			page([]map[string]any{{"id": "22222222-2222-4222-8222-222222222222", "name": "LAN", "management": "GATEWAY", "enabled": true, "vlanId": 10}})
		case site + "/networks/22222222-2222-4222-8222-222222222222":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "22222222-2222-4222-8222-222222222222", "name": "LAN", "management": "GATEWAY", "enabled": true, "vlanId": 10, "ipv4Configuration": map[string]any{"hostIpAddress": "127.0.0.1", "prefixLength": 8}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, &calls, &present
}

// kubeIntegrationEnv is one envtest API server with an admin client and the
// cluster identity, and a license that allows integrations.
type kubeIntegrationEnv struct {
	rest     *rest.Config
	admin    client.Client
	identity string
	token    string
}

func newKubeIntegrationEnv(t *testing.T) *kubeIntegrationEnv {
	t.Helper()
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
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, coordinationv1.AddToScheme, public.AddToScheme} {
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
	wire, _ := tokens.Generate()
	t.Setenv(tokens.EnvBootstrapToken, wire)
	t.Setenv(tokens.EnvBootstrapTokenFile, "")
	return &kubeIntegrationEnv{rest: restConfig, admin: admin, identity: string(identity.UID), token: wire}
}

// replica builds one replica's configuration. offlineServeConfig sets the
// process-global KUBECONFIG: start each replica before building the next.
func (e *kubeIntegrationEnv) replica(t *testing.T, name string, gate *partitionGate, credential string) *config.Config {
	t.Helper()
	cfg := offlineServeConfig(t, gate.server.URL)
	cfg.Node.Name = name
	cfg.Store.Kube.ExpectedClusterID = e.identity
	cfg.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	cfg.Credentials.AllowedFiles = []string{credential}
	if os.Getenv("KUDURRU_TEST_KUBE_LOG") != "" {
		cfg.Log.Level = os.Getenv("KUDURRU_TEST_KUBE_LOG")
	}
	return cfg
}

// api sends one API request with the bootstrap token and decodes the answer.
func (e *kubeIntegrationEnv) api(t *testing.T, cfg *config.Config, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+cfg.Listeners.API.Address+"/api/v1"+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := (&http.Client{Timeout: 40 * time.Second}).Do(req)
	if err != nil {
		return 0, nil
	}
	defer func() { _ = res.Body.Close() }()
	out := map[string]any{}
	_ = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&out)
	return res.StatusCode, out
}

func kubeTestCredential(t *testing.T) string {
	t.Helper()
	credential := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(credential, []byte("test-only-private-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	return credential
}

// kubeTestKubeconfig points the process-global KUBECONFIG at url, as
// offlineServeConfig does, so a replica can be started again.
func kubeTestKubeconfig(t *testing.T, url string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	body := "apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: " + url + "\ncontexts:\n- name: test\n  context:\n    cluster: test\n    user: test\ncurrent-context: test\nusers:\n- name: test\n  user: {}\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
}

func kubeAwait(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("never converged:", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// kubeRewritten reports whether a query for name sent from source is
// answered with the policy rewrite target.
func kubeRewritten(ctx context.Context, cfg *config.Config, source, name, target string) bool {
	dialer := &net.Dialer{Timeout: time.Second, LocalAddr: &net.UDPAddr{IP: net.ParseIP(source)}}
	raw, err := dialer.DialContext(ctx, "udp", cfg.Listeners.DNS[0].Address)
	if err != nil {
		return false
	}
	defer func() { _ = raw.Close() }()
	q := new(dns.Msg)
	q.SetQuestion(name, dns.TypeA)
	a, _, err := (&dns.Client{Net: "udp", ReadTimeout: 6 * time.Second, WriteTimeout: time.Second}).ExchangeWithConnContext(ctx, q, &dns.Conn{Conn: raw})
	if err != nil || a == nil || a.Rcode != dns.RcodeSuccess || len(a.Answer) != 1 {
		return false
	}
	rr, ok := a.Answer[0].(*dns.A)
	return ok && rr.A.String() == target
}

// TestRunServeKubeUniFiSourcesTwoReplicasAndPartition mounts two replicas
// on envtest with a UniFi HTTPS double (F09b-K4, K5, K6, K7, D384). It shows:
// only the Lease holder polls and commits one IntegrationSourceHead with
// strictly increasing sequences; BOTH replicas identify the client by MAC
// from the committed head, both through a manual MAC group and through the
// MAC grant into the group the Integration contributes to; suspending the
// Integration closes both on both replicas without a new commit, and
// resuming reopens them; the administration API answers list and get the
// same on both replicas from the durable status, refuses a stale revision,
// sends a command to the non-holder back with the holder's name, and runs a
// reviewed observe sync on the holder; at a two-second poll the non-holder's
// grant never lapses across successive holder commits (the heads are
// watched); a restarted replica grants again from the committed head with no
// new commit and no holder change; partitioning the holder from the API
// server hands the Lease to the other replica, which alone commits, while
// the partitioned replica keeps answering from its last committed source
// without a gap until shortly before that observation's original expiry,
// then stops at it, and never renews it; after
// healing it converges on the successor's head; and deletionPolicy Delete
// arms the cleanup finalizer, so deleting the Integration purges its source
// carriers and closes its grants on both replicas before the object goes.
func TestRunServeKubeUniFiSourcesTwoReplicasAndPartition(t *testing.T) {
	env := newKubeIntegrationEnv(t)
	ctx := t.Context()
	server, calls, _ := kubeUniFiController(t, "127.0.0.7")
	credential := kubeTestCredential(t)
	meta0 := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "default"} }
	for _, object := range []client.Object{
		&public.ClientGroup{ObjectMeta: meta0("devices"), Spec: public.ClientGroupSpec{Sources: &public.ClientGroupSources{IntegrationRef: &public.ObjectRef{Name: "inventory"}}}},
		&public.Policy{ObjectMeta: meta0("devices"), Spec: public.PolicySpec{ClientGroupRefs: []string{"devices"}, Priority: 1000, Rewrites: []public.Rewrite{{Name: "granted.test", Targets: []string{"192.0.2.99"}}}}},
		&public.ClientGroup{ObjectMeta: meta0("manual"), Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{MAC: "00:11:22:33:44:55"}}}},
		&public.Policy{ObjectMeta: meta0("manual"), Spec: public.PolicySpec{ClientGroupRefs: []string{"manual"}, Priority: 900, Rewrites: []public.Rewrite{{Name: "manual.test", Targets: []string{"192.0.2.98"}}}}},
	} {
		if err := env.admin.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	gateA, gateB := newPartitionGate(t, env.rest), newPartitionGate(t, env.rest)
	cfgA := env.replica(t, "sources-a", gateA, credential)
	stops := map[string]func(){"sources-a": kubeIntegrationMount(t, cfgA)}
	// The bootstrap token's service account is created by the first
	// replica; start the second once the first answers.
	kubeAwait(t, 60*time.Second, "replica A answers the API", func() bool {
		status, _ := env.api(t, cfgA, http.MethodGet, "/integrations?namespace=default", nil)
		return status == http.StatusOK
	})
	cfgB := env.replica(t, "sources-b", gateB, credential)
	stops["sources-b"] = kubeIntegrationMount(t, cfgB)
	byName := map[string]*config.Config{"sources-a": cfgA, "sources-b": cfgB}
	gates := map[string]*partitionGate{"sources-a": gateA, "sources-b": gateB}

	sum := sha256.Sum256(server.Certificate().Raw)
	inventory := &public.Integration{ObjectMeta: meta0("inventory"), Spec: public.IntegrationSpec{Type: "unifi", Resync: metav1.Duration{Duration: time.Hour}, Credentials: map[string]public.CredentialReference{"apiKey": {File: credential}}, UniFi: &public.UniFiIntegration{Endpoint: server.URL, TLSFingerprint: "sha256:" + hex.EncodeToString(sum[:]), Site: "default", Poll: metav1.Duration{Duration: 5 * time.Second}, Groups: public.UniFiGroups{ByNetwork: map[string]string{"LAN": "devices"}}}}}
	if err := env.admin.Create(ctx, inventory); err != nil {
		t.Fatal(err)
	}
	leaseName := "kudurru-" + ih.ElectionKey(ih.InstanceID{Namespace: "default", Name: "inventory"})
	holder := func() string {
		var lease coordinationv1.Lease
		if err := env.admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: leaseName}, &lease); err != nil || lease.Spec.HolderIdentity == nil {
			return ""
		}
		return *lease.Spec.HolderIdentity
	}
	heads := func() []kinds.IntegrationSourceHead {
		var list corev1.SecretList
		if err := env.admin.List(ctx, &list, client.InNamespace("default"), client.MatchingLabels{kube.InternalKindLabel: kinds.KindIntegrationSourceHead}); err != nil {
			t.Fatal(err)
		}
		var out []kinds.IntegrationSourceHead
		for _, sec := range list.Items {
			var head kinds.IntegrationSourceHead
			if err := json.Unmarshal(sec.Data[kube.InternalObjectKey], &head); err != nil {
				t.Fatal(err)
			}
			out = append(out, head)
		}
		return out
	}
	sequence := func() uint64 {
		h := heads()
		if len(h) != 1 {
			return 0
		}
		return h[0].Spec.Sequence
	}
	granted := func(cfg *config.Config) bool {
		return kubeRewritten(ctx, cfg, "127.0.0.7", "granted.test.", "192.0.2.99")
	}
	manual := func(cfg *config.Config) bool {
		return kubeRewritten(ctx, cfg, "127.0.0.7", "manual.test.", "192.0.2.98")
	}
	kubeAwait(t, 90*time.Second, "one replica holds the instance's Lease and commits a head", func() bool { return holder() != "" && sequence() >= 1 })
	first := holder()
	other := "sources-a"
	if first == "sources-a" {
		other = "sources-b"
	}
	for name, cfg := range byName {
		kubeAwait(t, 60*time.Second, name+" grants the contributed group and identifies the MAC", func() bool { return granted(cfg) && manual(cfg) })
	}
	// A client at another address is not identified on either replica.
	for name, cfg := range byName {
		if kubeRewritten(ctx, cfg, "127.0.0.8", "granted.test.", "192.0.2.99") || kubeRewritten(ctx, cfg, "127.0.0.8", "manual.test.", "192.0.2.98") {
			t.Fatal(name, "granted an unknown client")
		}
	}
	if got := heads(); len(got) != 1 || got[0].Spec.Owner.Name != "inventory" {
		t.Fatal("not exactly one head for the Integration UID", len(got))
	}

	// Administration (K6): list and get answer on both replicas from the
	// durable status; a command on the non-holder names the holder; a stale
	// revision is a conflict; the holder runs a reviewed observe sync.
	var current public.Integration
	kubeAwait(t, 60*time.Second, "the holder writes Ready status", func() bool {
		if err := env.admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: "inventory"}, &current); err != nil {
			return false
		}
		c := meta.FindStatusCondition(current.Status.Conditions, "Ready")
		return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == current.Generation
	})
	sources := map[string]string{}
	for name, cfg := range byName {
		kubeAwait(t, 30*time.Second, name+" lists the instance as Ready", func() bool {
			status, body := env.api(t, cfg, http.MethodGet, "/integrations?namespace=default", nil)
			items, _ := body["items"].([]any)
			if status != http.StatusOK || len(items) != 1 {
				return false
			}
			item, _ := items[0].(map[string]any)
			return item["phase"] == "Ready" && item["statusKnown"] == true && item["leader"] == (name == holder())
		})
		status, body := env.api(t, cfg, http.MethodGet, "/integrations/default/inventory", nil)
		if status != http.StatusOK || body["uid"] != string(current.UID) || body["type"] != "unifi" {
			t.Fatal(name, "get", status, body)
		}
		// Every replica names the same inventory source for the incarnation.
		if source, _ := body["inventorySource"].(string); !strings.HasPrefix(source, "integration-") || sources[source] == "" && len(sources) > 0 {
			t.Fatal(name, "inventorySource", body["inventorySource"], sources)
		} else {
			sources[source] = name
		}
	}
	if err := env.admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: "inventory"}, &current); err != nil {
		t.Fatal(err)
	}
	review := map[string]any{"uid": string(current.UID), "revision": current.ResourceVersion, "mode": "observe"}
	status, body := env.api(t, byName[other], http.MethodPost, "/integrations/default/inventory/sync", review)
	problem, _ := body["error"].(map[string]any)
	hint, _ := problem["hint"].(map[string]any)
	if status != http.StatusServiceUnavailable || hint["holder"] != first {
		t.Fatal("the non-holder did not name the holder", status, body)
	}
	status, _ = env.api(t, byName[first], http.MethodPost, "/integrations/default/inventory/sync", map[string]any{"uid": string(current.UID), "revision": "1", "mode": "observe"})
	if status != http.StatusConflict {
		t.Fatal("a stale revision was not a conflict", status)
	}
	before := sequence()
	ok := false
	for attempt := 0; attempt < 20 && !ok; attempt++ {
		// The holder's own status writes move the revision; review the
		// current one and retry only a conflict, which made no effect.
		if err := env.admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: "inventory"}, &current); err != nil {
			t.Fatal(err)
		}
		status, body = env.api(t, byName[first], http.MethodPost, "/integrations/default/inventory/sync", map[string]any{"uid": string(current.UID), "revision": current.ResourceVersion, "mode": "observe"})
		switch status {
		case http.StatusOK:
			ok = true
		case http.StatusConflict:
			time.Sleep(200 * time.Millisecond)
		default:
			t.Fatal("reviewed sync on the holder", status, body)
		}
	}
	if !ok || body["completed"] != true || body["sourceCommitted"] != true || body["sourcePublished"] != true || body["clients"] != float64(1) {
		t.Fatal("reviewed observe sync", status, body)
	}
	if sequence() <= before {
		t.Fatal("the reviewed sync did not commit a successor head", before, sequence())
	}

	// setPoll changes the poll interval, and so the spec digest every head
	// carries, and waits until both replicas grant from a head committed
	// under the new spec.
	setPoll := func(poll time.Duration) {
		t.Helper()
		for attempt := 0; ; attempt++ {
			if err := env.admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: "inventory"}, &current); err != nil {
				t.Fatal(err)
			}
			current.Spec.UniFi.Poll = metav1.Duration{Duration: poll}
			err := env.admin.Update(ctx, &current)
			if err == nil {
				break
			}
			if attempt == 20 {
				t.Fatal(err)
			}
			time.Sleep(200 * time.Millisecond)
		}
		raw, err := json.Marshal(current.Spec)
		if err != nil {
			t.Fatal(err)
		}
		digest := kinds.Digest(raw)
		kubeAwait(t, 60*time.Second, "a head committed under the new spec", func() bool {
			h := heads()
			return len(h) == 1 && h[0].Spec.ConfigDigest == digest
		})
		for name, cfg := range byName {
			kubeAwait(t, 60*time.Second, name+" grants under the new spec", func() bool { return granted(cfg) && manual(cfg) })
		}
	}

	// Continuity (K5): at a two-second poll an observation expires after
	// six (five to six, truncated to the second). The replica that does not
	// hold the Lease must install every successor head before its
	// predecessor expires, so its grant never lapses across successive
	// holder commits. A relist on a ten-second timer alone cannot do that.
	setPoll(2 * time.Second)
	holderNow := holder()
	nonHolder := "sources-a"
	if holderNow == "sources-a" {
		nonHolder = "sources-b"
	}
	from := sequence()
	for until := time.Now().Add(12 * time.Second); time.Now().Before(until); time.Sleep(100 * time.Millisecond) {
		if !granted(byName[nonHolder]) {
			t.Fatal("the non-holder's grant lapsed between holder commits", sequence()-from, "commits in; the holder grants:", granted(byName[holderNow]))
		}
	}
	if commits := sequence() - from; commits < 3 {
		t.Fatal("the holder committed too few heads to show continuity", commits)
	}

	// Restart (K5, K7): with an hour's poll no head is committed while a
	// replica restarts. The restarted process rebuilds the same grants from
	// the committed head alone, before any new commit.
	setPoll(time.Hour)
	holderNow = holder()
	restarted := "sources-a"
	if holderNow == "sources-a" {
		restarted = "sources-b"
	}
	atRestart := sequence()
	stops[restarted]()
	if granted(byName[restarted]) {
		t.Fatal("a stopped replica answered")
	}
	kubeTestKubeconfig(t, gates[restarted].server.URL)
	stops[restarted] = kubeIntegrationMount(t, byName[restarted])
	kubeAwait(t, 60*time.Second, "the restarted replica grants from the committed head", func() bool { return granted(byName[restarted]) && manual(byName[restarted]) })
	if sequence() != atRestart || holder() != holderNow {
		t.Fatal("the restart was served by a new commit or a holder change, not from the committed head", atRestart, sequence(), holderNow, holder())
	}
	setPoll(5 * time.Second)

	// Suspension closes the source on both replicas without a new commit.
	if err := env.admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: "inventory"}, &current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Suspend = true
	if err := env.admin.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	for name, cfg := range byName {
		kubeAwait(t, 30*time.Second, name+" closes the suspended source", func() bool { return !granted(cfg) && !manual(cfg) })
	}
	suspendedAt := sequence()
	time.Sleep(6 * time.Second)
	if sequence() != suspendedAt {
		t.Fatal("a suspended instance committed", suspendedAt, sequence())
	}
	if err := env.admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: "inventory"}, &current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Suspend = false
	if err := env.admin.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	for name, cfg := range byName {
		kubeAwait(t, 60*time.Second, name+" reopens the resumed source", func() bool { return granted(cfg) && manual(cfg) })
	}

	// Partition the holder. The other replica acquires the instance's Lease
	// without intervention and alone commits; the partitioned one keeps
	// its last committed source until its original expiry and never renews.
	// Suspension released the Lease; partition whichever replica holds it now.
	kubeAwait(t, 60*time.Second, "a replica holds the resumed instance's Lease", func() bool { return holder() != "" })
	first = holder()
	other = "sources-a"
	if first == "sources-a" {
		other = "sources-b"
	}
	pollsBefore := calls.Load()
	gates[first].partition(true)
	partitionedAt := time.Now()
	// The last head's observation expires three polls (fifteen seconds,
	// truncated to the second) after it was made, and the head's AcceptedAt
	// is stamped right after the observation. Until twelve seconds after
	// AcceptedAt the partitioned replica must answer without a gap.
	last := heads()
	if len(last) != 1 {
		t.Fatal("heads at the partition", len(last))
	}
	until := last[0].Spec.AcceptedAt.Add(12 * time.Second)
	t.Log("the partitioned replica must answer without a gap for", time.Until(until).Round(100*time.Millisecond))
	for time.Now().Before(until) {
		if !granted(byName[first]) {
			t.Fatal("the partitioned replica stopped answering from its last committed source before its expiry", time.Since(partitionedAt), time.Until(until))
		}
		time.Sleep(200 * time.Millisecond)
	}
	kubeAwait(t, 90*time.Second, "the other replica acquires the instance's Lease", func() bool { return holder() == other })
	takeover := sequence()
	kubeAwait(t, 60*time.Second, "the successor commits", func() bool { return sequence() > takeover })
	if calls.Load() == pollsBefore {
		t.Fatal("the successor never polled the provider")
	}
	kubeAwait(t, 30*time.Second, "the successor grants its own committed source", func() bool { return granted(byName[other]) })
	// Five-second polls expire an observation after fifteen seconds.
	kubeAwait(t, 40*time.Second, "the partitioned replica's last observation expires", func() bool { return !granted(byName[first]) && !manual(byName[first]) })
	gates[first].partition(false)
	kubeAwait(t, 60*time.Second, "the healed replica converges on the successor's head", func() bool { return granted(byName[first]) && manual(byName[first]) })
	if holder() != other {
		t.Fatal("the healed former holder reclaimed the Lease", holder())
	}
	healed := sequence()
	kubeAwait(t, 30*time.Second, "sequences keep increasing", func() bool { return sequence() > healed })
	if got := heads(); len(got) != 1 {
		t.Fatal("partition and heal left more than one head", len(got))
	}

	// deletionPolicy Delete: the root arms the cleanup finalizer; deleting
	// the Integration purges its source carriers and grants on both
	// replicas, then removes the object.
	if err := env.admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: "inventory"}, &current); err != nil {
		t.Fatal(err)
	}
	current.Spec.DeletionPolicy = public.IntegrationDeletionDelete
	if err := env.admin.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	kubeAwait(t, 30*time.Second, "the cleanup finalizer is armed", func() bool {
		if err := env.admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: "inventory"}, &current); err != nil {
			return false
		}
		return store.HasFinalizer(&current, store.IntegrationCleanupFinalizer)
	})
	armed := sequence()
	kubeAwait(t, 60*time.Second, "the armed instance commits again", func() bool { return sequence() > armed })
	if err := env.admin.Delete(ctx, &current); err != nil {
		t.Fatal(err)
	}
	kubeAwait(t, 90*time.Second, "the cleanup purges the source and removes the object", func() bool {
		err := env.admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: "inventory"}, &current)
		return err != nil && strings.Contains(err.Error(), "not found") && len(heads()) == 0
	})
	var chunks corev1.SecretList
	if err := env.admin.List(ctx, &chunks, client.InNamespace("default"), client.MatchingLabels{kube.InternalKindLabel: kinds.KindIntegrationSourceChunk}); err != nil {
		t.Fatal(err)
	}
	if len(chunks.Items) != 0 {
		t.Fatal("the purge left source chunks", len(chunks.Items))
	}
	for name, cfg := range byName {
		kubeAwait(t, 30*time.Second, name+" closes the deleted source", func() bool { return !granted(cfg) && !manual(cfg) })
	}
	if err := env.admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: "manual"}, &public.ClientGroup{}); err != nil {
		t.Fatal("the manual group was touched", err)
	}
}
