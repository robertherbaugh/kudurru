package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
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
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
)

// A primary zone and a primary child zone under it on one mounted node
// (D404): created in both orders, each written, the child deleted and
// created again twice. Direct UDP probes for a name of each zone are sent
// every 5 ms from the zone's first answer until the test deletes it. Every
// probe must get the zone's committed answer: never SERVFAIL, never a wrong
// answer, and (without the race detector) never silence. Every step must
// converge within the mounted bound: never stuck.
//
// Before D404 a child created after its parent stayed SERVFAIL for good
// in 3 of 5 runs (CORE-COMMIT-GAP fix-13): the parent's lifecycle receipt
// was behind its DNSZone after a committed-status write, the child's fence
// required the exact receipt, and realigning the receipt required the
// fence. Created together, the parent never answered in 4 of 6 race runs
// (fix-06, fix-15). And the parent answered SERVFAIL from a child's
// creation until its fence was published, the child from a late parent's
// fence until that parent activated.
func TestRunServeSQLitePrimaryParentAndChildZones(t *testing.T) {
	for _, parentFirst := range []bool{true, false} {
		t.Run(fmt.Sprint("parentFirst=", parentFirst), func(t *testing.T) {
			cfg, _ := mountedTestConfig(t)
			licenseKey := trustTestKey(t)(license.Claims{Plan: license.PlanPersonal, Nodes: 3})
			cfg.Auth.IdentityProviders = nil
			cfg.DNSSEC.Enabled = false
			cfg.Cache.MaxEntries = 0
			cfg.RateLimit.Enabled = false
			cfg.Blocking.Enabled = false
			cfg.Listeners.Internal.Address = offlineFreeAddress(t, "tcp")
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- runServe(ctx, cfg, licenseKey) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error("serve ended", err)
					}
				case <-time.After(15 * time.Second):
					t.Error("serve failed to join shutdown")
				}
			})
			for deadline := time.Now().Add(60 * time.Second); offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK; {
				if time.Now().After(deadline) {
					t.Fatal("the node did not become ready")
				}
				time.Sleep(30 * time.Millisecond)
			}
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
			base := "http://" + cfg.Listeners.API.Address + api.BasePath
			var login api.SessionInfo
			controlMountedRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
			mountedParentAndChildZones(t, cfg, pendingAwareCall(t, client, base, login.CSRF), parentFirst)
		})
	}
}

// mountedCall sends one API request of a test's own setup and writes.
type mountedCall func(method, path string, body any, want int, out any)

// pendingAwareCall asks a request again after a refusal, as
// TestRunServeSQLiteZonesAnswerAcrossCommits does: a 503
// generation_pending always, and the SQLite command path's "current
// writable serving generation unavailable" too. The tests' POSTs create one
// named object each, so asking one again is safe: if the earlier attempt
// did create it, the repeat answers 409 and the object exists, which is
// accepted only after such a 503. What the tests assert is DNS, never
// these requests.
func pendingAwareCall(t *testing.T, client *http.Client, base, csrf string) mountedCall {
	return func(method, path string, body any, want int, out any) {
		t.Helper()
		var encoded []byte
		if body != nil {
			var err error
			if encoded, err = json.Marshal(body); err != nil {
				t.Fatal(err)
			}
		}
		repeated := false
		for deadline := time.Now().Add(30 * time.Second); ; {
			req, err := http.NewRequestWithContext(t.Context(), method, base+path, bytes.NewReader(encoded))
			if err != nil {
				t.Fatal(err)
			}
			if body != nil {
				req.Header.Set("Content-Type", "application/json")
			}
			req.Header.Set("X-Kudurru-CSRF", csrf)
			response, err := client.Do(req)
			if err != nil {
				t.Fatal("control request failed", method, path, err)
			}
			raw, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
			_ = response.Body.Close()
			if err != nil {
				t.Fatal("control response unreadable", method, path, err)
			}
			if response.StatusCode == want {
				if out != nil {
					if err := json.Unmarshal(raw, out); err != nil {
						t.Fatal("control response decoding failed", method, path, err)
					}
				}
				return
			}
			if repeated && method == http.MethodPost && want == http.StatusCreated && response.StatusCode == http.StatusConflict && out == nil {
				return
			}
			again := response.StatusCode == http.StatusServiceUnavailable && (bytes.Contains(raw, []byte("generation_pending")) || bytes.Contains(raw, []byte("current writable serving generation unavailable")))
			if !again || time.Now().After(deadline) {
				t.Fatalf("control %s %s: status=%d want=%d: %s", method, path, response.StatusCode, want, raw)
			}
			repeated = true
			time.Sleep(25 * time.Millisecond)
		}
	}
}

func mountedParentAndChildZones(t *testing.T, cfg *config.Config, call mountedCall, parentFirst bool) {
	t.Helper()
	const parentName, childName = "probe.family.example.", "probe.child.family.example."
	const parentAddress, childAddress = "192.0.2.61", "192.0.2.62"
	zone := func(name, apex string) *public.DNSZone {
		return &public.DNSZone{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSZone"}, ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: public.DNSZoneSpec{ZoneName: apex, Type: public.ZoneTypePrimary, Nameservers: []string{"ns." + apex}, Policy: public.ZonePolicy{QueryAccess: public.AccessAllow}}}
	}
	records := func(zoneName, name, address string) *public.DNSRecordSet {
		return &public.DNSRecordSet{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSRecordSet"}, ObjectMeta: metav1.ObjectMeta{Name: zoneName, Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: zoneName}, Endpoints: []public.Endpoint{{DNSName: name, RecordType: "A", Targets: []string{address}, RecordTTL: 300}}}}
	}
	createParent := func() {
		call(http.MethodPost, "/objects/dnszones", zone("family-parent", "family.example."), http.StatusCreated, nil)
		call(http.MethodPost, "/objects/dnsrecordsets", records("family-parent", parentName, parentAddress), http.StatusCreated, nil)
	}
	createChild := func() {
		call(http.MethodPost, "/objects/dnszones", zone("family-child", "child.family.example."), http.StatusCreated, nil)
		call(http.MethodPost, "/objects/dnsrecordsets", records("family-child", childName, childAddress), http.StatusCreated, nil)
	}

	var (
		mu       sync.Mutex
		watched  = map[string]string{}
		probes   = map[string]int{}
		failures []string
		kinds    = map[string]int{}
		action   string
	)
	address := cfg.Listeners.DNS[0].Address
	probe := func(name string) {
		mu.Lock()
		want, on := watched[name]
		during := action
		mu.Unlock()
		if !on {
			return
		}
		sent := time.Now()
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeA)
		m, _, err := (&dns.Client{Timeout: 1500 * time.Millisecond}).Exchange(q, address)
		mu.Lock()
		defer mu.Unlock()
		if _, still := watched[name]; !still {
			return // deleted while the probe was in flight
		}
		probes[name]++
		got, kind := "", ""
		switch {
		case err != nil || m == nil:
			got, kind = fmt.Sprintf("no response (%v)", err), "no response"
		case m.Rcode != dns.RcodeSuccess || len(m.Answer) != 1:
			got, kind = fmt.Sprintf("%s with %d answers", dns.RcodeToString[m.Rcode], len(m.Answer)), dns.RcodeToString[m.Rcode]
		default:
			if a, ok := m.Answer[0].(*dns.A); !ok || a.A.String() != want {
				got, kind = fmt.Sprintf("wrong answer %v", m.Answer[0]), "wrong answer"
			}
		}
		if got != "" {
			kinds[name+" "+kind]++
			failures = append(failures, fmt.Sprintf("%s at %s during %q: %s", name, sent.Format("15:04:05.000"), during, got))
		}
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, name := range []string{parentName, childName} {
		wg.Go(func() {
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
				}
				wg.Go(func() { probe(name) })
			}
		})
	}
	set := func(label string) { mu.Lock(); action = label; mu.Unlock() }
	// awaitAnswer waits for the zone's first answer (awaitMountedDNS also
	// waits for readiness: never stuck) and then probes it.
	awaitAnswer := func(name, address string) {
		t.Helper()
		awaitMountedDNS(t, cfg, name, address)
		mu.Lock()
		watched[name] = address
		mu.Unlock()
	}
	writes := 0
	settle := func() { writes++; time.Sleep(1500 * time.Millisecond) }
	nameservers := func(object, ns string) {
		t.Helper()
		set("name servers of " + object)
		var z public.DNSZone
		call(http.MethodGet, "/objects/dnszones/"+object, nil, http.StatusOK, &z)
		z.Spec.Nameservers = []string{ns}
		call(http.MethodPut, "/objects/dnszones/"+object, &z, http.StatusOK, nil)
		settle()
	}

	if parentFirst {
		set("create parent")
		createParent()
		awaitAnswer(parentName, parentAddress)
		set("create child")
		createChild()
		awaitAnswer(childName, childAddress)
	} else {
		set("create child")
		createChild()
		awaitAnswer(childName, childAddress)
		set("create parent")
		createParent()
		awaitAnswer(parentName, parentAddress)
	}
	settle()
	for round := range 2 {
		nameservers("family-parent", fmt.Sprintf("ns%d.family.example.net.", round))
		nameservers("family-child", fmt.Sprintf("ns%d.child.example.net.", round))
		set("delete child")
		mu.Lock()
		delete(watched, childName)
		mu.Unlock()
		call(http.MethodDelete, "/objects/dnsrecordsets/default/family-child", nil, http.StatusNoContent, nil)
		call(http.MethodDelete, "/objects/dnszones/family-child", nil, http.StatusNoContent, nil)
		settle()
		set("create child again")
		createChild()
		awaitAnswer(childName, childAddress)
		settle()
	}
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	t.Logf("%d writes; %d probes of the parent name, %d of the child name; %d without a correct answer %v", writes, probes[parentName], probes[childName], len(failures), kinds)
	if probes[parentName] < 1000 || probes[childName] < 1000 {
		t.Fatalf("too few probes: %v", probes)
	}
	// As in TestRunServeSQLiteZonesAnswerAcrossCommits: a SERVFAIL or a
	// wrong answer is never a matter of timing; only under the race
	// detector are silent probes bounded by one per write.
	silent := kinds[parentName+" no response"] + kinds[childName+" no response"]
	if raceEnabled && silent == len(failures) && silent <= writes {
		return
	}
	if len(failures) != 0 {
		shown := failures
		if len(shown) > 20 {
			shown = shown[:20]
		}
		t.Fatalf("%d probes across %d writes got no correct answer; first:\n%s", len(failures), writes, strings.Join(shown, "\n"))
	}
}

// The same parent/child sequence on the Kubernetes driver (envtest). It has
// no ZoneLifecycle driver: its primary zones are compiled from the desired
// objects, so it never had the D404 cycle. The test holds it to the same
// contract: both orders, each zone written, the child deleted and created
// again, both zones answered throughout and never stuck.
func TestRunServeKubePrimaryParentAndChildZones(t *testing.T) {
	for _, parentFirst := range []bool{true, false} {
		t.Run(fmt.Sprint("parentFirst=", parentFirst), func(t *testing.T) {
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
			cfg.RateLimit.Enabled = false
			startStaticMount(t, cfg)
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			transport := &http.Transport{DisableKeepAlives: true}
			t.Cleanup(transport.CloseIdleConnections)
			c := &http.Client{Transport: transport, Jar: jar, Timeout: 20 * time.Second}
			base := "http://" + cfg.Listeners.API.Address + api.BasePath
			tsigMountAwaitRead(t, c, base+"/auth/status", nil)
			var session api.SessionInfo
			controlMountedRequest(t, c, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "offline-test-password-1"}, http.StatusOK, &session)
			mountedParentAndChildZones(t, cfg, pendingAwareCall(t, c, base, session.CSRF), parentFirst)
		})
	}
}
