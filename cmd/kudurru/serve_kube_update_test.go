package main

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
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
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
)

// RFC 2136 on the Kubernetes driver (F12a, D366): prerequisites, key x owner x
// type grants, ownership, serialization of concurrent UPDATEs, serial advance,
// durability across restart and removal of the UPDATE-owned set.
func TestRunServeKubeDynamicUpdateAtomicityAndOwnership(t *testing.T) {
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
	gate := httptest.NewServer(proxy)
	t.Cleanup(gate.Close)
	cfg := offlineServeConfig(t, gate.URL)
	cfg.Store.Kube.ExpectedClusterID = string(namespace.UID)
	cfg.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0
	cfg.Listeners.DNS[0].Protocols = []string{"udp", "tcp"}

	// Provision the stored update key before startup.
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
	secret := bytes.Repeat([]byte{0x44}, 32)
	plan, err := prepared.Plan(sealer, tsigkeys.MutationInput{Operation: tsigkeys.Create, KeyName: "updater.", Algorithm: "hmac-sha256", Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.ApplyStore(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	keys, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "updater.", Algorithm: dns.HmacSHA256, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range []client.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "dyn"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "dyn.test.", Nameservers: []string{"ns.dyn.test."},
			Policy: public.ZonePolicy{Update: public.AccessAllow, UpdatePolicies: []public.UpdatePolicy{{TSIGKeyRef: "updater", Name: "*.dyn.test.", Types: []string{"A", "TXT"}}}}}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "managed", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "dyn"}, Endpoints: []public.Endpoint{{DNSName: "api.dyn.test.", RecordType: "A", Targets: []string{"192.0.2.1"}}}}},
	} {
		if err := admin.Create(t.Context(), object); err != nil {
			t.Fatal(err)
		}
	}
	update := func(sign bool, build func(*dns.Msg)) int {
		t.Helper()
		m := new(dns.Msg)
		m.SetUpdate("dyn.test.")
		build(m)
		c := &dns.Client{Net: "tcp", Timeout: 5 * time.Second}
		if sign {
			m.SetTsig("updater.", dns.HmacSHA256, 300, time.Now().Unix())
			c.TsigProvider = dnsserver.TSIGProvider(keys)
		}
		answer, _, err := c.Exchange(m, cfg.Listeners.DNS[0].Address)
		if err != nil {
			t.Fatal("UPDATE exchange failed", err)
		}
		if sign && answer.IsTsig() == nil {
			t.Fatal("signed UPDATE response is not signed")
		}
		return answer.Rcode
	}
	rr := func(s string) dns.RR {
		r, err := dns.NewRR(s)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	answers := func(name, want string) bool {
		q := new(dns.Msg).SetQuestion(name, dns.TypeA)
		a, _, err := (&dns.Client{Net: "udp", Timeout: time.Second}).Exchange(q, cfg.Listeners.DNS[0].Address)
		if err != nil || !a.Authoritative {
			return false
		}
		if want == "" {
			return a.Rcode == dns.RcodeNameError
		}
		return len(a.Answer) == 1 && a.Answer[0].(*dns.A).A.String() == want
	}
	await := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatal("never converged:", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	serial := func() uint32 {
		q := new(dns.Msg).SetQuestion("dyn.test.", dns.TypeSOA)
		a, _, err := (&dns.Client{Net: "udp", Timeout: time.Second}).Exchange(q, cfg.Listeners.DNS[0].Address)
		if err != nil || len(a.Answer) != 1 {
			return 0
		}
		return a.Answer[0].(*dns.SOA).Serial
	}
	stop := startStaticMount(t, cfg)
	await("API record served", func() bool { return answers("api.dyn.test.", "192.0.2.1") })
	before := serial()

	// Add under the grant; the response is signed and the record is served.
	if rc := update(true, func(m *dns.Msg) { m.Insert([]dns.RR{rr("host.dyn.test. 60 IN A 192.0.2.10")}) }); rc != dns.RcodeSuccess {
		t.Fatal("authorized UPDATE refused", dns.RcodeToString[rc])
	}
	await("dynamic record served", func() bool { return answers("host.dyn.test.", "192.0.2.10") })
	var owned public.DNSRecordSet
	if err := admin.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "rfc2136-dyn"}, &owned); err != nil || store.ManagedBy(&owned) != store.ManagerDynamicUpdate {
		t.Fatal("UPDATE-owned record set missing or foreign", err)
	}
	await("serial advanced", func() bool { return serial() != before && serial() != 0 })

	// A failed prerequisite (name must not exist) applies nothing.
	rv := owned.ResourceVersion
	if rc := update(true, func(m *dns.Msg) {
		m.NameNotUsed([]dns.RR{rr("host.dyn.test. 0 IN A 0.0.0.0")})
		m.Insert([]dns.RR{rr("other.dyn.test. 60 IN A 192.0.2.11")})
	}); rc != dns.RcodeYXDomain {
		t.Fatal("prerequisite failure not reported", dns.RcodeToString[rc])
	}
	// Ungranted type, unsigned request and a write to an API-owned name refuse.
	if rc := update(true, func(m *dns.Msg) { m.Insert([]dns.RR{rr("host.dyn.test. 60 IN MX 10 mail.dyn.test.")}) }); rc != dns.RcodeRefused {
		t.Fatal("ungranted type not refused", dns.RcodeToString[rc])
	}
	if rc := update(false, func(m *dns.Msg) { m.Insert([]dns.RR{rr("unsigned.dyn.test. 60 IN A 192.0.2.12")}) }); rc != dns.RcodeRefused {
		t.Fatal("unsigned UPDATE not refused", dns.RcodeToString[rc])
	}
	if rc := update(true, func(m *dns.Msg) { m.RemoveRRset([]dns.RR{rr("api.dyn.test. 0 IN A 0.0.0.0")}) }); rc == dns.RcodeSuccess {
		if !answers("api.dyn.test.", "192.0.2.1") {
			t.Fatal("UPDATE removed an API-owned record")
		}
	}
	if err := admin.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "rfc2136-dyn"}, &owned); err != nil || owned.ResourceVersion != rv {
		t.Fatal("a refused UPDATE wrote the UPDATE-owned set", err)
	}
	if !answers("other.dyn.test.", "") || !answers("api.dyn.test.", "192.0.2.1") {
		t.Fatal("refused UPDATEs changed served data")
	}

	// Concurrent UPDATEs serialize: none is lost.
	var wg sync.WaitGroup
	codes := make([]int, 12)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := new(dns.Msg)
			m.SetUpdate("dyn.test.")
			m.Insert([]dns.RR{rr(fmt.Sprintf("c%d.dyn.test. 60 IN A 192.0.2.%d", i, 100+i))})
			m.SetTsig("updater.", dns.HmacSHA256, 300, time.Now().Unix())
			a, _, err := (&dns.Client{Net: "tcp", Timeout: 15 * time.Second, TsigProvider: dnsserver.TSIGProvider(keys)}).Exchange(m, cfg.Listeners.DNS[0].Address)
			if err == nil {
				codes[i] = a.Rcode
			} else {
				codes[i] = -1
			}
		}()
	}
	wg.Wait()
	for i, rc := range codes {
		if rc != dns.RcodeSuccess {
			t.Fatal("concurrent UPDATE on one node was not serialized", i, rc)
		}
		name := fmt.Sprintf("c%d.dyn.test.", i)
		await("concurrent update served", func() bool { return answers(name, fmt.Sprintf("192.0.2.%d", 100+i)) })
	}
	succeeded := 0
	for _, rc := range codes {
		if rc == dns.RcodeSuccess {
			succeeded++
		}
	}
	if succeeded == 0 {
		t.Fatal("no concurrent UPDATE committed")
	}
	t.Logf("concurrent UPDATEs committed: %d of %d", succeeded, len(codes))

	// Two nodes accept UPDATEs concurrently; the per-zone CAS serializes them
	// across nodes, so every acknowledged UPDATE is present afterwards.
	cfgB := offlineServeConfig(t, gate.URL)
	cfgB.Node.Name = "update-b"
	cfgB.Store.Kube.ExpectedClusterID = string(namespace.UID)
	cfgB.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
	cfgB.Blocking.Enabled = false
	cfgB.DNSSEC.Enabled = false
	cfgB.Cache.MaxEntries = 0
	cfgB.Listeners.DNS[0].Protocols = []string{"udp", "tcp"}
	stopB := startStaticMount(t, cfgB)
	awaitOn := func(c *config.Config, name, want string) bool {
		q := new(dns.Msg).SetQuestion(name, dns.TypeA)
		a, _, err := (&dns.Client{Net: "udp", Timeout: time.Second}).Exchange(q, c.Listeners.DNS[0].Address)
		return err == nil && a.Authoritative && len(a.Answer) == 1 && a.Answer[0].(*dns.A).A.String() == want
	}
	await("second node serves dynamic data", func() bool { return awaitOn(cfgB, "c0.dyn.test.", "192.0.2.100") })
	cross := make([]int, 16)
	for i := range cross {
		wg.Add(1)
		go func() {
			defer wg.Done()
			addr := cfg.Listeners.DNS[0].Address
			if i%2 == 1 {
				addr = cfgB.Listeners.DNS[0].Address
			}
			m := new(dns.Msg)
			m.SetUpdate("dyn.test.")
			m.Insert([]dns.RR{rr(fmt.Sprintf("x%d.dyn.test. 60 IN A 198.51.100.%d", i, 10+i))})
			m.SetTsig("updater.", dns.HmacSHA256, 300, time.Now().Unix())
			a, _, err := (&dns.Client{Net: "tcp", Timeout: 15 * time.Second, TsigProvider: dnsserver.TSIGProvider(keys)}).Exchange(m, addr)
			if err == nil {
				cross[i] = a.Rcode
			} else {
				cross[i] = -1
			}
		}()
	}
	wg.Wait()
	acknowledged := 0
	for i, rc := range cross {
		switch rc {
		case dns.RcodeSuccess:
			acknowledged++
			name, want := fmt.Sprintf("x%d.dyn.test.", i), fmt.Sprintf("198.51.100.%d", 10+i)
			await("acknowledged cross-node UPDATE present", func() bool { return answers(name, want) && awaitOn(cfgB, name, want) })
		case dns.RcodeServerFailure:
			// Bounded contention refusal: nothing was committed for it.
			if answers(fmt.Sprintf("x%d.dyn.test.", i), fmt.Sprintf("198.51.100.%d", 10+i)) {
				t.Fatal("a refused UPDATE was applied", i)
			}
		default:
			t.Fatal("cross-node UPDATE failed unexpectedly", i, rc)
		}
	}
	t.Logf("cross-node UPDATEs acknowledged: %d of %d", acknowledged, len(cross))
	if acknowledged < len(cross)/2 {
		t.Fatal("cross-node serialization starved", acknowledged)
	}
	stopB()

	// Restart keeps every committed dynamic record.
	stop()
	stop = startStaticMount(t, cfg)
	await("dynamic record survives restart", func() bool { return answers("host.dyn.test.", "192.0.2.10") && answers("c0.dyn.test.", "192.0.2.100") })

	// A name-wide delete is allowed when every type present there is granted.
	if rc := update(true, func(m *dns.Msg) { m.RemoveName([]dns.RR{rr("host.dyn.test. 0 IN A 0.0.0.0")}) }); rc != dns.RcodeSuccess {
		t.Fatal("name-wide delete of granted types refused", dns.RcodeToString[rc])
	}
	await("name-wide delete applied", func() bool { return answers("host.dyn.test.", "") })
	// Removing every dynamic RRset deletes the UPDATE-owned set.
	if rc := update(true, func(m *dns.Msg) {
		m.RemoveRRset([]dns.RR{rr("host.dyn.test. 0 IN A 0.0.0.0")})
		for i := range codes {
			m.RemoveRRset([]dns.RR{rr(fmt.Sprintf("c%d.dyn.test. 0 IN A 0.0.0.0", i))})
		}
		for i := range cross {
			m.RemoveRRset([]dns.RR{rr(fmt.Sprintf("x%d.dyn.test. 0 IN A 0.0.0.0", i))})
		}
	}); rc != dns.RcodeSuccess {
		t.Fatal("removal UPDATE refused", dns.RcodeToString[rc])
	}
	await("dynamic record removed", func() bool { return answers("host.dyn.test.", "") })
	if err := admin.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "rfc2136-dyn"}, &owned); err == nil {
		t.Fatal("empty UPDATE-owned set was not deleted")
	}
	if !answers("api.dyn.test.", "192.0.2.1") {
		t.Fatal("removal UPDATE affected an API-owned record")
	}
	stop()
}
