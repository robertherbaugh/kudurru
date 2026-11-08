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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

func TestRunServeKubeBlockingTwoReplicasRestartAndOutage(t *testing.T) {
	control := &envtest.Environment{BinaryAssetsDirectory: mountedKubeTestAssets(t), CRDDirectoryPaths: []string{filepath.Join("..", "..", "deploy", "kustomize", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	control.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1").Set("endpoint-reconciler-type", "none")
	restConfig, err := control.Start()
	if err != nil {
		t.Fatal("blocking control plane startup", err)
	}
	t.Cleanup(func() {
		if err := control.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := public.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	var ns corev1.Namespace
	if err := admin.Get(t.Context(), client.ObjectKey{Name: "kube-system"}, &ns); err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(restConfig.Host)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport, err = rest.TransportFor(restConfig)
	if err != nil {
		t.Fatal(err)
	}
	var unavailable atomic.Bool
	var writes atomic.Int32
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/configmaps") {
			if r.Method == http.MethodPut || r.Method == http.MethodPost {
				writes.Add(1)
			}
			if unavailable.Load() && strings.HasSuffix(r.URL.Path, "/"+kube.BlockingRuntimeName) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"ServiceUnavailable","code":503}`)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(gate.Close)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	upstream := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		answer := new(dns.Msg)
		answer.SetReply(q)
		answer.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET}, A: net.IPv4(192, 0, 2, 90)}}
		_ = w.WriteMsg(answer)
	})}
	upDone := make(chan error, 1)
	go func() { upDone <- upstream.ActivateAndServe() }()
	<-started
	t.Cleanup(func() {
		_ = upstream.Shutdown()
		if err := <-upDone; err != nil {
			t.Error(err)
		}
	})
	configs := []*config.Config{offlineServeConfig(t, gate.URL), offlineServeConfig(t, gate.URL)}
	for i, cfg := range configs {
		cfg.Node.Name = []string{"blocking-a", "blocking-b"}[i]
		cfg.Store.Kube.ExpectedClusterID = string(ns.UID)
		cfg.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
		cfg.Cache.MaxEntries = 0
		cfg.DNSSEC.Enabled = false
		cfg.Blocking.DefaultResponse = "nxdomain"
		cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{pc.LocalAddr().String()}, Strategy: "strict", Timeout: config.Duration(time.Second)}}
		// Kubernetes settings initialization rereads the actual host source. Keep
		// this fixture's upstream and cache policy in that source, not only in
		// a manually edited Config which startup correctly replaces.
		source, err := json.Marshal(map[string]any{"upstreams": cfg.Upstreams, "cache": map[string]any{"maxEntries": 0}, "dnssec": map[string]any{"enabled": false}, "blocking": map[string]any{"defaultResponse": "nxdomain", "initStrategy": "blocking", "allowDegraded": false}})
		if err != nil {
			t.Fatal(err)
		}
		cfg.Source = filepath.Join(t.TempDir(), "kudurru.json")
		if err := os.WriteFile(cfg.Source, source, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	wire, _ := tokens.Generate()
	t.Setenv(tokens.EnvBootstrapToken, wire)
	t.Setenv(tokens.EnvBootstrapTokenFile, "")
	for _, obj := range []client.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "root"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypeForward, ZoneName: ".", Forward: &public.ForwardSpec{UpstreamGroup: "default"}}},
		&public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "blocked", Namespace: "default"}, Spec: public.BlockListSpec{Sources: []public.ListSource{{Inline: []string{"blocked.example"}, Format: "domains"}}}},
		&public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}, Spec: public.PolicySpec{BlockListRefs: []string{"blocked"}, BlockMode: "nxdomain"}},
	} {
		if err := admin.Create(t.Context(), obj); err != nil {
			t.Fatal(err)
		}
	}
	_ = startStaticMount(t, configs[0])
	stopB := startStaticMount(t, configs[1])
	hc := &http.Client{Timeout: 5 * time.Second}
	request := func(i int, method string, body any) (api.BlockingState, int) {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		r, err := http.NewRequestWithContext(ctx, method, "http://"+configs[i].Listeners.API.Address+api.BasePath+"/dns/blocking", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		r.GetBody = nil
		r.Header.Set("Authorization", "Bearer "+wire)
		if method == http.MethodPost {
			r.Header.Set("Content-Type", "application/json")
		}
		response, err := hc.Do(r)
		if err != nil {
			t.Fatal("blocking API exchange", err)
		}
		defer func() { _ = response.Body.Close() }()
		var state api.BlockingState
		if response.StatusCode == http.StatusOK {
			if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&state); err != nil {
				t.Fatal(err)
			}
		}
		return state, response.StatusCode
	}
	await := func(i int, blocking bool, until *time.Time) {
		t.Helper()
		end := time.Now().Add(10 * time.Second)
		for {
			state, code := request(i, http.MethodGet, nil)
			if code == http.StatusOK && state.Scope == "cluster" && state.Blocking == blocking && (until == nil || state.Until != nil && state.Until.Equal(*until)) {
				return
			}
			if time.Now().After(end) {
				t.Fatal("replica did not observe shared blocking state", i, code, state)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	checkDNS := func(i int, blocked bool) {
		t.Helper()
		q := new(dns.Msg)
		q.SetQuestion("blocked.example.", dns.TypeA)
		answer, _, err := (&dns.Client{Timeout: time.Second}).Exchange(q, configs[i].Listeners.DNS[0].Address)
		want := dns.RcodeSuccess
		if blocked {
			want = dns.RcodeNameError
		}
		if err != nil || answer == nil || answer.Rcode != want {
			t.Fatal("actual replica DNS differs from shared toggle", i, err, answer)
		}
	}
	for i := range configs {
		await(i, true, nil)
		checkDNS(i, true)
	}
	before := writes.Load()
	changed, code := request(0, http.MethodPost, map[string]any{"blocking": false, "timer": 30})
	if code != http.StatusOK || changed.Until == nil || changed.Blocking || changed.Scope != "cluster" || writes.Load() != before+1 {
		t.Fatal("single shared disable did not commit", code, changed)
	}
	original := *changed.Until
	for i := range configs {
		await(i, false, &original)
		checkDNS(i, false)
	}
	stopB()
	_ = startStaticMount(t, configs[1])
	await(1, false, &original)
	checkDNS(1, false)
	unavailable.Store(true)
	before = writes.Load()
	_, code = request(1, http.MethodPost, map[string]any{"blocking": true})
	if code != http.StatusServiceUnavailable || writes.Load() != before {
		t.Fatal("unavailable original read allowed a mutation", code)
	}
	checkDNS(0, false)
	checkDNS(1, false)
	unavailable.Store(false)
	// Observe the original absolute expiry; neither polling nor restart renews it.
	if delay := time.Until(original); delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-t.Context().Done():
			timer.Stop()
			t.Fatal("fixture expired")
		}
	}
	for i := range configs {
		await(i, true, nil)
		checkDNS(i, true)
	}
	var saved corev1.ConfigMap
	if err := admin.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: kube.BlockingRuntimeName}, &saved); err != nil || saved.Annotations["kudurru.io/blocking-disabled-until"] != original.Format(time.RFC3339Nano) {
		t.Fatal("expiry was rewritten or renewed", err)
	}
	before = writes.Load()
	_, code = request(1, http.MethodPost, map[string]any{"blocking": true})
	if code != http.StatusOK || writes.Load() != before+1 {
		t.Fatal("second replica could not issue one shared enable", code)
	}
	for i := range configs {
		await(i, true, nil)
	}
}
