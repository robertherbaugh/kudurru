package main

import (
	"bufio"
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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
)

// TestRunServeKubeQuerylogSinksSettingsFileReload is the Kubernetes half of
// the mounted D371 proof. On this driver the cluster settings arrive as the
// mounted settings file (the kudurru-settings ConfigMap), and a change runs
// through the file watcher, the shared-settings refresh and the reload
// subscriber instead of a publication. The test rewrites that file under a
// real serve command on envtest and checks the same properties through real
// UDP queries, GET /queries and the metrics listener.
func TestRunServeKubeQuerylogSinksSettingsFileReload(t *testing.T) {
	doubles := useExportDoubles(t)
	recorder := doubles.add("recorder", &exportSink{})
	stalled := doubles.add("stalled", &exportSink{stall: make(chan struct{}), entered: make(chan struct{}, 1)})
	var release sync.Once
	unstall := func() { release.Do(func() { close(stalled.stall) }) }
	defer unstall()

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

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	up := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		a := new(dns.Msg)
		a.SetReply(q)
		a.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(192, 0, 2, 91)}}
		_ = w.WriteMsg(a)
	})}
	go func() { _ = up.ActivateAndServe() }()
	<-started
	defer func() { _ = up.Shutdown() }()

	// The node file carries the cluster settings the shared-settings refresh
	// reloads; the settings file carries querylog.sinks and is rewritten.
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
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{pc.LocalAddr().String()}, Strategy: "strict", Timeout: config.Duration(4 * time.Second)}}
	source, err := json.Marshal(map[string]any{
		"store": map[string]any{"driver": "kube"}, "blocking": map[string]any{"enabled": false}, "dnssec": map[string]any{"enabled": false},
		"cache": map[string]any{"maxEntries": 0}, "ratelimit": map[string]any{"enabled": false},
		"querylog":  map[string]any{"enabled": true, "exclude": map[string]any{"sudn": false}},
		"upstreams": map[string]any{"groups": map[string]any{"default": map[string]any{"servers": []string{pc.LocalAddr().String()}, "strategy": "strict", "timeout": "4s"}}},
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
	setSinks := func(sinks string) {
		t.Helper()
		next := cfg.SettingsFile + ".next"
		if err := os.WriteFile(next, []byte("querylog:\n  sinks: "+sinks+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(next, cfg.SettingsFile); err != nil {
			t.Fatal(err)
		}
	}
	setSinks("[]")
	stop := startStaticMount(t, cfg)

	wait := func(what string, fn func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !fn() {
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for " + what)
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
	metric := func(family string, labels ...string) (float64, bool) {
		t.Helper()
		response, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + cfg.Listeners.Metrics.Address + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	lines:
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, family+"{") {
				continue
			}
			for _, label := range labels {
				if !strings.Contains(line, label) {
					continue lines
				}
			}
			value, err := strconv.ParseFloat(line[strings.LastIndexByte(line, ' ')+1:], 64)
			if err != nil {
				t.Fatal("metrics line", line, err)
			}
			return value, true
		}
		return 0, false
	}

	total := 0
	query("k-before.example.")
	total++
	wait("the first row", func() bool { return rows() == total })

	// The settings file adds a sink. The watcher applies it on its own
	// schedule, so probe until the sink sees a query.
	setSinks(`[{name: recorder, type: slog, batchSize: 1}]`)
	probes := 0
	wait("the added sink to receive a query", func() bool {
		query(fmt.Sprintf("k-probe-%03d.example.", probes))
		probes++
		total++
		time.Sleep(50 * time.Millisecond)
		return len(recorder.names()) > 0
	})
	query("k1.example.")
	query("k2.example.")
	total += 2
	wait("the sink to receive the following queries", func() bool {
		got := recorder.names()
		return len(got) >= 2 && got[len(got)-1] == "k2.example."
	})
	got, ids := recorder.names(), recorder.ids()
	if !strings.HasPrefix(got[0], "k-probe-") || got[len(got)-2] != "k1.example." {
		t.Fatalf("added sink received %v: it must start with a query answered after it was added", got)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] != ids[i-1]+1 {
			t.Fatalf("sink ids %d then %d: an event was replayed, duplicated or lost", ids[i-1], ids[i])
		}
	}
	// Query-log IDs start at 1 in a new database, one per answered query.
	if last := uint64(total); ids[len(ids)-1] != last { // #nosec G115 -- a small positive test counter
		t.Fatalf("last sink id %d, want %d", ids[len(ids)-1], last)
	}

	// Replace it with a sink that stalls in its first write.
	setSinks(`[{name: stalled, type: slog, queueSize: 100, batchSize: 1}]`)
	wait("the removed sink to close", func() bool { _, closes, _ := recorder.counts(); return closes == 1 })
	delivered := len(recorder.names())
	query("k-stall-first.example.")
	total++
	select {
	case <-stalled.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the stalled sink never received its first batch")
	}
	const burst = 150
	for i := range burst {
		query(fmt.Sprintf("k-burst-%03d.example.", i))
	}
	total += burst
	wait("SQLite rows to keep committing behind a stalled sink", func() bool { return rows() == total })
	const overflow = burst - 100
	wait("the per-sink overflow counter", func() bool {
		dropped, _ := metric("kudurru_querylog_dropped_total", `sink="stalled"`, `stage="sink"`)
		return dropped == overflow
	})
	if queued, _ := metric("kudurru_sink_queue_length", `sink="stalled"`); queued != 100 {
		t.Errorf("kudurru_sink_queue_length{sink=stalled} = %v, want 100", queued)
	}
	if dropped, found := metric("kudurru_querylog_dropped_total", `stage="queue"`); found && dropped != 0 {
		t.Errorf("the collector queue dropped %v events", dropped)
	}
	if len(recorder.names()) != delivered || doubles.builds("recorder") != 1 {
		t.Errorf("the removed sink received events after it closed, or was rebuilt")
	}

	// Remove every sink; the stalled one is still inside its write.
	setSinks("[]")
	query("k-after.example.")
	total++
	wait("the last row", func() bool { return rows() == total })
	if _, closes, calls := stalled.counts(); closes != 0 || calls != 1 {
		t.Fatalf("stalled sink: closes %d calls %d", closes, calls)
	}
	unstall()
	wait("the released sink to close", func() bool { _, closes, _ := stalled.counts(); return closes == 1 })
	wait("its series to disappear", func() bool {
		_, queued := metric("kudurru_sink_queue_length", `sink="stalled"`)
		return !queued
	})
	stop()
}
