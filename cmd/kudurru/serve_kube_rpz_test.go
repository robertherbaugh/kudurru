package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
)

// RPZ feeds on the Kubernetes driver (D390): an RPZ BlockList source signed
// with a stored TSIG key is transferred on every replica and enforced over UDP
// and TCP for QNAME, CLIENT-IP and RESPONSE-IP triggers, with blocked_rpz in
// the query log. A feed update is applied, a required unavailable feed fails
// closed, enforcement survives a leader change and an apiserver outage, a
// restart reuses the unexpired copy and the expired copy fails closed. A
// restart while the apiserver is unreachable fails the signed feed closed,
// because its stored key cannot be read, and reuses an unsigned feed's copy.
// From before each restarted process answers, the listed name never resolves
// NOERROR: a referenced RPZ list is required before the list resolver
// compiles it, and with blocking.allowDegraded: false readiness waits for it.
func TestRunServeKubeRPZFeeds(t *testing.T) {
	control := &envtest.Environment{BinaryAssetsDirectory: mountedKubeTestAssets(t), CRDDirectoryPaths: []string{"../../deploy/kustomize/crd/bases"}, ErrorIfCRDPathMissing: true}
	control.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1").Set("endpoint-reconciler-type", "none")
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
	admin, err := client.New(actual, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	var namespace corev1.Namespace
	if err := admin.Get(t.Context(), client.ObjectKey{Name: "kube-system"}, &namespace); err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(actual.Host)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	if proxy.Transport, err = rest.TransportFor(actual); err != nil {
		t.Fatal(err)
	}
	// The gate refuses every new apiserver request while closed. Streams that
	// were already open keep running; DNS answers must need neither.
	var gateClosed atomic.Bool
	var gateRefused atomic.Int64
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gateClosed.Load() {
			gateRefused.Add(1)
			http.Error(w, "apiserver unavailable", http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(gate.Close)

	// Provision the stored transfer key before startup (D314/D365).
	st, err := kube.New(actual, kube.Options{Scheme: scheme, InternalKinds: privateStoreKinds(), ExpectedClusterID: string(namespace.UID), StartupTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sealer, err := tsigkeys.NewSealer(make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := tsigkeys.Read(t.Context(), st, string(namespace.UID))
	if err != nil {
		t.Fatal(err)
	}
	empty, err := dnsserver.NewStaticKeyring()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := source.Prepare(sealer, empty)
	if err != nil {
		t.Fatal(err)
	}
	secret := bytes.Repeat([]byte{0x52}, 32)
	plan, err := prepared.Plan(sealer, tsigkeys.MutationInput{Operation: tsigkeys.Create, KeyName: "feed.", Algorithm: "hmac-sha256", Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.ApplyStore(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	primary := kubeRPZPrimary(t, dnsserver.TSIGKey{Name: "feed.", Algorithm: dns.HmacSHA256, Secret: secret})
	openPrimary := kubeRPZOpenPrimary(t)
	upstream := kubeRPZUpstream(t)

	nodeConfig := func(name string) *config.Config {
		cfg := offlineServeConfig(t, gate.URL)
		cfg.Node.Name = name
		cfg.Store.Kube.ExpectedClusterID = string(namespace.UID)
		cfg.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
		cfg.Blocking.Enabled = true
		// Readiness waits until every referenced list, RPZ-only ones included,
		// is in the policy. With the default (true) a node answers while lists
		// load and filters with the lists already loaded, RPZ included.
		cfg.Blocking.AllowDegraded = false
		cfg.DNSSEC.Enabled = false
		cfg.Cache.MaxEntries = 0
		cfg.Listeners.DNS[0].Address = kubeRPZDualAddress(t)
		cfg.Listeners.DNS[0].Protocols = []string{"udp", "tcp"}
		cfg.Listeners.Metrics.Address = offlineFreeAddress(t, "tcp")
		cfg.Querylog.Enabled = true
		cfg.Querylog.Sink = "sqlite"
		cfg.Querylog.Privacy = "full"
		cfg.Querylog.Exclude.SUDN = false
		cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{upstream}, Strategy: "strict", Timeout: config.Duration(time.Second)}}
		// The shared-settings refresh reloads the cluster settings from the
		// node file, so the same settings must be there.
		source, err := json.Marshal(map[string]any{
			"store": map[string]any{"driver": "kube"}, "blocking": map[string]any{"enabled": true, "allowDegraded": false}, "dnssec": map[string]any{"enabled": false},
			"cache":     map[string]any{"maxEntries": 0},
			"querylog":  map[string]any{"enabled": true, "sink": "sqlite", "privacy": "full", "exclude": map[string]any{"sudn": false}},
			"upstreams": map[string]any{"groups": map[string]any{"default": map[string]any{"servers": []string{upstream}, "strategy": "strict", "timeout": "1s"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		cfg.Source = filepath.Join(t.TempDir(), "kudurru.json")
		if err := os.WriteFile(cfg.Source, source, 0o600); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	cfgA, cfgB := nodeConfig("rpz-a"), nodeConfig("rpz-b")

	feed := &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "feed", Namespace: "default"}, Spec: public.BlockListSpec{Mode: "block",
		Sources: []public.ListSource{{Name: "native", Format: "rpz", RPZ: &public.RPZSource{ZoneName: "policy.test.", Primaries: []string{primary.address}, TSIGKeyRef: "feed."}}}}}
	pol := &public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}, Spec: public.PolicySpec{BlockListRefs: []string{"feed"}}}
	// An unsigned feed for one client (127.0.0.3) only, so its reuse on an
	// offline restart is observable while the signed feed fails closed.
	open := &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "open", Namespace: "default"}, Spec: public.BlockListSpec{Mode: "block",
		Sources: []public.ListSource{{Name: "native", Format: "rpz", RPZ: &public.RPZSource{ZoneName: "open.test.", Primaries: []string{openPrimary.address}}}}}}
	openGroup := &public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "open", Namespace: "default"}, Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{CIDR: "127.0.0.3/32"}}}}
	openPol := &public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "open", Namespace: "default"}, Spec: public.PolicySpec{ClientGroupRefs: []string{"open"}, BlockListRefs: []string{"open"}}}
	openClient := net.IPv4(127, 0, 0, 3)
	for _, object := range []client.Object{feed, pol, open, openGroup, openPol} {
		if err := admin.Create(t.Context(), object); err != nil {
			t.Fatal(err)
		}
	}

	stopA := startStaticMount(t, cfgA)
	awaitWithin := func(what string, limit time.Duration, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(limit)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatal("never converged:", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	await := func(what string, cond func() bool) {
		t.Helper()
		awaitWithin(what, 45*time.Second, cond)
	}
	await("node A leads", func() bool { return kubeRPZMetric(cfgA, `kudurru_leader{role="controller"} 1`) })
	await("node A enforces the QNAME trigger", func() bool {
		return kubeRPZRcode(cfgA, "blocked.rpzlab.net.", "udp", nil) == dns.RcodeNameError
	})
	stopB := startStaticMount(t, cfgB)
	await("node B enforces the QNAME trigger", func() bool {
		return kubeRPZRcode(cfgB, "blocked.rpzlab.net.", "udp", nil) == dns.RcodeNameError
	})
	await("node B enforces the unsigned feed", func() bool {
		return kubeRPZRcode(cfgB, "unsigned.rpzlab.net.", "udp", openClient) == dns.RcodeNameError && kubeRPZRcode(cfgB, "good.rpzlab.net.", "udp", openClient) == dns.RcodeSuccess
	})
	if !kubeRPZMetric(cfgB, `kudurru_role_active{role="rpz"} 1`) || kubeRPZMetric(cfgB, `kudurru_leader{role="controller"} 1`) {
		t.Fatal("the follower does not run its own RPZ feed")
	}
	matrix := func(cfg *config.Config) {
		t.Helper()
		for _, network := range []string{"udp", "tcp"} {
			if rc := kubeRPZRcode(cfg, "blocked.rpzlab.net.", network, nil); rc != dns.RcodeNameError {
				t.Fatal("QNAME trigger not enforced", cfg.Node.Name, network, dns.RcodeToString[rc])
			}
			if a := kubeRPZExchange(cfg, "good.rpzlab.net.", network, nil); a == nil || a.Rcode != dns.RcodeSuccess || len(a.Answer) != 1 {
				t.Fatal("unlisted name not resolved", cfg.Node.Name, network)
			}
			// RESPONSE-IP: the answer address 192.0.2.66 selects NODATA.
			if a := kubeRPZExchange(cfg, "bad-ip.rpzlab.net.", network, nil); a == nil || a.Rcode != dns.RcodeSuccess || len(a.Answer) != 0 {
				t.Fatal("RESPONSE-IP trigger not enforced", cfg.Node.Name, network)
			}
			// CLIENT-IP: only 127.0.0.2 is listed.
			if rc := kubeRPZRcode(cfg, "client.rpzlab.net.", network, net.IPv4(127, 0, 0, 2)); rc != dns.RcodeNameError {
				t.Fatal("CLIENT-IP trigger not enforced", cfg.Node.Name, network, dns.RcodeToString[rc])
			}
			if a := kubeRPZExchange(cfg, "client.rpzlab.net.", network, nil); a == nil || a.Rcode != dns.RcodeSuccess || len(a.Answer) != 1 {
				t.Fatal("CLIENT-IP trigger matched another client", cfg.Node.Name, network)
			}
		}
		// rpz-tcp-only truncates over UDP and answers over TCP.
		if a := kubeRPZExchange(cfg, "tcp.rpzlab.net.", "udp", nil); a == nil || !a.Truncated || len(a.Answer) != 0 {
			t.Fatal("TCP-only action did not truncate over UDP", cfg.Node.Name)
		}
		if a := kubeRPZExchange(cfg, "tcp.rpzlab.net.", "tcp", nil); a == nil || a.Rcode != dns.RcodeSuccess || len(a.Answer) != 1 {
			t.Fatal("TCP-only action did not answer over TCP", cfg.Node.Name)
		}
	}
	matrix(cfgA)
	matrix(cfgB)
	if primary.signed.Load() == 0 || primary.transfers.Load() == 0 || primary.unsigned.Load() != 0 {
		t.Fatal("feeds were not transferred with the stored TSIG key")
	}

	// A feed update is applied on both replicas.
	if rc := kubeRPZRcode(cfgA, "later.rpzlab.net.", "udp", nil); rc != dns.RcodeSuccess {
		t.Fatal("name blocked before its rule was published", dns.RcodeToString[rc])
	}
	primary.serial.Store(2)
	await("feed update applied on both nodes", func() bool {
		return kubeRPZRcode(cfgA, "later.rpzlab.net.", "udp", nil) == dns.RcodeNameError && kubeRPZRcode(cfgB, "later.rpzlab.net.", "tcp", nil) == dns.RcodeNameError
	})

	// A selected feed that cannot be transferred is required coverage: every
	// recursive query of the policy fails closed until it is deselected.
	missing := &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "missing", Namespace: "default"}, Spec: public.BlockListSpec{Mode: "block",
		Sources: []public.ListSource{{Name: "native", Format: "rpz", RPZ: &public.RPZSource{ZoneName: "missing.test.", Primaries: []string{offlineFreeAddress(t, "tcp")}}}}}}
	if err := admin.Create(t.Context(), missing); err != nil {
		t.Fatal(err)
	}
	setRefs := func(refs ...string) {
		t.Helper()
		var current public.Policy
		if err := admin.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "default"}, &current); err != nil {
			t.Fatal(err)
		}
		current.Spec.BlockListRefs = refs
		if err := admin.Update(t.Context(), &current); err != nil {
			t.Fatal(err)
		}
	}
	setRefs("feed", "missing")
	await("unavailable feed fails closed", func() bool {
		a := kubeRPZExchange(cfgA, "good.rpzlab.net.", "udp", nil)
		return a != nil && a.Rcode == dns.RcodeServerFailure && kubeRPZEDE(a, "RPZ missing.test. unavailable")
	})
	// The feed state reaches the API, the CLI and /metrics on Kubernetes
	// (D391): node A serves the updated feed and reports the missing one.
	cliA := mountedCLIAs(t, cfgA, "rpz-a", "offline-test-password-1")
	await("node A reports the missing feed", func() bool {
		missing := kubeRPZFeeds(t, cliA, "missing")
		return len(missing) == 1 && missing[0].State == "unavailable" && missing[0].Reason == "RPZTransfer" && missing[0].LastError != ""
	})
	if served := kubeRPZFeeds(t, cliA, "feed"); len(served) != 1 || served[0].State != "serving" || served[0].Node != "rpz-a" || served[0].Serial == nil || *served[0].Serial != 2 ||
		served[0].Counts == nil || served[0].Counts.Rules != 5 || served[0].Rules == nil || served[0].Rules.ClientIP != 1 || served[0].Rules.ResponseIP != 1 || served[0].Rules.TCPOnly != 1 {
		t.Fatalf("node A feed = %+v", served)
	}
	if !kubeRPZMetric(cfgA, `kudurru_rpz_feed_available{list="default/missing",source="native"} 0`) || !kubeRPZMetric(cfgA, `kudurru_rpz_feed_serial{list="default/feed",source="native"} 2`) {
		t.Fatal("node A feed metrics missing")
	}
	setRefs("feed")
	await("deselected feed no longer required", func() bool {
		return kubeRPZRcode(cfgA, "good.rpzlab.net.", "udp", nil) == dns.RcodeSuccess && kubeRPZRcode(cfgB, "good.rpzlab.net.", "udp", nil) == dns.RcodeSuccess
	})

	// Leader change: B takes over and keeps enforcing and refreshing.
	stopA()
	rows := kubeRPZQuerylog(t, cfgA)
	if !kubeRPZLogged(rows, "blocked.rpzlab.net", "blocked_rpz", "default/feed") || !kubeRPZLogged(rows, "bad-ip.rpzlab.net", "blocked_rpz", "default/feed") ||
		!kubeRPZLogged(rows, "client.rpzlab.net", "blocked_rpz", "default/feed") || !kubeRPZLogged(rows, "later.rpzlab.net", "blocked_rpz", "default/feed") ||
		!kubeRPZLogged(rows, "good.rpzlab.net", "servfail_upstream", "default/missing") {
		t.Fatal("node A query log lacks the RPZ outcomes")
	}
	await("node B leads", func() bool { return kubeRPZMetric(cfgB, `kudurru_leader{role="controller"} 1`) })
	matrix(cfgB)
	primary.serial.Store(3)
	await("feed update applied after the leader change", func() bool {
		return kubeRPZRcode(cfgB, "after.rpzlab.net.", "udp", nil) == dns.RcodeNameError
	})

	// No store read on the DNS path: the answers hold while the apiserver
	// refuses every new request.
	// Hold it closed past the lease renewals so the refusals are observed.
	gateClosed.Store(true)
	before := gateRefused.Load()
	closedUntil := time.Now().Add(4 * time.Second)
	for time.Now().Before(closedUntil) {
		matrix(cfgB)
	}
	refused := gateRefused.Load() - before
	gateClosed.Store(false)
	if refused == 0 {
		t.Fatal("the closed gate refused no apiserver request; it proves nothing")
	}
	t.Logf("apiserver requests refused while the gate was closed: %d", refused)

	// Restart reuses the unexpired copy while the primary is unavailable.
	// From before the process starts, no answer for the listed name may be
	// NOERROR: the RPZ list is required before the resolver compiles it.
	primary.available.Store(false)
	stopB()
	watch := watchKubeRPZRestart(cfgB, "blocked.rpzlab.net.")
	stopB = startStaticMount(t, cfgB)
	// The primary refuses every refresh, so only the persisted copy can
	// answer; a short bound keeps "at once" without a single-shot probe.
	awaitWithin("restart reuses the unexpired copy", 5*time.Second, func() bool {
		return kubeRPZRcode(cfgB, "after.rpzlab.net.", "udp", nil) == dns.RcodeNameError && kubeRPZRcode(cfgB, "after.rpzlab.net.", "tcp", nil) == dns.RcodeNameError
	})
	matrix(cfgB)
	watch.finish(t, "restart with the primary unavailable", false)

	// The expired copy fails closed; no later answer renews it.
	await("expired feed fails closed", func() bool {
		a := kubeRPZExchange(cfgB, "good.rpzlab.net.", "udp", nil)
		return a != nil && a.Rcode == dns.RcodeServerFailure && kubeRPZEDE(a, "RPZ policy.test. unavailable")
	})
	if rc := kubeRPZRcode(cfgB, "blocked.rpzlab.net.", "tcp", nil); rc != dns.RcodeServerFailure {
		t.Fatal("expired feed answered from its rules", dns.RcodeToString[rc])
	}
	cliB := mountedCLIAs(t, cfgB, "rpz-b", "offline-test-password-1")
	if expired := kubeRPZFeeds(t, cliB, "feed"); len(expired) != 1 || expired[0].State != "expired" || expired[0].Available || expired[0].Node != "rpz-b" ||
		expired[0].Serial == nil || *expired[0].Serial != 3 || expired[0].LastError == "" {
		t.Fatalf("node B expired feed = %+v", expired)
	}
	// A fresh transfer recovers.
	primary.available.Store(true)
	await("fresh transfer recovers", func() bool {
		return kubeRPZRcode(cfgB, "good.rpzlab.net.", "udp", nil) == dns.RcodeSuccess && kubeRPZRcode(cfgB, "blocked.rpzlab.net.", "udp", nil) == dns.RcodeNameError
	})

	// A restart while the apiserver is unreachable cannot read the stored
	// TSIG keys, so the signed feed's unexpired persisted copy is not bound:
	// every recursive query under the policy fails closed (D390). Only
	// unsigned feeds are reused without the apiserver.
	gateClosed.Store(true)
	cfgB.Store.Kube.StartupTimeout = config.Duration(100 * time.Millisecond)
	// The unsigned feed's primary is down too, so only the copy persisted in
	// local.db can answer for it after the restart.
	if kubeRPZRcode(cfgB, "unsigned.rpzlab.net.", "udp", openClient) != dns.RcodeNameError {
		t.Fatal("unsigned feed not enforced before the offline restart")
	}
	openPrimary.available.Store(false)
	stopB()
	if rows := kubeRPZQuerylog(t, cfgB); !kubeRPZLogged(rows, "after.rpzlab.net", "blocked_rpz", "default/feed") || !kubeRPZLogged(rows, "good.rpzlab.net", "servfail_upstream", "default/feed") {
		t.Fatal("node B query log lacks the RPZ outcomes")
	}
	watch = watchKubeRPZRestart(cfgB, "blocked.rpzlab.net.")
	stopB = startStaticMount(t, cfgB)
	// Serving the fresh local.db copy, the configuration is authoritative, so
	// the failure below is the unbound feed, not an unsynced node.
	await("offline restart serves the local.db snapshot", func() bool {
		return kubeRPZMetric(cfgB, "kudurru_store_snapshot_serving 1")
	})
	// The RPZ list is required from the snapshot's policy before the list
	// resolver compiles it (D390), so the signed feed fails closed from the
	// first answer; the bounded wait covers only the unsynced SERVFAIL (no
	// EDE) before the local.db snapshot is published. The watch proves that
	// no answer for the listed name was NOERROR in between.
	offlineClosed := func() bool {
		for _, network := range []string{"udp", "tcp"} {
			a := kubeRPZExchange(cfgB, "good.rpzlab.net.", network, nil)
			if a == nil || a.Rcode != dns.RcodeServerFailure || !kubeRPZEDE(a, "RPZ policy.test. unavailable") {
				return false
			}
			if kubeRPZRcode(cfgB, "blocked.rpzlab.net.", network, nil) != dns.RcodeServerFailure {
				return false
			}
		}
		return true
	}
	awaitWithin("offline restart fails the signed feed closed", 10*time.Second, offlineClosed)
	for holdUntil := time.Now().Add(2 * time.Second); time.Now().Before(holdUntil); time.Sleep(50 * time.Millisecond) {
		if !offlineClosed() {
			t.Fatal("offline restart answered a signed feed without its stored key", kubeRPZDescribe(kubeRPZExchange(cfgB, "good.rpzlab.net.", "udp", nil)))
		}
	}
	// With blocking.allowDegraded: false, readiness came only once the list
	// was in the policy: it never fell back to not ready after it was first
	// reported.
	watch.finish(t, "offline restart", true)
	// The unsigned feed is reused from local.db without the apiserver and
	// without its primary: the stored TSIG keys are not needed to bind it.
	openBefore := openPrimary.transfers.Load()
	awaitWithin("offline restart reuses the unsigned feed", 5*time.Second, func() bool {
		return kubeRPZRcode(cfgB, "unsigned.rpzlab.net.", "udp", openClient) == dns.RcodeNameError && kubeRPZRcode(cfgB, "unsigned.rpzlab.net.", "tcp", openClient) == dns.RcodeNameError
	})
	if a := kubeRPZExchange(cfgB, "good.rpzlab.net.", "udp", openClient); a == nil || a.Rcode != dns.RcodeSuccess || len(a.Answer) != 1 {
		t.Fatal("the unsigned feed's client was failed closed by the signed feed of another policy", kubeRPZDescribe(a))
	}
	if openPrimary.transfers.Load() != openBefore {
		t.Fatal("the unsigned feed was transferred while its primary was down")
	}
	openPrimary.available.Store(true)
	// A restart with the apiserver reachable reads the keys and recovers.
	gateClosed.Store(false)
	cfgB.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
	stopB()
	watch = watchKubeRPZRestart(cfgB, "blocked.rpzlab.net.")
	stopB = startStaticMount(t, cfgB)
	await("online restart binds the signed feed again", func() bool {
		return kubeRPZRcode(cfgB, "good.rpzlab.net.", "udp", nil) == dns.RcodeSuccess && kubeRPZRcode(cfgB, "blocked.rpzlab.net.", "tcp", nil) == dns.RcodeNameError
	})
	watch.finish(t, "online restart", false)
	stopB()
}

// kubeRPZWatch queries one listed name over UDP and reads /healthz/ready in a
// loop from before a mount starts until finish. It counts NOERROR answers,
// which a listed name must never get, and readiness that drops after it was
// first reported.
type kubeRPZWatch struct {
	stop, done                        chan struct{}
	answers, noerror, ready, notReady atomic.Int64
	firstNoError                      atomic.Pointer[string]
}

func watchKubeRPZRestart(cfg *config.Config, name string) *kubeRPZWatch {
	w := &kubeRPZWatch{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		start := time.Now()
		httpClient := &http.Client{Timeout: 200 * time.Millisecond}
		dnsClient := &dns.Client{Net: "udp", Timeout: 200 * time.Millisecond}
		for {
			select {
			case <-w.stop:
				return
			default:
			}
			q := new(dns.Msg)
			q.SetQuestion(name, dns.TypeA)
			q.SetEdns0(1232, false)
			if a, _, err := dnsClient.Exchange(q, cfg.Listeners.DNS[0].Address); err == nil && a != nil {
				w.answers.Add(1)
				if a.Rcode == dns.RcodeSuccess {
					if w.noerror.Add(1) == 1 {
						first := time.Since(start).String() + " " + kubeRPZDescribe(a)
						w.firstNoError.Store(&first)
					}
				}
			}
			if r, err := httpClient.Get("http://" + cfg.Listeners.Ops.Address + "/healthz/ready"); err == nil {
				_, _ = io.Copy(io.Discard, r.Body)
				_ = r.Body.Close()
				switch {
				case r.StatusCode == http.StatusOK:
					w.ready.Add(1)
				case w.ready.Load() > 0:
					w.notReady.Add(1)
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	return w
}

func (w *kubeRPZWatch) finish(t *testing.T, phase string, steadyReadiness bool) {
	t.Helper()
	close(w.stop)
	<-w.done
	if w.answers.Load() == 0 {
		t.Fatal(phase, ": the watch got no DNS answer; it proves nothing")
	}
	if n := w.noerror.Load(); n > 0 {
		t.Fatal(phase, ": the listed name resolved NOERROR ", n, " times of ", w.answers.Load(), "; first at ", *w.firstNoError.Load())
	}
	if steadyReadiness && w.notReady.Load() > 0 {
		t.Fatal(phase, ": the node reported ready, then not ready ", w.notReady.Load(), " times: it was ready before every referenced list was in the policy")
	}
	t.Logf("%s: %d answers for the listed name, none NOERROR; %d ready probes", phase, w.answers.Load(), w.ready.Load())
}

// kubeRPZFeeds reads one BlockList's feeds through the real CLI.
func kubeRPZFeeds(t *testing.T, cli *harness, list string) []api.RPZFeed {
	t.Helper()
	out, stderr, err := cli.run("lists", "rpz", list, "-o", "json")
	if err != nil {
		t.Fatalf("lists rpz %s: %v\n%s", list, err, stderr)
	}
	var items []api.RPZFeed
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	return items
}

type kubeRPZFeed struct {
	address                     string
	available                   atomic.Bool
	serial                      atomic.Uint32
	signed, unsigned, transfers atomic.Int64
}

// kubeRPZPrimary serves policy.test. over TCP, signed with key. Serial 2 adds
// later.rpzlab.net and serial 3 adds after.rpzlab.net. Expire bounds the copy to 30 s
// after the last confirmation.
func kubeRPZPrimary(t *testing.T, key dnsserver.TSIGKey) *kubeRPZFeed {
	t.Helper()
	keys, err := dnsserver.NewStaticKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	feed := &kubeRPZFeed{address: listener.Addr().String()}
	feed.available.Store(true)
	feed.serial.Store(1)
	rr := func(s string) dns.RR {
		r, err := dns.NewRR(s)
		if err != nil {
			panic(err)
		}
		return r
	}
	started := make(chan struct{})
	server := &dns.Server{Listener: listener, TsigProvider: dnsserver.TSIGProvider(keys), NotifyStartedFunc: func() { close(started) },
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, query *dns.Msg) {
			defer func() { _ = w.Close() }()
			if query.IsTsig() == nil || w.TsigStatus() != nil {
				feed.unsigned.Add(1)
				answer := new(dns.Msg)
				answer.SetRcode(query, dns.RcodeRefused)
				_ = w.WriteMsg(answer)
				return
			}
			feed.signed.Add(1)
			answer := new(dns.Msg)
			answer.SetReply(query)
			answer.Authoritative = true
			if !feed.available.Load() || len(query.Question) != 1 || query.Question[0].Name != "policy.test." {
				answer.Rcode = dns.RcodeServerFailure
			} else {
				serial := feed.serial.Load()
				soa := &dns.SOA{Hdr: dns.RR_Header{Name: "policy.test.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 30}, Ns: "ns.policy.test.", Mbox: "hostmaster.policy.test.", Serial: serial, Refresh: 1, Retry: 1, Expire: 30, Minttl: 5}
				answer.Answer = []dns.RR{soa}
				switch query.Question[0].Qtype {
				case dns.TypeSOA:
				case dns.TypeAXFR, dns.TypeIXFR:
					feed.transfers.Add(1)
					answer.Answer = append(answer.Answer,
						rr("policy.test. 30 IN NS ns.policy.test."),
						rr("blocked.rpzlab.net.policy.test. 30 IN CNAME ."),
						rr("32.66.2.0.192.rpz-ip.policy.test. 30 IN CNAME *."),
						rr("32.2.0.0.127.rpz-client-ip.policy.test. 30 IN CNAME ."),
						rr("tcp.rpzlab.net.policy.test. 30 IN CNAME rpz-tcp-only."))
					if serial >= 2 {
						answer.Answer = append(answer.Answer, rr("later.rpzlab.net.policy.test. 30 IN CNAME ."))
					}
					if serial >= 3 {
						answer.Answer = append(answer.Answer, rr("after.rpzlab.net.policy.test. 30 IN CNAME ."))
					}
					answer.Answer = append(answer.Answer, dns.Copy(soa))
				default:
					answer.Rcode = dns.RcodeRefused
				}
			}
			answer.SetTsig(query.IsTsig().Hdr.Name, query.IsTsig().Algorithm, 300, time.Now().Unix())
			_ = w.WriteMsg(answer)
		})}
	go func() { _ = server.ActivateAndServe() }()
	t.Cleanup(func() { _ = server.Shutdown() })
	select {
	case <-started:
	case <-time.After(4 * time.Second):
		t.Fatal("RPZ primary did not start")
	}
	return feed
}

// kubeRPZOpenPrimary serves the unsigned open.test. over TCP: one QNAME rule
// for unsigned.rpzlab.net. While unavailable it answers SERVFAIL and counts
// the query as refused. Expire bounds the copy to 120 s after the last
// confirmation.
func kubeRPZOpenPrimary(t *testing.T) *kubeRPZOpenFeed {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	feed := &kubeRPZOpenFeed{address: listener.Addr().String()}
	feed.available.Store(true)
	started := make(chan struct{})
	server := &dns.Server{Listener: listener, NotifyStartedFunc: func() { close(started) },
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, query *dns.Msg) {
			defer func() { _ = w.Close() }()
			answer := new(dns.Msg)
			answer.SetReply(query)
			answer.Authoritative = true
			if !feed.available.Load() || len(query.Question) != 1 || query.Question[0].Name != "open.test." {
				feed.refused.Add(1)
				answer.Rcode = dns.RcodeServerFailure
				_ = w.WriteMsg(answer)
				return
			}
			soa := &dns.SOA{Hdr: dns.RR_Header{Name: "open.test.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 30}, Ns: "ns.open.test.", Mbox: "hostmaster.open.test.", Serial: 1, Refresh: 1, Retry: 1, Expire: 120, Minttl: 5}
			answer.Answer = []dns.RR{soa}
			switch query.Question[0].Qtype {
			case dns.TypeSOA:
			case dns.TypeAXFR, dns.TypeIXFR:
				feed.transfers.Add(1)
				ns, _ := dns.NewRR("open.test. 30 IN NS ns.open.test.")
				rule, _ := dns.NewRR("unsigned.rpzlab.net.open.test. 30 IN CNAME .")
				answer.Answer = append(answer.Answer, ns, rule, dns.Copy(soa))
			default:
				answer.Rcode = dns.RcodeRefused
			}
			_ = w.WriteMsg(answer)
		})}
	go func() { _ = server.ActivateAndServe() }()
	t.Cleanup(func() { _ = server.Shutdown() })
	select {
	case <-started:
	case <-time.After(4 * time.Second):
		t.Fatal("unsigned RPZ primary did not start")
	}
	return feed
}

type kubeRPZOpenFeed struct {
	address            string
	available          atomic.Bool
	transfers, refused atomic.Int64
}

// kubeRPZUpstream answers every A query; bad-ip.rpzlab.net resolves to the
// RESPONSE-IP trigger 192.0.2.66.
func kubeRPZUpstream(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	server := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, query *dns.Msg) {
		answer := new(dns.Msg)
		answer.SetReply(query)
		answer.RecursionAvailable = true
		if len(query.Question) == 1 && query.Question[0].Qtype == dns.TypeA {
			ip := net.IPv4(192, 0, 2, 10)
			if query.Question[0].Name == "bad-ip.rpzlab.net." {
				ip = net.IPv4(192, 0, 2, 66)
			}
			answer.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: query.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30}, A: ip}}
		}
		_ = w.WriteMsg(answer)
	})}
	go func() { _ = server.ActivateAndServe() }()
	t.Cleanup(func() { _ = server.Shutdown() })
	<-started
	return pc.LocalAddr().String()
}

// kubeRPZDualAddress returns a loopback port that was free for both UDP and
// TCP when probed.
func kubeRPZDualAddress(t *testing.T) string {
	t.Helper()
	for range 50 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := l.Addr().String()
		pc, err := net.ListenPacket("udp", address)
		_ = l.Close()
		if err == nil {
			_ = pc.Close()
			return address
		}
	}
	t.Fatal("no loopback port free for both UDP and TCP")
	return ""
}

func kubeRPZExchange(cfg *config.Config, name, network string, source net.IP) *dns.Msg {
	q := new(dns.Msg)
	q.SetQuestion(name, dns.TypeA)
	q.SetEdns0(1232, false)
	c := &dns.Client{Net: network, Timeout: 2 * time.Second}
	if source != nil {
		if network == "udp" {
			c.Dialer = &net.Dialer{LocalAddr: &net.UDPAddr{IP: source}}
		} else {
			c.Dialer = &net.Dialer{LocalAddr: &net.TCPAddr{IP: source}}
		}
	}
	a, _, err := c.Exchange(q, cfg.Listeners.DNS[0].Address)
	if err != nil {
		return nil
	}
	return a
}

func kubeRPZRcode(cfg *config.Config, name, network string, source net.IP) int {
	if a := kubeRPZExchange(cfg, name, network, source); a != nil {
		return a.Rcode
	}
	return -1
}

func kubeRPZEDE(a *dns.Msg, text string) bool {
	if opt := a.IsEdns0(); opt != nil {
		for _, o := range opt.Option {
			if ede, ok := o.(*dns.EDNS0_EDE); ok && ede.InfoCode == dns.ExtendedErrorCodeNotReady && ede.ExtraText == text {
				return true
			}
		}
	}
	return false
}

// kubeRPZDescribe renders an answer's rcode, answer count and EDE texts for
// failure messages.
func kubeRPZDescribe(a *dns.Msg) string {
	if a == nil {
		return "no answer"
	}
	out := dns.RcodeToString[a.Rcode] + " answers=" + strconv.Itoa(len(a.Answer))
	if opt := a.IsEdns0(); opt != nil {
		for _, o := range opt.Option {
			if ede, ok := o.(*dns.EDNS0_EDE); ok {
				out += " ede=" + strconv.Itoa(int(ede.InfoCode)) + ":" + strconv.Quote(ede.ExtraText)
			}
		}
	}
	return out
}

func kubeRPZMetric(cfg *config.Config, line string) bool {
	c := &http.Client{Timeout: time.Second}
	res, err := c.Get("http://" + cfg.Listeners.Metrics.Address + "/metrics")
	if err != nil {
		return false
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	return err == nil && strings.Contains(string(body), "\n"+line+"\n")
}

// kubeRPZQuerylog reads the stopped node's local query log.
func kubeRPZQuerylog(t *testing.T, cfg *config.Config) []sqlitelog.QueryRow {
	t.Helper()
	opts, err := sqlitelog.OptionsFrom(cfg.Querylog, cfg.QuerylogDataDir(), cfg.Node.Name)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	w, err := sqlitelog.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	page, err := w.Reader().Queries(ctx, sqlitelog.QueryFilter{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return page.Queries
}

func kubeRPZLogged(rows []sqlitelog.QueryRow, domain, status, list string) bool {
	for _, row := range rows {
		if row.Domain == domain && row.Status == status && row.List == list {
			return true
		}
	}
	return false
}
