package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

func TestRunServeQuerylogDisabledRestartRetainsHistoryWithoutAdmission(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	upstream := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		a := new(dns.Msg)
		a.SetReply(q)
		a.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET}, A: net.IPv4(192, 0, 2, 90)}}
		_ = w.WriteMsg(a)
	})}
	go func() { _ = upstream.ActivateAndServe() }()
	<-started
	defer func() { _ = upstream.Shutdown() }()
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Cache.MaxEntries = 0
	cfg.DNSSEC.Enabled = false
	cfg.Querylog.Enabled = true
	cfg.Querylog.Privacy = "full"
	cfg.Querylog.Exclude.SUDN = false
	cfg.Listeners.Metrics.Address = offlineFreeAddress(t, "tcp")
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{pc.LocalAddr().String()}, Strategy: "strict", Timeout: config.Duration(time.Second)}}
	wait := func(what string, fn func() bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for !fn() {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	start := func() (*http.Client, api.SessionInfo, func()) {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- runServe(ctx, cfg, "") }()
		var once sync.Once
		stop := func() {
			once.Do(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error("restart mount shutdown", err)
					}
				case <-time.After(15 * time.Second):
					t.Error("restart mount did not join")
				}
			})
		}
		t.Cleanup(stop)
		wait("restart mount never ready", func() bool { return offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") == http.StatusOK })
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
		var login api.SessionInfo
		mountedFactorRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
		return client, login, stop
	}
	query := func(name, network string) {
		t.Helper()
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeA)
		a, _, err := (&dns.Client{Net: network, Timeout: time.Second}).Exchange(q, cfg.Listeners.DNS[0].Address)
		if err != nil || a == nil || a.Rcode != dns.RcodeSuccess || len(a.Answer) != 1 {
			t.Fatal("restart native query failed", name, err)
		}
	}
	page := func(client *http.Client) api.QueriesBody {
		t.Helper()
		var out api.QueriesBody
		mountedFactorRequest(t, client, base, http.MethodGet, "/queries?limit=20", "", nil, http.StatusOK, &out)
		return out
	}
	client, login, stop := start()
	query("retained-udp.example.", "udp")
	query("retained-tcp.example.", "tcp")
	wait("seed native facts never stored", func() bool { return len(page(client).Queries) == 2 })
	var settings api.SettingsBody
	mountedFactorRequest(t, client, base, http.MethodPatch, "/settings", login.CSRF, map[string]any{"querylog": map[string]any{"enabled": false, "sink": "slog", "privacy": "hide_domains"}}, http.StatusOK, &settings)
	if settings.Persisted == nil || !*settings.Persisted || len(settings.RestartRequired) != 0 {
		t.Fatal("disabled restart settings were not durable hot settings")
	}
	stop()
	// The persisted complete settings and host startup both select disabled/slog.
	// Retained facts keep the original full transform; new traffic admits none.
	cfg.Querylog.Enabled = false
	cfg.Querylog.Sink = "slog"
	cfg.Querylog.Privacy = "hide_domains"
	client, login, stop = start()
	rows := page(client).Queries
	if len(rows) != 2 || rows[0].Domain != "retained-tcp.example" || rows[0].Proto != "tcp" || rows[1].Domain != "retained-udp.example" || rows[1].Proto != "udp" || rows[0].Client.IP != "127.0.0.1" {
		t.Fatal("disabled restart lost or retransformed original local history")
	}
	var status api.QuerylogStatusBody
	mountedFactorRequest(t, client, base, http.MethodGet, "/querylog/status", "", nil, http.StatusOK, &status)
	if status.Rows != 2 || status.Privacy != "hide_domains" {
		t.Fatal("disabled restart did not expose the current local writer")
	}
	query("disabled-udp.example.", "udp")
	query("disabled-tcp.example.", "tcp")
	// A slog route would leave SQLite unchanged even if accidentally enabled.
	// The actual admission counter proves that neither native query entered it.
	response, err := client.Get("http://" + cfg.Listeners.Metrics.Address + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(raw), "\nkudurru_querylog_events_total 0\n") {
		t.Fatal("disabled restart admitted new native events")
	}
	if len(page(client).Queries) != 2 {
		t.Fatal("disabled native queries changed retained facts")
	}
	var purged api.PurgeResult
	mountedFactorRequest(t, client, base, http.MethodDelete, "/querylog", login.CSRF, nil, http.StatusOK, &purged)
	if purged.Deleted != 2 || len(page(client).Queries) != 0 {
		t.Fatal("disabled restart history purge unavailable")
	}
	stop()
	opts, err := sqlitelog.OptionsFrom(cfg.Querylog, cfg.QuerylogDataDir(), cfg.Node.Name)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlitelog.Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	persisted, err := reopened.Reader().Queries(t.Context(), sqlitelog.QueryFilter{Limit: 20})
	if err != nil || len(persisted.Queries) != 0 {
		t.Fatal("disabled restart admitted/replayed facts after purge or shutdown", err)
	}
}
