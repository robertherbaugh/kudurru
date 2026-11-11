package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/sinktest"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

// TestRunServeKubeQuerylogSinksReadSecretCredentials is the Kubernetes half
// of the mounted proof of the HTTP sinks (D371). On this driver the cluster
// settings arrive as the mounted settings file, and the credentials of a
// sink are keys of a Secret in the namespace the server runs in. The test
// runs the real serve command on envtest: the settings file names a
// ClickHouse sink (TLS, password and CA from a Secret) and a Loki sink
// (bearer token from a Secret); real UDP queries are answered and exported;
// the Secret is then rotated through the API server, and a sink that names
// a Secret nobody marked for the sinks sends nothing.
func TestRunServeKubeQuerylogSinksReadSecretCredentials(t *testing.T) {
	warehouse := sinktest.New(t, http.StatusOK, true)
	logs := sinktest.New(t, http.StatusNoContent, false)
	orphan := sinktest.New(t, http.StatusNoContent, false)
	var mu sync.Mutex
	password := "kube-clickhouse-password"
	warehouse.Answer(func(_ int, r sinktest.Request) sinktest.Answer {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != sinktest.Basic("kudurru", password) {
			return sinktest.Answer{Status: http.StatusForbidden, Header: http.Header{"X-ClickHouse-Exception-Code": {"516"}}}
		}
		return sinktest.Answer{}
	})
	logs.Answer(func(_ int, r sinktest.Request) sinktest.Answer {
		if r.Header.Get("Authorization") != "Bearer kube-loki-token" {
			return sinktest.Answer{Status: http.StatusUnauthorized}
		}
		return sinktest.Answer{}
	})

	control := &envtest.Environment{BinaryAssetsDirectory: mountedKubeTestAssets(t), CRDDirectoryPaths: []string{"../../deploy/kustomize/crd/bases"}, ErrorIfCRDPathMissing: true}
	control.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1").Set("endpoint-reconciler-type", "none")
	t.Cleanup(func() {
		if err := control.Stop(); err != nil {
			t.Error(err)
		}
	})
	restConfig, err := control.Start()
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	var namespace corev1.Namespace
	if err := admin.Get(t.Context(), client.ObjectKey{Name: "kube-system"}, &namespace); err != nil {
		t.Fatal(err)
	}
	// The Secret lives in the namespace the server runs in. A second Secret
	// of the same name in another namespace holds other values: it must
	// never be read, because the settings cannot name a namespace.
	release := kube.PodNamespace()
	export := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: release, Name: "query-log-export", Labels: map[string]string{querylogSinkSecretLabel: "true"}}, Data: map[string][]byte{
		"clickhouse-password": []byte("kube-clickhouse-password\n"), "ca.crt": warehouse.CertPEM(), "loki-token": []byte("kube-loki-token"),
	}}
	if err := admin.Create(t.Context(), export); err != nil {
		t.Fatal(err)
	}
	// A Secret of the server's own namespace that nobody marked for the
	// sinks, as the server's keys and certificates are. The settings name
	// it; it must not leave the cluster.
	private := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: release, Name: "server-private"}, Data: map[string][]byte{"token": []byte("private-token-of-the-server")}}
	if err := admin.Create(t.Context(), private); err != nil {
		t.Fatal(err)
	}
	const other = "another-tenant"
	if release == other {
		t.Fatal("the test needs a namespace other than the server's")
	}
	if err := admin.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: other}}); err != nil {
		t.Fatal(err)
	}
	elsewhere := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: other, Name: "query-log-export"}, Data: map[string][]byte{
		"clickhouse-password": []byte("password-of-another-namespace"), "ca.crt": []byte("not a certificate"), "loki-token": []byte("token-of-another-namespace"),
	}}
	if err := admin.Create(t.Context(), elsewhere); err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(restConfig.Host)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	if proxy.Transport, err = rest.TransportFor(restConfig); err != nil {
		t.Fatal(err)
	}
	gate := httptest.NewServer(proxy)
	t.Cleanup(gate.Close)

	upstream := sinkUpstream(t, net.IPv4(192, 0, 2, 93))
	cfg := offlineServeConfig(t, gate.URL)
	cfg.Store.Kube.ExpectedClusterID = string(namespace.UID)
	cfg.Store.Kube.StartupTimeout = config.Duration(5 * time.Second)
	cfg.Listeners.Metrics.Address = offlineFreeAddress(t, "tcp")
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0
	cfg.RateLimit.Enabled = false
	cfg.Querylog.Enabled = true
	cfg.Querylog.Exclude.SUDN = false
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{upstream}, Strategy: "strict", Timeout: config.Duration(4 * time.Second)}}
	source, err := json.Marshal(map[string]any{
		"store": map[string]any{"driver": "kube"}, "blocking": map[string]any{"enabled": false}, "dnssec": map[string]any{"enabled": false},
		"cache": map[string]any{"maxEntries": 0}, "ratelimit": map[string]any{"enabled": false},
		"querylog":  map[string]any{"enabled": true, "exclude": map[string]any{"sudn": false}},
		"upstreams": map[string]any{"groups": map[string]any{"default": map[string]any{"servers": []string{upstream}, "strategy": "strict", "timeout": "4s"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg.Source = filepath.Join(dir, "kudurru.json")
	if err := os.WriteFile(cfg.Source, source, 0o600); err != nil {
		t.Fatal(err)
	}
	settingsDir := filepath.Join(dir, "settings")
	if err := os.Mkdir(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.SettingsFile = filepath.Join(settingsDir, "settings.yaml")
	t.Setenv(config.EnvSettingsFile, cfg.SettingsFile)
	// Replace the file as a ConfigMap update does: a rename into place.
	setSinks := func(sinks []map[string]any) {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"querylog": map[string]any{"sinks": sinks}})
		if err != nil {
			t.Fatal(err)
		}
		next := cfg.SettingsFile + ".next"
		if err := os.WriteFile(next, raw, 0o600); err != nil { // JSON is YAML
			t.Fatal(err)
		}
		if err := os.Rename(next, cfg.SettingsFile); err != nil {
			t.Fatal(err)
		}
	}
	secret := func(key string) map[string]any {
		return map[string]any{"secretRef": map[string]any{"name": "query-log-export", "key": key}}
	}
	setSinks([]map[string]any{})
	stop := startStaticMount(t, cfg)

	wait := func(what string, fn func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !fn() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s\nClickHouse double:\n%sLoki double:\n%s", what, sinktest.Describe(warehouse.Requests()), sinktest.Describe(logs.Requests()))
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	query := func(name string) {
		t.Helper()
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeA)
		a, _, err := (&dns.Client{Net: "udp", Timeout: 5 * time.Second}).Exchange(q, cfg.Listeners.DNS[0].Address)
		if err != nil || a == nil || a.Rcode != dns.RcodeSuccess || len(a.Answer) != 1 {
			t.Fatalf("UDP query %s was not answered: %v", name, err)
		}
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	wait("the bootstrap administrator", func() bool {
		body, err := json.Marshal(map[string]string{"username": "admin", "password": "offline-test-password-1"})
		if err != nil {
			t.Fatal(err)
		}
		response, err := httpClient.Post(base+"/auth/login", "application/json", bytes.NewReader(body))
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		return response.StatusCode == http.StatusOK
	})
	rows := func() int {
		t.Helper()
		var out api.QueriesBody
		mountedFactorRequest(t, httpClient, base, http.MethodGet, "/queries?limit=1", "", nil, http.StatusOK, &out)
		return int(out.RecordsTotal)
	}
	metric := func(family string, labels ...string) float64 {
		t.Helper()
		value, _ := mountedMetric(t, cfg.Listeners.Metrics.Address, family, labels...)
		return value
	}
	sent := func(sink string) float64 { return metric("kudurru_sink_sent_total", `sink="`+sink+`"`) }

	total := 0
	query("k-before.example.")
	total++
	wait("the first row", func() bool { return rows() == total })

	// The settings file adds the sinks; their credentials are Secret keys.
	setSinks([]map[string]any{
		{"name": "warehouse", "type": "clickhouse", "batchSize": 1, "attempts": 2, "retryBackoff": "100ms", "clickhouse": map[string]any{
			"url": warehouse.URL, "username": "kudurru", "passwordRef": secret("clickhouse-password"), "caRef": secret("ca.crt"),
		}},
		{"name": "logs", "type": "loki", "batchSize": 1, "loki": map[string]any{"url": logs.URL, "bearerTokenRef": secret("loki-token")}},
		{"name": "orphan", "type": "loki", "batchSize": 1, "retryBackoff": "100ms", "loki": map[string]any{
			"url": orphan.URL, "bearerTokenRef": map[string]any{"secretRef": map[string]any{"name": "server-private", "key": "token"}},
		}},
	})
	// The watcher applies the file on its own schedule, so probe until both
	// sinks have delivered a query.
	probes := 0
	wait("both sinks to deliver a query with the Secret's credentials", func() bool {
		query(fmt.Sprintf("k-probe-%03d.example.", probes))
		probes++
		total++
		time.Sleep(50 * time.Millisecond)
		return sent("warehouse") >= 1 && sent("logs") >= 1
	})
	first := warehouse.Requests()[0]
	if first.Header.Get("Authorization") != sinktest.Basic("kudurru", "kube-clickhouse-password") {
		t.Fatal("the ClickHouse sink did not present the password of the Secret in the server's namespace")
	}
	if first.Query.Get("query") != "INSERT INTO `kudurru`.`queries` FORMAT JSONEachRow" {
		t.Fatalf("statement with the default table: %v", first.Query)
	}
	row := sinktest.ClickHouseRows(t, first)[0]
	if !strings.HasPrefix(row["domain"].(string), "k-probe-") || row["node"] != cfg.Node.Name || row["status"] != "forwarded" {
		t.Fatalf("ClickHouse row: %v", row)
	}
	stream := sinktest.LokiStreams(t, logs.Requests()[0])[0]
	if stream.Labels["job"] != "kudurru" || stream.Labels["node"] != cfg.Node.Name || !strings.HasPrefix(stream.Entries[0].Metadata["domain"], "k-probe-") {
		t.Fatalf("Loki stream: %+v", stream)
	}
	for _, r := range append(warehouse.Requests(), logs.Requests()...) {
		if auth := r.Header.Get("Authorization"); strings.Contains(auth, "another-namespace") || auth == sinktest.Basic("kudurru", "password-of-another-namespace") {
			t.Fatal("a Secret of another namespace was read")
		}
	}
	if failed := metric("kudurru_sink_errors_total", `sink="warehouse"`); failed != 0 {
		t.Fatalf("kudurru_sink_errors_total{sink=warehouse} = %v before the rotation", failed)
	}

	// The sink whose Secret is not marked for the sinks never opened: it
	// contacts nothing, counts its attempts, and costs nobody else anything.
	wait("the refused Secret to be counted", func() bool { return metric("kudurru_sink_errors_total", `sink="orphan"`) >= 2 })
	if len(orphan.Requests()) != 0 || sent("orphan") != 0 {
		t.Fatal("a sink sent a batch without the credential its settings name")
	}

	// Rotate the password in the Secret. ClickHouse refuses the old one;
	// the sink reads the Secret again and sends the refused batch with the
	// new value. No settings change and no restart are involved.
	delivered := sent("warehouse")
	mu.Lock()
	password = "kube-rotated-password" //nolint:gosec // G101: a made-up password for the rotation
	mu.Unlock()
	export.Data["clickhouse-password"] = []byte("kube-rotated-password")
	if err := admin.Update(t.Context(), export); err != nil {
		t.Fatal(err)
	}
	before := len(warehouse.Requests())
	query("k-after-rotation.example.")
	total++
	wait("the batch after the rotation", func() bool { return sent("warehouse") == delivered+1 })
	after := warehouse.Requests()[before:]
	if len(after) != 2 || after[0].Header.Get("Authorization") != sinktest.Basic("kudurru", "kube-clickhouse-password") ||
		after[1].Header.Get("Authorization") != sinktest.Basic("kudurru", "kube-rotated-password") || string(after[0].Body) != string(after[1].Body) {
		t.Fatalf("%d requests after the rotation; the refused batch was not repeated with the Secret's new value", len(after))
	}
	if dropped := metric("kudurru_querylog_dropped_total", `sink="warehouse"`, `stage="sink"`); dropped != 0 {
		t.Fatalf("the rotation cost %v queries", dropped)
	}
	if failed := metric("kudurru_sink_errors_total", `sink="warehouse"`); failed != 1 {
		t.Fatalf("kudurru_sink_errors_total{sink=warehouse} = %v, want the one refused attempt", failed)
	}
	wait("the local rows", func() bool { return rows() == total })

	// Remove the sinks; the orphan stops trying.
	setSinks([]map[string]any{})
	wait("the series of the removed sinks to disappear", func() bool {
		_, warehouseSeries := mountedMetric(t, cfg.Listeners.Metrics.Address, "kudurru_sink_sent_total", `sink="warehouse"`)
		_, orphanSeries := mountedMetric(t, cfg.Listeners.Metrics.Address, "kudurru_sink_errors_total", `sink="orphan"`)
		return !warehouseSeries && !orphanSeries
	})
	if len(orphan.Requests()) != 0 {
		t.Fatal("the sink with a refused Secret sent a batch when it was removed")
	}
	query("k-after-removal.example.")
	total++
	wait("the last row", func() bool { return rows() == total })
	stop()
}
