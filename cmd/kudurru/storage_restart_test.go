package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/cache/persist"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// Exercise the actual serving assembly, shutdown persistence and restore. The
// second process must use the selected databases, not silently start fresh in
// node.dataDir. No HTTP writes or retries are needed to establish this evidence.
func TestStorageRunServeRestart(t *testing.T) {
	for _, split := range []bool{false, true} {
		name := "default"
		if split {
			name = "split"
		}
		t.Run(name, func(t *testing.T) {
			pc, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int64
			started := make(chan struct{})
			upstream := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
				calls.Add(1)
				answer := new(dns.Msg)
				answer.SetReply(q)
				answer.RecursionAvailable = true
				answer.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300}, A: net.IPv4(192, 0, 2, 80)}}
				_ = w.WriteMsg(answer)
			})}
			go func() { _ = upstream.ActivateAndServe() }()
			<-started
			defer func() { _ = upstream.Shutdown() }()
			cfg, _ := mountedTestConfig(t)
			cfg.Auth.IdentityProviders = nil
			cfg.Blocking.Enabled, cfg.DNSSEC.Enabled = false, false
			cfg.Querylog.Enabled, cfg.Querylog.Sink = true, "sqlite"
			cfg.Cache.MaxEntries, cfg.Cache.Prefetch.Threshold = 1000, 0
			cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{"controlled=" + pc.LocalAddr().String()}, Strategy: "strict", Timeout: config.Duration(time.Second)}}
			if split {
				root := t.TempDir()
				cfg.Node.LocalDir, cfg.Node.QuerylogDir = filepath.Join(root, "local"), filepath.Join(root, "query")
			}
			v, err := validator.New()
			if err != nil {
				t.Fatal(err)
			}
			st, local, _, err := openStore(t.Context(), cfg, v, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			for _, obj := range []store.Object{
				&public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}},
				&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home"}, Spec: public.DNSZoneSpec{ZoneName: "example.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.example."}, Policy: public.ZonePolicy{Notify: public.NotifyNone}}},
				&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "seed", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: "seed.example.", RecordType: "A", Targets: []string{"192.0.2.42"}}}}},
			} {
				if _, err := st.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					t.Fatal(err)
				}
			}
			if err := local.Close(); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			var previousQueries int
			for range 2 {
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- runServe(ctx, cfg, "") }()
				stopped := false
				stop := func() {
					if stopped {
						return
					}
					stopped = true
					cancel()
					select {
					case err := <-done:
						if err != nil {
							t.Error("serve shutdown", err)
						}
					case <-time.After(15 * time.Second):
						t.Error("serve shutdown did not join")
					}
				}
				t.Cleanup(stop)
				deadline := time.Now().Add(20 * time.Second)
				for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
					if time.Now().After(deadline) {
						t.Fatal("storage fixture never ready")
					}
					time.Sleep(20 * time.Millisecond)
				}
				awaitMountedDNS(t, cfg, "seed.example.", "192.0.2.42")
				q := new(dns.Msg)
				q.SetQuestion("stored-cache.example.org.", dns.TypeA)
				a, _, err := (&dns.Client{Timeout: 2 * time.Second}).Exchange(q, cfg.Listeners.DNS[0].Address)
				if err != nil || a == nil || a.Rcode != dns.RcodeSuccess || len(a.Answer) != 1 || a.Answer[0].(*dns.A).A.String() != "192.0.2.80" {
					t.Fatal("persisted cache query failed", err)
				}
				if calls.Load() != 1 {
					t.Fatal("response cache was not restored from selected local.db", calls.Load())
				}
				stop()
				db := storageReadOnly(t, filepath.Join(cfg.QuerylogDataDir(), "querylog.db"))
				var count int
				if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM query`).Scan(&count); err != nil {
					t.Fatal(err)
				}
				_ = db.Close()
				if count <= previousQueries {
					t.Fatal("query log lost rows or failed to append after restart", count, previousQueries)
				}
				previousQueries = count
				db = storageReadOnly(t, filepath.Join(cfg.LocalDataDir(), "local.db"))
				var size int
				if err := db.QueryRowContext(t.Context(), `SELECT length(data) FROM blobs WHERE name = ?`, persist.BlobName).Scan(&size); err != nil || size == 0 {
					t.Fatal("selected local.db missing cache dump", err)
				}
				_ = db.Close()
			}
			var files []os.FileInfo
			for _, item := range []struct{ dir, name string }{{cfg.Node.DataDir, "state.db"}, {cfg.LocalDataDir(), "local.db"}, {cfg.QuerylogDataDir(), "querylog.db"}} {
				info, err := os.Stat(filepath.Join(item.dir, item.name))
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0o600 {
					t.Fatal("database permission changed", item.name, info.Mode().Perm())
				}
				for _, prior := range files {
					if os.SameFile(info, prior) {
						t.Fatal("database files alias")
					}
				}
				files = append(files, info)
			}
			if split {
				for _, name := range []string{"local.db", "querylog.db"} {
					if _, err := os.Stat(filepath.Join(cfg.Node.DataDir, name)); !os.IsNotExist(err) {
						t.Fatal("split storage fell back to dataDir", name, err)
					}
				}
			}
		})
	}
}

func storageReadOnly(t *testing.T, path string) *sql.DB {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
