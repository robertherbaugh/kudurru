package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/keys"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
)

// Exercise the real runServe path, store, listeners and query pipeline. A
// black-holed API also verifies discovery cannot ignore the startup deadline.
func TestRunServeKubeOfflineSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name      string
		age       time.Duration
		snapshot  bool
		wantCode  int
		wantReady int
	}{
		{"fresh", time.Minute, true, dns.RcodeSuccess, http.StatusOK},
		{"stale", 48 * time.Hour, true, dns.RcodeServerFailure, http.StatusServiceUnavailable},
		{"first boot", 0, false, dns.RcodeServerFailure, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unavailable := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
			}))
			t.Cleanup(unavailable.Close)
			cfg := offlineServeConfig(t, unavailable.URL)
			if tc.snapshot {
				db, err := localdb.OpenSnapshotStore(t.Context(), filepath.Join(cfg.Node.DataDir, localdb.FileName))
				if err != nil {
					t.Fatal(err)
				}
				zone := &v1alpha1.DNSZone{
					ObjectMeta: metav1.ObjectMeta{Name: "home"},
					Spec:       v1alpha1.DNSZoneSpec{ZoneName: "home.example.", Type: v1alpha1.ZoneTypePrimary},
				}
				rs := &v1alpha1.DNSRecordSet{
					ObjectMeta: metav1.ObjectMeta{Name: "nas", Namespace: "apps"},
					Spec: v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: "home"},
						Endpoints: []v1alpha1.Endpoint{{DNSName: "nas.home.example.", RecordType: "A", Targets: []string{"192.0.2.42"}}}},
				}
				if err := db.Write(t.Context(), &localdb.Snapshot{Objects: []store.Object{zone, rs}, ClusterID: "offline-cluster", WrittenAt: time.Now().Add(-tc.age)}); err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- runServe(ctx, cfg, "") }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("runServe: %v", err)
					}
				case <-time.After(10 * time.Second):
					t.Error("runServe did not stop")
				}
			})
			deadline := time.Now().Add(20 * time.Second)
			client := &dns.Client{Timeout: 100 * time.Millisecond}
			question := new(dns.Msg)
			question.SetQuestion("nas.home.example.", dns.TypeA)
			for {
				answer, _, err := client.Exchange(question, cfg.Listeners.DNS[0].Address)
				ready := offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready")
				if err == nil && answer.Rcode == tc.wantCode && ready == tc.wantReady {
					if tc.wantCode == dns.RcodeSuccess && (!answer.Authoritative || len(answer.Answer) != 1 || answer.Answer[0].(*dns.A).A.String() != "192.0.2.42") {
						t.Fatalf("snapshot answer: %v", answer)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("DNS never reached expected snapshot state: answer=%v err=%v readiness=%d", answer, err, ready)
				}
				time.Sleep(20 * time.Millisecond)
			}
			// A DNS snapshot cannot prove IAM migration admission. v0.2 returns
			// unavailable after the bounded journal check, without authenticating.
			for {
				status := offlineHTTPStatusWithTimeout(cfg.Listeners.API.Address, "/api/v1/zones", 6*time.Second)
				if status == http.StatusServiceUnavailable {
					break
				}
				if status != 0 || time.Now().After(deadline) {
					t.Fatalf("anonymous management request: %d", status)
				}
				time.Sleep(20 * time.Millisecond)
			}
			if _, err := os.Stat(filepath.Join(cfg.Node.DataDir, keys.Dir, keys.FileName)); !os.IsNotExist(err) {
				t.Fatalf("kube must not generate a replacement encryption key: %v", err)
			}
			t.Logf("actual DNS startup with black-holed kube: rcode=%d readiness=%d", tc.wantCode, tc.wantReady)
		})
	}
}

func TestRunServeKubeOfflineRequiresSharedKey(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(api.Close)
	cfg := offlineServeConfig(t, api.URL)
	t.Setenv(keys.EnvKey, "")
	if err := runServe(t.Context(), cfg, ""); !errors.Is(err, keys.ErrKeyRequired) {
		t.Fatalf("offline startup must preserve the shared key requirement: %v", err)
	}
}

func offlineServeConfig(t *testing.T, apiURL string) *config.Config {
	t.Helper()
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Store.Driver = "kube"
	cfg.Node.DataDir = t.TempDir()
	cfg.Node.Lameduck = 0
	cfg.Store.Kube.StartupTimeout = config.Duration(100 * time.Millisecond)
	cfg.Listeners.DNS = []config.DNSListener{{Address: offlineFreeAddress(t, "udp"), Protocols: []string{"udp"}}}
	cfg.Listeners.Sockets = 1
	cfg.Listeners.Ops.Address = offlineFreeAddress(t, "tcp")
	cfg.Listeners.API.Address = offlineFreeAddress(t, "tcp")
	cfg.Listeners.API.TLS.Auto = config.APITLSOff
	cfg.Listeners.Metrics.Address = "127.0.0.1:0"
	cfg.Listeners.Internal.Address = "127.0.0.1:0"
	cfg.Auth.AdminSocket.Path = ""
	cfg.Querylog.Enabled = false
	cfg.Log.Level = "error"
	path := filepath.Join(t.TempDir(), "kubeconfig")
	body := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: %s\ncontexts:\n- name: test\n  context:\n    cluster: test\n    user: test\ncurrent-context: test\nusers:\n- name: test\n  user: {}\n", apiURL)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
	t.Setenv(keys.EnvKey, base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv(keys.EnvKey+"_FILE", "")
	t.Setenv(keys.EnvPreviousKey, "")
	t.Setenv(keys.EnvPreviousKey+"_FILE", "")
	t.Setenv(users.EnvAdminPassword, "offline-test-password-1")
	return cfg
}

func offlineFreeAddress(t *testing.T, network string) string {
	t.Helper()
	if network == "udp" {
		c, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		return c.LocalAddr().String()
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func offlineHTTPStatus(address, path string) int {
	return offlineHTTPStatusWithTimeout(address, path, time.Second)
}

func offlineHTTPStatusWithTimeout(address, path string, timeout time.Duration) int {
	c := &http.Client{Timeout: timeout}
	res, err := c.Get("http://" + address + path)
	if err != nil {
		return 0
	}
	defer func() { _ = res.Body.Close() }()
	return res.StatusCode
}
