package main

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
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
	"github.com/robertherbaugh/kudurru/internal/store/kube"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
)

// Stored TSIG keys are managed through the API on Kubernetes and replace the
// signing/verification ring without a restart (F07a, D365).
func TestRunServeKubeStoredTSIGManagementHotRotation(t *testing.T) {
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
	for _, object := range []client.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "management"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "management.test.", Nameservers: []string{"ns.management.test."}}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "seed", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "management"}, Endpoints: []public.Endpoint{{DNSName: "seed.management.test.", RecordType: "A", Targets: []string{"192.0.2.97"}}}}},
	} {
		if err := admin.Create(t.Context(), object); err != nil {
			t.Fatal(err)
		}
	}
	ring := func(secret []byte) *dnsserver.StaticKeyring {
		keys, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "managed.", Algorithm: dns.HmacSHA256, Secret: secret})
		if err != nil {
			t.Fatal(err)
		}
		return keys
	}
	first, second := bytes.Repeat([]byte{0x31}, 32), bytes.Repeat([]byte{0x32}, 32)
	login := func() (*http.Client, string, string) {
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		transport := &http.Transport{DisableKeepAlives: true}
		t.Cleanup(transport.CloseIdleConnections)
		c := &http.Client{Transport: transport, Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		base := "http://" + cfg.Listeners.API.Address + api.BasePath
		tsigMountAwaitRead(t, c, base+"/auth/status", nil)
		var session api.SessionInfo
		controlMountedRequest(t, c, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "offline-test-password-1"}, http.StatusOK, &session)
		return c, base, session.CSRF
	}
	signedEventually := func(keys *dnsserver.StaticKeyring, rcode int) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			if probeSignedManagement(cfg, keys, rcode) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("signed query never reached the expected outcome", rcode, describeSignedManagement(cfg, keys))
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	stop := startStaticMount(t, cfg)
	awaitMountedDNS(t, cfg, "seed.management.test.", "192.0.2.97")
	signedEventually(ring(first), dns.RcodeNotAuth)
	c, base, csrf := login()
	tsigMountAwaitRead(t, c, base+"/tsigkeys", nil)
	var receipt api.TSIGKeyCommandReceipt
	controlMountedRequest(t, c, base, http.MethodPost, "/tsigkeys", csrf, map[string]any{"keyName": "managed.", "algorithm": "hmac-sha256", "secret": base64.StdEncoding.EncodeToString(first)}, http.StatusCreated, &receipt)
	if !receipt.Committed || !receipt.Published || receipt.Removed || receipt.Key.UID == "" || receipt.Key.ResourceVersion == "" {
		t.Fatal("kube create receipt absent or unpublished", receipt)
	}
	signedEventually(ring(first), dns.RcodeSuccess)
	var list api.TSIGKeyList
	tsigMountAwaitRead(t, c, base+"/tsigkeys", &list)
	stored := tsigMountMetadata(t, list, "managed.")
	if stored.UID != receipt.Key.UID || stored.ResourceVersion != receipt.Key.ResourceVersion {
		t.Fatal("metadata does not match the create receipt")
	}
	// A stale binding is refused without effect.
	controlMountedRequest(t, c, base, http.MethodPost, "/tsigkeys/managed./replace", csrf, map[string]any{"uid": stored.UID, "resourceVersion": "1", "secret": base64.StdEncoding.EncodeToString(second)}, http.StatusConflict, nil)
	controlMountedRequest(t, c, base, http.MethodPost, "/tsigkeys/managed./replace", csrf, map[string]any{"uid": stored.UID, "resourceVersion": stored.ResourceVersion, "secret": base64.StdEncoding.EncodeToString(second)}, http.StatusOK, &receipt)
	if !receipt.Committed || !receipt.Published || receipt.Key.UID != stored.UID || receipt.Key.ResourceVersion == stored.ResourceVersion {
		t.Fatal("kube replace receipt lost its binding")
	}
	signedEventually(ring(second), dns.RcodeSuccess)
	signedEventually(ring(first), dns.RcodeNotAuth)
	// Restart retains the replaced key.
	stop()
	stop = startStaticMount(t, cfg)
	awaitMountedDNS(t, cfg, "seed.management.test.", "192.0.2.97")
	signedEventually(ring(second), dns.RcodeSuccess)
	c, base, csrf = login()
	tsigMountAwaitRead(t, c, base+"/tsigkeys", &list)
	stored = tsigMountMetadata(t, list, "managed.")
	controlMountedRequest(t, c, base, http.MethodPost, "/tsigkeys/managed./revoke", csrf, map[string]any{"uid": stored.UID, "resourceVersion": stored.ResourceVersion}, http.StatusOK, &receipt)
	if !receipt.Committed || !receipt.Published || !receipt.Removed {
		t.Fatal("kube revoke receipt absent")
	}
	signedEventually(ring(second), dns.RcodeNotAuth)
	awaitMountedDNS(t, cfg, "seed.management.test.", "192.0.2.97")
	// Another writer (as another replica would) creates the key directly; this
	// replica adopts it through its periodic refresh without a restart.
	other, err := kube.New(actual, kube.Options{Scheme: scheme, InternalKinds: privateStoreKinds(), ExpectedClusterID: string(namespace.UID), StartupTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	sealer, err := tsigkeys.NewSealer(make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := tsigkeys.Read(t.Context(), other, string(namespace.UID))
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
	third := bytes.Repeat([]byte{0x33}, 32)
	plan, err := prepared.Plan(sealer, tsigkeys.MutationInput{Operation: tsigkeys.Create, KeyName: "managed.", Algorithm: "hmac-sha256", Secret: third})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.ApplyStore(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	signedEventually(ring(third), dns.RcodeSuccess)
	stop()
}

// probeSignedManagement is the non-fatal form of tsigMountSignedManagementQuery.
func probeSignedManagement(cfg *config.Config, keys *dnsserver.StaticKeyring, rcode int) bool {
	question := new(dns.Msg)
	question.SetQuestion("seed.management.test.", dns.TypeA)
	question.SetTsig("managed.", dns.HmacSHA256, 30, time.Now().Unix())
	c := &dns.Client{Net: "tcp", Timeout: 2 * time.Second}
	if rcode == dns.RcodeSuccess {
		c.TsigProvider = dnsserver.TSIGProvider(keys)
		answer, _, err := c.Exchange(question, cfg.Listeners.DNS[0].Address)
		return err == nil && answer != nil && answer.Rcode == rcode && answer.IsTsig() != nil && len(answer.Answer) == 1
	}
	wire, _, err := dns.TsigGenerateWithProvider(question, dnsserver.TSIGProvider(keys), "", false)
	if err != nil {
		return false
	}
	conn, err := dns.Dial("tcp", cfg.Listeners.DNS[0].Address)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(wire); err != nil {
		return false
	}
	answer, err := conn.ReadMsg()
	if err != nil && answer == nil {
		return false
	}
	return answer.Rcode == rcode && answer.IsTsig() != nil && (answer.IsTsig().Error == dns.RcodeBadKey || answer.IsTsig().Error == dns.RcodeBadSig)
}

func describeSignedManagement(cfg *config.Config, keys *dnsserver.StaticKeyring) string {
	question := new(dns.Msg)
	question.SetQuestion("seed.management.test.", dns.TypeA)
	question.SetTsig("managed.", dns.HmacSHA256, 30, time.Now().Unix())
	wire, _, err := dns.TsigGenerateWithProvider(question, dnsserver.TSIGProvider(keys), "", false)
	if err != nil {
		return err.Error()
	}
	conn, err := dns.Dial("tcp", cfg.Listeners.DNS[0].Address)
	if err != nil {
		return err.Error()
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(wire); err != nil {
		return err.Error()
	}
	answer, err := conn.ReadMsg()
	if answer == nil {
		return "no answer: " + err.Error()
	}
	tsig := "none"
	if rr := answer.IsTsig(); rr != nil {
		tsig = dns.RcodeToString[int(rr.Error)]
	}
	return dns.RcodeToString[answer.Rcode] + " tsig=" + tsig
}
