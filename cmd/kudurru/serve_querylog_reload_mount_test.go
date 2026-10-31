package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

// This exercises the actual mounted settings command, complete publisher, log
// observer, UDP/TCP listeners, SQLite history, resumable SSE tail and shutdown.
func TestRunServeQuerylogReloadOriginalNativePublicationAndTail(t *testing.T) {
	type hold struct {
		entered, release chan struct{}
		once             sync.Once
	}
	holds := map[string]*hold{}
	for _, name := range []string{"held-off.example.", "held-sqlite.example.", "held-slog.example."} {
		holds[name] = &hold{entered: make(chan struct{}), release: make(chan struct{})}
	}
	unblock := func() {
		for _, h := range holds {
			h.once.Do(func() { close(h.release) })
		}
	}
	defer unblock()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	up := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		if h := holds[q.Question[0].Name]; h != nil {
			close(h.entered)
			<-h.release
		}
		a := new(dns.Msg)
		a.SetReply(q)
		a.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET}, A: net.IPv4(192, 0, 2, 90)}}
		_ = w.WriteMsg(a)
	})}
	go func() { _ = up.ActivateAndServe() }()
	<-started
	defer func() { unblock(); _ = up.Shutdown() }()
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Cache.MaxEntries = 0
	cfg.DNSSEC.Enabled = false
	cfg.Querylog.Exclude.SUDN = false
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{pc.LocalAddr().String()}, Strategy: "strict", Timeout: config.Duration(4 * time.Second)}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			unblock()
			select {
			case err := <-done:
				if err != nil {
					t.Error("querylog mount shutdown", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("querylog mount failed to join")
			}
		})
	}
	defer stop()
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
	ready := func() bool { return offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") == http.StatusOK }
	wait("querylog mount never ready", ready)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	var login api.SessionInfo
	mountedFactorRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
	patch := func(enabled bool, sink, privacy string) {
		t.Helper()
		var result api.SettingsBody
		mountedFactorRequest(t, client, base, http.MethodPatch, "/settings", login.CSRF, map[string]any{"querylog": map[string]any{"enabled": enabled, "sink": sink, "privacy": privacy}}, http.StatusOK, &result)
		if result.Persisted == nil || !*result.Persisted || len(result.RestartRequired) != 0 {
			t.Fatal("querylog change was not a durable hot setting")
		}
		wait("querylog successor unavailable", ready)
	}
	exchange := func(name, network string) error {
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeA)
		a, _, err := (&dns.Client{Net: network, Timeout: 5 * time.Second}).Exchange(q, cfg.Listeners.DNS[0].Address)
		if err != nil {
			return err
		}
		if a == nil || a.Rcode != dns.RcodeSuccess || len(a.Answer) != 1 {
			return dns.ErrRdata
		}
		return nil
	}
	query := func(name, network string) {
		t.Helper()
		if err := exchange(name, network); err != nil {
			t.Fatal("native query failed", name, err)
		}
	}
	held := func(name, network string) func() {
		t.Helper()
		answer := make(chan error, 1)
		go func() { answer <- exchange(name, network) }()
		select {
		case <-holds[name].entered:
		case <-time.After(time.Second):
			t.Fatal("native request never reached held upstream")
		}
		return func() {
			t.Helper()
			h := holds[name]
			h.once.Do(func() { close(h.release) })
			select {
			case err := <-answer:
				if err != nil {
					t.Fatal("original held query failed", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("original native query did not finish")
			}
		}
	}
	page := func() api.QueriesBody {
		t.Helper()
		var out api.QueriesBody
		mountedFactorRequest(t, client, base, http.MethodGet, "/queries?limit=20", "", nil, http.StatusOK, &out)
		return out
	}
	// Disabled startup owns an explicit false decision, even after SQLite opens.
	releaseOff := held("held-off.example.", "udp")
	patch(true, "sqlite", "full")
	releaseOff()
	query("sqlite.example.", "tcp")
	wait("first local fact missing", func() bool { return len(page().Queries) == 1 })
	first := page().Queries[0]
	if first.Domain != "sqlite.example" || first.ID != 1 {
		t.Fatal("startup-disabled request adopted enabled successor")
	}
	releaseSQLite := held("held-sqlite.example.", "udp")
	patch(true, "slog", "hide_domains")
	query("slog.example.", "tcp")
	releaseSQLite()
	wait("original SQLite fact lost after slog replacement", func() bool { return len(page().Queries) == 2 })
	releaseSlog := held("held-slog.example.", "tcp")
	patch(false, "sqlite", "nothing")
	query("disabled.example.", "udp")
	releaseSlog()
	// A held slog request emits to the original hidden tail after disabling;
	// a newly enabled SQLite request gets its own full transform and stable ID.
	patch(true, "sqlite", "full")
	query("final.example.", "tcp")
	wait("SQLite did not resume", func() bool { return len(page().Queries) == 3 })
	rows := page().Queries
	if rows[0].Domain != "final.example" || rows[0].ID != 5 || rows[1].Domain != "held-sqlite.example" || rows[1].Client.IP != "127.0.0.1" {
		t.Fatal("original sink/privacy or monotonic IDs lost")
	}
	// Reconnect through the actual mounted SSE transport; all original local
	// tail records after ID 1 survive toggles, with no replay/duplicate IDs.
	streamCtx, streamCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer streamCancel()
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, base+"/queries/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Last-Event-ID", "1")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatal("tail resume failed", response.StatusCode)
	}
	scanner := bufio.NewScanner(response.Body)
	var tail []sqlitelog.QueryRow
	for len(tail) < 4 && scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			var row sqlitelog.QueryRow
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &row) != nil {
				t.Fatal("invalid SSE row")
			}
			tail = append(tail, row)
		}
	}
	if len(tail) != 4 {
		t.Fatal("tail was lost across sink/disabled reload", scanner.Err())
	}
	for i, row := range tail {
		if row.ID != uint64(i+2) {
			t.Fatal("tail replay duplicated/reset IDs")
		}
	}
	if tail[0].Domain != "hidden" || tail[1].Domain != "held-sqlite.example" || tail[2].Domain != "hidden" || tail[3].Domain != "final.example" {
		t.Fatal("tail adopted successor privacy")
	}
	streamCancel()
	_ = response.Body.Close()
	patch(false, "slog", "full")
	var purged api.PurgeResult
	mountedFactorRequest(t, client, base, http.MethodDelete, "/querylog?before="+rows[0].TS.Format(time.RFC3339Nano), login.CSRF, nil, http.StatusOK, &purged)
	if purged.Deleted != 2 || len(page().Queries) != 1 {
		t.Fatal("inactive writer lost local reader/purge")
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
	if err != nil || len(persisted.Queries) != 1 || persisted.Queries[0].ID != 5 || persisted.Queries[0].Domain != "final.example" {
		t.Fatal("joined shutdown lost/duplicated local facts", err)
	}
	if cfg.Querylog.Enabled {
		t.Fatal("reload mutated startup config")
	}
}

func TestRunServeQuerylogFailedPrepareKeepsOldCompleteRoute(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	// A directory at the database path forces a real local open failure only
	// after startup-disabled has published. No injected builder or retrying PATCH.
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(15 * time.Second):
			t.Error("failed-prepare mount did not join")
		}
	}()
	deadline := time.Now().Add(15 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("failed-prepare mount never ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	var login api.SessionInfo
	mountedFactorRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
	path := filepath.Join(cfg.QuerylogDataDir(), sqlitelog.FileName)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	mountedFactorRequest(t, client, base, http.MethodPatch, "/settings", login.CSRF, map[string]any{"querylog": map[string]any{"enabled": true}}, http.StatusOK, nil)
	deadline = time.Now().Add(5 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusServiceUnavailable {
		if time.Now().After(deadline) {
			t.Fatal("failed sink preparation did not surface unavailable")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Last-good DNS still answers, using its old disabled logging owner.
	q := new(dns.Msg)
	q.SetQuestion("health.kudurru.internal.", dns.TypeTXT)
	if a, _, err := (&dns.Client{Timeout: time.Second}).Exchange(q, cfg.Listeners.DNS[0].Address); err != nil || a == nil || a.Rcode != dns.RcodeSuccess {
		t.Fatal("failed candidate displaced coherent old DNS route", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("local preparation never recovered without mutation replay")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var settings api.SettingsBody
	mountedFactorRequest(t, client, base, http.MethodGet, "/settings", "", nil, http.StatusOK, &settings)
	if settings.Settings["querylog"].(map[string]any)["enabled"] != true {
		t.Fatal("recovery lost accepted setting")
	}
}
