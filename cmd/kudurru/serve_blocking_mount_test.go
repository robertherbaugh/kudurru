package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The actual mounted POST must change the canonical state used by complete DNS,
// preserve its absolute deadline on restart, and acknowledge serving publication.
func TestRunServeSQLiteBlockingCommandRestart(t *testing.T) {
	upstream, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	up := &dns.Server{PacketConn: upstream, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		answer := new(dns.Msg)
		answer.SetReply(q)
		if len(q.Question) == 1 && q.Question[0].Qtype == dns.TypeA {
			answer.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 0}, A: net.IPv4(192, 0, 2, 90)}}
		}
		_ = w.WriteMsg(answer)
	})}
	upDone := make(chan error, 1)
	go func() { upDone <- up.ActivateAndServe() }()
	t.Cleanup(func() {
		_ = up.Shutdown()
		if err := <-upDone; err != nil {
			t.Error(err)
		}
	})
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.DefaultResponse = "nxdomain"
	cfg.Cache.MaxEntries = 0
	group := cfg.Upstreams.Groups["default"]
	group.Servers = []string{upstream.LocalAddr().String()}
	group.Timeout = config.Duration(time.Second)
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": group}
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st, local, _, err := openStore(t.Context(), cfg, v, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range []store.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "root"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypeForward, ZoneName: "."}},
		&public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "blocked", Namespace: "default"}, Spec: public.BlockListSpec{Sources: []public.ListSource{{Inline: []string{"blocked.example"}, Format: "domains"}}}},
		&public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}, Spec: public.PolicySpec{BlockListRefs: []string{"blocked"}}},
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
	var deadline time.Time
	for restart := range 2 {
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
					t.Error("blocking mount shutdown", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("blocking mount did not join shutdown")
			}
		}
		t.Cleanup(stop)
		until := time.Now().Add(30 * time.Second)
		for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
			select {
			case err := <-done:
				t.Fatal("blocking mount ended before ready", err)
			case <-time.After(25 * time.Millisecond):
			}
			if time.Now().After(until) {
				t.Fatal("blocking mount never became ready")
			}
		}
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		// Toggle requests below are idempotent enable/absolute-deadline values.
		// Any fixture retry is a new HTTP request, never a renewed relative timer.
		client := &contractClient{t: t, base: "http://" + cfg.Listeners.API.Address + api.BasePath, http: &http.Client{Jar: jar, Timeout: 15 * time.Second, Transport: mountedRetryTransport{}}}
		var login api.SessionInfo
		client.call(http.MethodPost, "/auth/login", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
		client.csrf = login.CSRF
		var state api.BlockingState
		if restart == 0 {
			assertMountedBlockingDNS(t, cfg, true)
			deadline = time.Now().Add(2 * time.Minute).Truncate(time.Second)
			client.call(http.MethodPost, "/dns/blocking", map[string]any{"blocking": false, "until": deadline}, http.StatusOK, &state)
		} else {
			client.call(http.MethodGet, "/dns/blocking", nil, http.StatusOK, &state)
		}
		if state.Scope != "cluster" || !state.Configured || state.Blocking || state.Until == nil || !state.Until.Equal(deadline) || state.Timer == nil || *state.Timer <= 0 {
			t.Fatal("canonical blocking deadline was not published or changed on restart")
		}
		assertMountedBlockingDNS(t, cfg, false)
		if restart == 1 {
			client.call(http.MethodPost, "/dns/blocking", map[string]any{"blocking": true}, http.StatusOK, &state)
			if !state.Blocking || state.Until != nil || state.Timer != nil {
				t.Fatal("enable did not acknowledge the published blocking state")
			}
			assertMountedBlockingDNS(t, cfg, true)
		}
		stop()
	}
}

func assertMountedBlockingDNS(t *testing.T, cfg *config.Config, blocked bool) {
	t.Helper()
	q := new(dns.Msg)
	q.SetQuestion("blocked.example.", dns.TypeA)
	answer, _, err := (&dns.Client{Timeout: time.Second}).Exchange(q, cfg.Listeners.DNS[0].Address)
	if err != nil || answer == nil {
		t.Fatal("mounted blocking DNS query failed", err)
	}
	if blocked {
		if answer.Rcode != dns.RcodeNameError || len(answer.Answer) != 0 {
			t.Fatal("canonical enabled state did not block the configured name", answer)
		}
		return
	}
	if answer.Rcode != dns.RcodeSuccess || len(answer.Answer) != 1 {
		t.Fatal("canonical disabled state did not reach the native upstream", answer)
	}
	a, ok := answer.Answer[0].(*dns.A)
	if !ok || !a.A.Equal(net.IPv4(192, 0, 2, 90)) {
		t.Fatal("disabled blocking returned an unexpected upstream answer")
	}
}
