package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
)

// Client-group views on the Kubernetes driver (D403): the zone view built
// from informer data answers two ClientGroups differently for one name over
// UDP and TCP, a view-only name is NXDOMAIN for other clients, and a view
// change reaches the served zone.
func TestRunServeKubeClientGroupViews(t *testing.T) {
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
	front := httptest.NewServer(proxy)
	t.Cleanup(front.Close)

	cfg := offlineServeConfig(t, front.URL)
	cfg.Node.Name = "views-a"
	cfg.Store.Kube.ExpectedClusterID = string(namespace.UID)
	cfg.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
	cfg.DNSSEC.Enabled = false
	cfg.Listeners.DNS[0].Address = kubeRPZDualAddress(t)
	cfg.Listeners.DNS[0].Protocols = []string{"udp", "tcp"}
	cfg.Listeners.Metrics.Address = offlineFreeAddress(t, "tcp")
	source, err := json.Marshal(map[string]any{"store": map[string]any{"driver": "kube"}, "dnssec": map[string]any{"enabled": false}})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Source = filepath.Join(t.TempDir(), "kudurru.json")
	if err := os.WriteFile(cfg.Source, source, 0o600); err != nil {
		t.Fatal(err)
	}

	view := func(v string) *public.EndpointExt { return &public.EndpointExt{View: v} }
	records := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "split", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "split"}, Endpoints: []public.Endpoint{
		{DNSName: "www.split.test", RecordType: "A", Targets: []string{"203.0.113.10"}},
		{DNSName: "www.split.test", RecordType: "A", Targets: []string{"10.0.0.10"}, Kudurru: view("lan")},
		{DNSName: "www.split.test", RecordType: "A", Targets: []string{"10.9.0.10"}, Kudurru: view("kids")},
		{DNSName: "nas.split.test", RecordType: "A", Targets: []string{"10.0.0.20"}, Kudurru: view("lan")},
		{DNSName: "tunnel.split.test", RecordType: "A", Targets: []string{"198.51.100.200"}, Kudurru: view("public")},
	}}}
	for _, object := range []client.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "split"}, Spec: public.DNSZoneSpec{ZoneName: "split.test.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns1.split.test."}}},
		&public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "lan", Namespace: "default"}, Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{IP: "127.0.0.2"}}}},
		&public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "kids", Namespace: "default"}, Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{IP: "127.0.0.3"}}}},
		records,
	} {
		if err := admin.Create(t.Context(), object); err != nil {
			t.Fatal(err)
		}
	}
	// The CRD schema refuses a view that cannot name a ClientGroup.
	bad := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "split"}, Endpoints: []public.Endpoint{
		{DNSName: "x.split.test", RecordType: "A", Targets: []string{"10.0.0.1"}, Kudurru: view("Not_A_Group")}}}}
	if err := admin.Create(t.Context(), bad); err == nil {
		t.Fatal("the apiserver accepted an invalid view name")
	}

	stop := startStaticMount(t, cfg)
	defer stop()
	answer := func(name, network, from string) (int, string) {
		m := kubeRPZExchange(cfg, name, network, net.ParseIP(from))
		if m == nil {
			return -1, ""
		}
		for _, rr := range m.Answer {
			if a, ok := rr.(*dns.A); ok {
				return m.Rcode, a.A.String()
			}
		}
		return m.Rcode, ""
	}
	await := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(45 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatal("never converged:", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	await("the lan view is served", func() bool {
		_, lan := answer("www.split.test.", "udp", "127.0.0.2")
		_, other := answer("www.split.test.", "udp", "127.0.0.1")
		return lan == "10.0.0.10" && other == "203.0.113.10"
	})
	for _, network := range []string{"udp", "tcp"} {
		for _, tc := range []struct {
			name, from string
			rcode      int
			want       string
		}{
			{"www.split.test.", "127.0.0.1", dns.RcodeSuccess, "203.0.113.10"},
			{"www.split.test.", "127.0.0.2", dns.RcodeSuccess, "10.0.0.10"},
			{"www.split.test.", "127.0.0.3", dns.RcodeSuccess, "10.9.0.10"},
			{"nas.split.test.", "127.0.0.1", dns.RcodeNameError, ""},
			{"nas.split.test.", "127.0.0.3", dns.RcodeNameError, ""},
			{"nas.split.test.", "127.0.0.2", dns.RcodeSuccess, "10.0.0.20"},
			{"tunnel.split.test.", "127.0.0.2", dns.RcodeNameError, ""},
		} {
			if rcode, got := answer(tc.name, network, tc.from); rcode != tc.rcode || got != tc.want {
				t.Fatalf("%s %s from %s: %d %q, want %d %q", network, tc.name, tc.from, rcode, got, tc.rcode, tc.want)
			}
		}
	}
	// A view change reaches the served zone.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current public.DNSRecordSet
		if err := admin.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "split"}, &current); err != nil {
			return err
		}
		current.Spec.Endpoints[1].Targets = []string{"10.0.0.11"}
		return admin.Update(t.Context(), &current)
	}); err != nil {
		t.Fatal(err)
	}
	await("the changed lan view is served", func() bool {
		_, lan := answer("www.split.test.", "tcp", "127.0.0.2")
		_, other := answer("www.split.test.", "tcp", "127.0.0.1")
		return lan == "10.0.0.11" && other == "203.0.113.10"
	})
}

// RFC 2136 UPDATE ownership at Kubernetes view endpoints (D403): static view
// names are reserved against UPDATE in every namespace, while prerequisites
// still see only default data.
func TestRunServeKubeViewUpdateOwnership(t *testing.T) {
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
	var system corev1.Namespace
	if err := admin.Get(t.Context(), client.ObjectKey{Name: "kube-system"}, &system); err != nil {
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
	front := httptest.NewServer(proxy)
	t.Cleanup(front.Close)

	cfg := offlineServeConfig(t, front.URL)
	cfg.Node.Name = "view-update-a"
	cfg.Store.Kube.ExpectedClusterID = string(system.UID)
	cfg.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0
	cfg.Listeners.DNS[0].Protocols = []string{"udp", "tcp"}

	st, err := kube.New(actual, kube.Options{Scheme: scheme, InternalKinds: privateStoreKinds(), ExpectedClusterID: string(system.UID), StartupTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sealer, err := tsigkeys.NewSealer(make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := tsigkeys.Read(t.Context(), st, string(system.UID))
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
	secret := bytes.Repeat([]byte{0x45}, 32)
	plan, err := prepared.Plan(sealer, tsigkeys.MutationInput{Operation: tsigkeys.Create, KeyName: "view-updater.", Algorithm: "hmac-sha256", Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.ApplyStore(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	keys, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "view-updater.", Algorithm: dns.HmacSHA256, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}

	view := func(v string) *public.EndpointExt { return &public.EndpointExt{View: v} }
	for _, object := range []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kids"}},
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "view-update"}, Spec: public.DNSZoneSpec{ZoneName: "view-update.test.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.view-update.test."}, Policy: public.ZonePolicy{Update: public.AccessAllow, UpdatePolicies: []public.UpdatePolicy{{TSIGKeyRef: "view-updater", Name: "*.view-update.test.", Types: []string{"A"}}}}}},
		&public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "lan", Namespace: "default"}, Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{IP: "127.0.0.2"}}}},
		&public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "lan", Namespace: "kids"}, Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{IP: "127.0.0.3"}}}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "same-view", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "view-update"}, Endpoints: []public.Endpoint{{DNSName: "same.view-update.test.", RecordType: "A", Targets: []string{"10.0.0.10"}, Kudurru: view("lan")}}}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "cross-view", Namespace: "kids"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "view-update"}, Endpoints: []public.Endpoint{{DNSName: "cross.view-update.test.", RecordType: "A", Targets: []string{"10.0.0.20"}, Kudurru: view("lan")}}}},
	} {
		if err := admin.Create(t.Context(), object); err != nil {
			t.Fatal(err)
		}
	}

	stop := startStaticMount(t, cfg)
	defer stop()
	answer := func(name, from string) (int, string) {
		m := kubeRPZExchange(cfg, name, "udp", net.ParseIP(from))
		if m == nil {
			return -1, ""
		}
		for _, rr := range m.Answer {
			if a, ok := rr.(*dns.A); ok {
				return m.Rcode, a.A.String()
			}
		}
		return m.Rcode, ""
	}
	await := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(45 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatal("never converged:", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	await("both view-only names are initially served only to their groups", func() bool {
		sameCode, same := answer("same.view-update.test.", "127.0.0.2")
		crossCode, cross := answer("cross.view-update.test.", "127.0.0.3")
		defaultSameCode, defaultSame := answer("same.view-update.test.", "127.0.0.1")
		defaultCrossCode, defaultCross := answer("cross.view-update.test.", "127.0.0.1")
		return sameCode == dns.RcodeSuccess && same == "10.0.0.10" && crossCode == dns.RcodeSuccess && cross == "10.0.0.20" &&
			defaultSameCode == dns.RcodeNameError && defaultSame == "" && defaultCrossCode == dns.RcodeNameError && defaultCross == ""
	})
	rr := func(text string) dns.RR {
		t.Helper()
		out, err := dns.NewRR(text)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	update := func(build func(*dns.Msg)) int {
		t.Helper()
		m := new(dns.Msg)
		m.SetUpdate("view-update.test.")
		build(m)
		m.SetTsig("view-updater.", dns.HmacSHA256, 300, time.Now().Unix())
		response, _, err := (&dns.Client{Net: "tcp", Timeout: 5 * time.Second, TsigProvider: dnsserver.TSIGProvider(keys)}).Exchange(m, cfg.Listeners.DNS[0].Address)
		if err != nil {
			t.Fatal("UPDATE exchange failed", err)
		}
		if response.IsTsig() == nil {
			t.Fatal("signed UPDATE response is not signed")
		}
		return response.Rcode
	}

	// An ordinary unreserved owner remains writable.
	if rc := update(func(m *dns.Msg) {
		m.Insert([]dns.RR{rr("ordinary.view-update.test. 60 IN A 192.0.2.30")})
	}); rc != dns.RcodeSuccess {
		t.Fatal("ordinary UPDATE refused", dns.RcodeToString[rc])
	}
	await("ordinary UPDATE is served to default and grouped clients", func() bool {
		_, lan := answer("ordinary.view-update.test.", "127.0.0.2")
		_, other := answer("ordinary.view-update.test.", "127.0.0.1")
		return lan == "192.0.2.30" && other == "192.0.2.30"
	})

	// UPDATE prerequisites remain default-only: the same-namespace view-only
	// RRset is absent to NXRRSET, so an unrelated permitted insertion applies.
	if rc := update(func(m *dns.Msg) {
		m.RRsetNotUsed([]dns.RR{rr("same.view-update.test. 0 IN A 0.0.0.0")})
		m.Insert([]dns.RR{rr("prerequisite.view-update.test. 60 IN A 192.0.2.31")})
	}); rc != dns.RcodeSuccess {
		t.Fatal("default-only NXRRSET prerequisite failed", dns.RcodeToString[rc])
	}
	await("prerequisite-controlled UPDATE is served", func() bool {
		_, inserted := answer("prerequisite.view-update.test.", "127.0.0.1")
		_, same := answer("same.view-update.test.", "127.0.0.2")
		sameDefaultCode, sameDefault := answer("same.view-update.test.", "127.0.0.1")
		return inserted == "192.0.2.31" && same == "10.0.0.10" && sameDefaultCode == dns.RcodeNameError && sameDefault == ""
	})

	updateKey := client.ObjectKey{Namespace: "default", Name: "rfc2136-view-update"}
	var before public.DNSRecordSet
	if err := admin.Get(t.Context(), updateKey, &before); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, target string
	}{
		{"same.view-update.test.", "192.0.2.10"},
		{"cross.view-update.test.", "192.0.2.20"},
	} {
		if rc := update(func(m *dns.Msg) {
			m.Insert([]dns.RR{rr(tc.name + " 60 IN A " + tc.target)})
		}); rc != dns.RcodeRefused {
			t.Fatalf("view-name UPDATE for %s returned %s, want REFUSED", tc.name, dns.RcodeToString[rc])
		}
	}
	var after public.DNSRecordSet
	if err := admin.Get(t.Context(), updateKey, &after); err != nil {
		t.Fatal(err)
	}
	if after.UID != before.UID || after.Generation != before.Generation {
		t.Fatalf("refused UPDATE modified the stored record set: before=%+v, after=%+v", before.Spec.Endpoints, after.Spec.Endpoints)
	}
	await("refused UPDATEs leave both view and default answers unchanged", func() bool {
		_, same := answer("same.view-update.test.", "127.0.0.2")
		_, cross := answer("cross.view-update.test.", "127.0.0.3")
		sameDefaultCode, sameDefault := answer("same.view-update.test.", "127.0.0.1")
		crossDefaultCode, crossDefault := answer("cross.view-update.test.", "127.0.0.1")
		return same == "10.0.0.10" && cross == "10.0.0.20" &&
			sameDefaultCode == dns.RcodeNameError && sameDefault == "" && crossDefaultCode == dns.RcodeNameError && crossDefault == ""
	})
	await("view sources remain reconciled without Duplicated", func() bool {
		for _, key := range []client.ObjectKey{{Namespace: "default", Name: "same-view"}, {Namespace: "kids", Name: "cross-view"}} {
			var current public.DNSRecordSet
			if err := admin.Get(t.Context(), key, &current); err != nil || current.Status.ObservedGeneration != current.Generation {
				return false
			}
			for _, condition := range current.Status.Conditions {
				if condition.Reason == public.ReasonDuplicated {
					return false
				}
			}
		}
		return true
	})
}
