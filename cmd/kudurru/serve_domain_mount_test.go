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

// Real serve wiring must publish domain commands to native DNS and retain
// their list and policy edits across restart.
func TestRunServeSQLiteDomainCommandRestart(t *testing.T) {
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
	var entryID string
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
					t.Error("domain mount shutdown", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("domain mount did not join shutdown")
			}
		}
		t.Cleanup(stop)
		until := time.Now().Add(30 * time.Second)
		for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
			select {
			case err := <-done:
				t.Fatal("domain mount ended before ready", err)
			case <-time.After(25 * time.Millisecond):
			}
			if time.Now().After(until) {
				t.Fatal("domain mount never became ready")
			}
		}
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		// Fixture retries capture a new HTTP request after independent status
		// publications; the command never renews an original request lease.
		client := &contractClient{t: t, base: "http://" + cfg.Listeners.API.Address + api.BasePath, http: &http.Client{Jar: jar, Timeout: 15 * time.Second, Transport: mountedRetryTransport{}}}
		var login api.SessionInfo
		client.call(http.MethodPost, "/auth/login", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
		client.csrf = login.CSRF
		if restart == 0 {
			waitMountedDomainDNS(t, cfg, true)
			var out api.DomainWriteResult
			client.call(http.MethodPost, "/allowlist", map[string]any{"values": []string{"Blocked.Example."}}, http.StatusOK, &out)
			if len(out.Items) != 1 || len(out.Attached) != 1 {
				t.Fatal("mounted domain result lacks attachment", out)
			}
			entryID = out.Items[0].ID
		}
		waitMountedDomainDNS(t, cfg, false)
		if restart == 1 {
			client.call(http.MethodPatch, "/allowlist/"+entryID, map[string]any{"enabled": false}, http.StatusOK, nil)
			waitMountedDomainDNS(t, cfg, true)
			client.call(http.MethodDelete, "/allowlist/"+entryID, nil, http.StatusNoContent, nil)
			waitMountedDomainDNS(t, cfg, true)
		}
		stop()
	}
}

func waitMountedDomainDNS(t *testing.T, cfg *config.Config, blocked bool) {
	t.Helper()
	until := time.Now().Add(10 * time.Second)
	for {
		q := new(dns.Msg)
		q.SetQuestion("blocked.example.", dns.TypeA)
		answer, _, err := (&dns.Client{Timeout: time.Second}).Exchange(q, cfg.Listeners.DNS[0].Address)
		if err == nil && answer != nil {
			if blocked && answer.Rcode == dns.RcodeNameError && len(answer.Answer) == 0 {
				return
			}
			if !blocked && answer.Rcode == dns.RcodeSuccess && len(answer.Answer) == 1 && answer.Answer[0].(*dns.A).A.Equal(net.IPv4(192, 0, 2, 90)) {
				return
			}
		}
		if time.Now().After(until) {
			t.Fatal("domain command was not published to native DNS", blocked, answer, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
