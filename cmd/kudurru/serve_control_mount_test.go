package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Every HTTP mutation is one attempt through a dedicated transport. Only DNS
// startup observations poll; a refused settings command is never replayed.
func controlMountedRequest(t *testing.T, client *http.Client, base, method, path, csrf string, body any, want int, out any) {
	t.Helper()
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal("control request encoding failed")
		}
	}
	req, err := http.NewRequestWithContext(t.Context(), method, base+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal("control HTTP construction failed")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set("X-Kudurru-CSRF", csrf)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal("control HTTP transport failed")
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	_, _ = io.Copy(io.Discard, response.Body)
	if err != nil || response.StatusCode != want {
		t.Fatalf("control %s %s: status=%d want=%d body_read=%t", method, path, response.StatusCode, want, err == nil)
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		t.Fatal("control HTTP response decode failed")
	}
}

// Settings commits precede background publication. Observe fresh GET cuts until
// that exact configuration is published; never replay the successful command.
func awaitControlMountedSettings(t *testing.T, client *http.Client, base string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/settings", nil)
		if err != nil {
			t.Fatal("control observation construction failed")
		}
		response, err := client.Do(req)
		if err == nil {
			data, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("control settings observation: status=%d", response.StatusCode)
			}
			if readErr == nil && response.StatusCode == http.StatusOK {
				var settings api.SettingsBody
				if json.Unmarshal(data, &settings) != nil {
					t.Fatal("control settings observation decode failed")
				}
				raw, encodeErr := json.Marshal(settings.Settings["control"])
				var saved config.Control
				if encodeErr == nil && json.Unmarshal(raw, &saved) == nil && slices.Equal(saved.BlockedQTypes, []string{"a"}) && slices.Equal(saved.AllowedNetworks, []string{"198.51.100.0/24"}) {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("committed control settings were not published")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Serving readiness permits pending operational work. Wait for the fixture's
// actual initial status and notification commits before its one-shot mutation.
// Each observation is read-only and leaves no connection across server restart.
func awaitControlMountedSource(t *testing.T, cfg *config.Config) {
	t.Helper()
	dsn := (&url.URL{Scheme: "file", Path: filepath.Join(cfg.Node.DataDir, sqlite.FileName), RawQuery: "mode=ro"}).String()
	deadline := time.Now().Add(5 * time.Second)
	for {
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatal("control fixture source observation failed")
		}
		var pending, notify, heads int
		err = db.QueryRowContext(t.Context(), `SELECT
            (SELECT count(*) FROM objects WHERE kind IN ('DNSZone','DNSRecordSet','Policy','ClientGroup','BlockList') AND COALESCE(json_extract(data,'$.status.observedGeneration'),0) != generation),
            (SELECT count(*) FROM objects WHERE kind='ZoneState' AND COALESCE(json_extract(data,'$.spec.notifySequence'),0) != 0),
            (SELECT count(*) FROM objects WHERE kind='ZoneState')`).Scan(&pending, &notify, &heads)
		closeErr := db.Close()
		if err != nil || closeErr != nil {
			t.Fatal("control fixture source read/close failed")
		}
		if pending == 0 && notify == 0 && heads == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("control fixture source pending: statuses=%d notifications=%d heads=%d", pending, notify, heads)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunServeSQLiteControlSettingsRestart(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("control upstream socket failed")
	}
	var calls atomic.Int64
	started := make(chan struct{})
	upstream := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		calls.Add(1)
		answer := new(dns.Msg)
		answer.SetReply(q)
		answer.RecursionAvailable = true
		if q.Question[0].Qtype == dns.TypeA {
			answer.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300}, A: net.IPv4(192, 0, 2, 80)}}
		}
		_ = w.WriteMsg(answer)
	})}
	go func() { _ = upstream.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = upstream.Shutdown() })
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	cfg.Response.DNS64.Prefix = "64:ff9b::/96"
	cfg.Cache.MaxEntries = 1000
	cfg.Cache.Prefetch.Threshold = 0
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{"controlled=" + pc.LocalAddr().String()}, Strategy: "strict", Timeout: config.Duration(time.Second)}}
	v, err := validator.New()
	if err != nil {
		t.Fatal("control schema failed")
	}
	st, local, _, err := openStore(t.Context(), cfg, v, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal("control store fixture failed")
	}
	for _, obj := range []store.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home"}, Spec: public.DNSZoneSpec{ZoneName: "home.example.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.home.example."}, Policy: public.ZonePolicy{Notify: public.NotifyNone}}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "seed", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: "seed.home.example.", RecordType: "A", Targets: []string{"192.0.2.42"}}}}},
	} {
		if _, err := st.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal("control local record fixture failed")
		}
	}
	if local.Close() != nil || st.Close() != nil {
		t.Fatal("control fixture close failed")
	}
	query := func(name string, typ uint16, want int) {
		t.Helper()
		q := new(dns.Msg)
		q.SetQuestion(name, typ)
		// DNS64 requires DO=false; direct A refusals also cover DO=true.
		q.SetEdns0(1232, typ != dns.TypeAAAA)
		answer, _, err := (&dns.Client{Timeout: 2 * time.Second}).ExchangeContext(t.Context(), q, cfg.Listeners.DNS[0].Address)
		if err != nil || answer == nil {
			t.Fatal("control UDP query failed")
		}
		if answer.Rcode != want || want == dns.RcodeSuccess && len(answer.Answer) != 1 {
			t.Fatalf("control UDP verdict: rcode=%d answers=%d want=%d", answer.Rcode, len(answer.Answer), want)
		}
		if want == dns.RcodeRefused {
			prohibited := false
			if opt := answer.IsEdns0(); opt != nil {
				for _, option := range opt.Option {
					if ede, ok := option.(*dns.EDNS0_EDE); ok && ede.InfoCode == dns.ExtendedErrorCodeProhibited {
						prohibited = true
					}
				}
			}
			if !prohibited || len(answer.Answer) != 0 || answer.AuthenticatedData {
				t.Fatal("control refusal lost EDE18 or leaked an answer/proof")
			}
		}
	}
	for restart := range 2 {
		transport := &http.Transport{}
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal("control cookie fixture failed")
		}
		client := &http.Client{Jar: jar, Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- runServe(ctx, cfg, "") }()
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			transport.CloseIdleConnections()
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error("control mount shutdown failed")
				}
			case <-time.After(15 * time.Second):
				t.Error("control mount shutdown did not join")
			}
		}
		t.Cleanup(stop)
		deadline := time.Now().Add(20 * time.Second)
		for {
			response, err := client.Get("http://" + cfg.Listeners.Ops.Address + "/healthz/ready")
			ready := false
			if err == nil {
				ready = response.StatusCode == http.StatusOK
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
			if ready {
				break
			}
			select {
			case err := <-done:
				stopped = true
				transport.CloseIdleConnections()
				cancel()
				t.Fatalf("control mount startup ended: error=%t", err != nil)
			case <-time.After(20 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				t.Fatal("control mount readiness deadline expired")
			}
		}
		awaitControlMountedSource(t, cfg)
		base := "http://" + cfg.Listeners.API.Address + api.BasePath
		var login api.SessionInfo
		controlMountedRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
		if restart == 0 {
			query("seed.home.example.", dns.TypeA, dns.RcodeSuccess)
			query("cached.example.org.", dns.TypeA, dns.RcodeSuccess)
			warmed := calls.Load()
			query("cached.example.org.", dns.TypeA, dns.RcodeSuccess)
			if warmed != 1 || calls.Load() != warmed {
				t.Fatal("control fixture did not warm native response cache")
			}
			var result api.SettingsBody
			controlMountedRequest(t, client, base, http.MethodPatch, "/settings", login.CSRF, map[string]any{"control": config.Control{AllowedNetworks: []string{"198.51.100.0/24"}, BlockedQTypes: []string{"a"}}}, http.StatusOK, &result)
			if result.Persisted == nil || !*result.Persisted || len(result.RestartRequired) != 0 || !slices.Contains(result.Changed, "control.blockedQTypes") || !slices.Contains(result.Changed, "control.allowedNetworks") {
				t.Fatal("control command did not acknowledge persisted hot settings")
			}
		}
		awaitControlMountedSettings(t, client, base)
		// Controls must precede local records, a warm cache, and a new upstream
		// lookup. Loopback bypasses only the network allowlist, never types.
		before := calls.Load()
		for _, name := range []string{"seed.home.example.", "cached.example.org.", "uncached.example.org."} {
			query(name, dns.TypeA, dns.RcodeRefused)
		}
		if calls.Load() != before {
			t.Fatal("refused control request reached native upstream")
		}
		// An allowed AAAA query must not synthesize from a prohibited A lookup.
		// Use a new question after restart: local.db legitimately restores raw
		// negative cache entries, which would skip the expected AAAA exchange.
		query([]string{"dns64-first.example.org.", "dns64-second.example.org."}[restart], dns.TypeAAAA, dns.RcodeRefused)
		if calls.Load() != before+1 {
			t.Fatalf("DNS64 request count mismatch: restart=%d before=%d after=%d", restart, before, calls.Load())
		}
		var schema api.List[api.SettingKey]
		controlMountedRequest(t, client, base, http.MethodGet, "/settings/_schema", login.CSRF, nil, http.StatusOK, &schema)
		found := 0
		for _, row := range schema.Items {
			if row.Path != "control.allowedNetworks" && row.Path != "control.blockedQTypes" {
				continue
			}
			found++
			if row.Scope != "cluster" || row.RestartRequired || row.HostOnly || row.Type != "string[]" || row.Items == nil {
				t.Fatal("projected control settings schema lacks hot cluster metadata")
			}
		}
		if found != 2 {
			t.Fatal("projected settings schema omitted controls")
		}
		var settings api.SettingsBody
		controlMountedRequest(t, client, base, http.MethodGet, "/settings", login.CSRF, nil, http.StatusOK, &settings)
		data, err := json.Marshal(settings.Settings["control"])
		var saved config.Control
		if err != nil || json.Unmarshal(data, &saved) != nil || !slices.Equal(saved.BlockedQTypes, []string{"a"}) || !slices.Equal(saved.AllowedNetworks, []string{"198.51.100.0/24"}) {
			t.Fatal("canonical control settings did not survive publication/restart")
		}
		if restart == 1 {
			controlMountedRequest(t, client, base, http.MethodPatch, "/settings", login.CSRF, map[string]any{"control": map[string]any{"blockedQTypes": []string{"NO-SUCH-TYPE"}}}, http.StatusBadRequest, nil)
			query("seed.home.example.", dns.TypeA, dns.RcodeRefused)
		}
		stop()
	}
}
