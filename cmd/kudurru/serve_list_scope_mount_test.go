package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync/atomic"
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

// Single-attempt authenticated current commands must retain their exact group
// meaning through mounted persistence/schema, full publication, native sockets
// and response/regex caches, even when both groups share one policy.
func TestRunServeSharedPolicyExactListScopeNative(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var fetches atomic.Int64
	started := make(chan struct{})
	upstream := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		fetches.Add(1)
		answer := new(dns.Msg)
		answer.SetReply(q)
		answer.RecursionAvailable = true
		answer.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(192, 0, 2, 81)}}
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
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.DefaultResponse = "nxdomain"
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 256
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{"controlled=" + pc.LocalAddr().String()}, Strategy: "strict", Timeout: config.Duration(time.Second)}}
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st, local, _, err := openStore(t.Context(), cfg, v, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "default"} }
	for _, o := range []store.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "fixture"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypeForward, ZoneName: "unused.invalid."}},
		&public.ClientGroup{ObjectMeta: meta("kids"), Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{IP: "127.0.0.2"}}}},
		&public.ClientGroup{ObjectMeta: meta("adults"), Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{IP: "127.0.0.3"}}}},
		&public.BlockList{ObjectMeta: meta("base"), Spec: public.BlockListSpec{Sources: []public.ListSource{{Inline: []string{"allow.scope.example", "*.allow-wild.scope.example", `/^allow-regex[0-9]+\.scope\.example$/`}}}}},
		&public.Policy{ObjectMeta: meta("shared"), Spec: public.PolicySpec{ClientGroupRefs: []string{"kids", "adults"}, BlockListRefs: []string{"base"}}},
	} {
		if _, err := st.Apply(t.Context(), o, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("scope mount shutdown", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("scope mount did not join")
		}
	})
	until := time.Now().Add(20 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		if time.Now().After(until) {
			t.Fatal("scope mount never ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	var login api.SessionInfo
	controlMountedRequest(t, httpClient, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
	exchange := func(source, network, name string) (*dns.Msg, error) {
		dialer := &net.Dialer{Timeout: time.Second}
		if network == "tcp" {
			dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(source)}
		} else {
			dialer.LocalAddr = &net.UDPAddr{IP: net.ParseIP(source)}
		}
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeA)
		r, _, err := (&dns.Client{Net: network, Timeout: time.Second, Dialer: dialer}).ExchangeContext(t.Context(), q, cfg.Listeners.DNS[0].Address)
		return r, err
	}
	await := func(source, name string, blocked bool) {
		t.Helper()
		until := time.Now().Add(8 * time.Second)
		for {
			r, err := exchange(source, "udp", name)
			if err == nil && r != nil && ((blocked && r.Rcode == dns.RcodeNameError) || (!blocked && r.Rcode == dns.RcodeSuccess && len(r.Answer) == 1)) {
				return
			}
			if time.Now().After(until) {
				t.Fatalf("scope publication %s %s blocked=%v failed: response=%v error=%v", source, name, blocked, r, err)
			}
			time.Sleep(15 * time.Millisecond)
		}
	}
	for _, q := range []string{"allow.scope.example.", "x.allow-wild.scope.example.", "allow-regex42.scope.example."} {
		await("127.0.0.2", q, true)
		await("127.0.0.3", q, true)
	}
	for _, tc := range []struct{ path, kind, value, q string }{
		{"allowlist", "exact", "allow.scope.example", "allow.scope.example."},
		{"allowlist", "wildcard", "*.allow-wild.scope.example", "x.allow-wild.scope.example."},
		{"allowlist", "regex", `^allow-regex[0-9]+\.scope\.example$`, "allow-regex42.scope.example."},
		{"denylist", "exact", "deny.scope.example", "deny.scope.example."},
		{"denylist", "wildcard", "*.deny-wild.scope.example", "x.deny-wild.scope.example."},
		{"denylist", "regex", `^deny-regex[0-9]+\.scope\.example$`, "deny-regex42.scope.example."},
	} {
		if tc.path == "denylist" {
			await("127.0.0.2", tc.q, false)
		} // populate raw response before denying only kids
		var result api.DomainWriteResult
		controlMountedRequest(t, httpClient, base, http.MethodPost, "/"+tc.path, login.CSRF, map[string]any{"values": []string{tc.value}, "type": tc.kind, "groups": []string{"kids"}}, http.StatusOK, &result)
		if len(result.Items) != 1 || len(result.Items[0].Groups) != 1 || result.Items[0].Everyone {
			t.Fatal("current domain command lost exact scope")
		}
		kidsBlocked := tc.path == "denylist"
		await("127.0.0.2", tc.q, kidsBlocked)
		await("127.0.0.3", tc.q, !kidsBlocked)
		list := "ui-allow-kids"
		if kidsBlocked {
			list = "ui-deny-kids"
		}
		var stored public.BlockList
		mountedFactorRequest(t, httpClient, base, http.MethodGet, "/objects/blocklists/default/"+list, login.CSRF, nil, http.StatusOK, &stored)
		if len(stored.Spec.ClientGroupRefs) != 1 || stored.Spec.ClientGroupRefs[0] != "kids" {
			t.Fatal("mounted current write pruned explicit list scope")
		}
		before := fetches.Load()
		for range 3 {
			for _, network := range []string{"udp", "tcp"} {
				for _, source := range []string{"127.0.0.2", "127.0.0.3"} {
					r, err := exchange(source, network, tc.q)
					if err != nil || r == nil {
						t.Fatal("native scoped query failed", err)
					}
					blocked := (source == "127.0.0.2") == kidsBlocked
					if (blocked && r.Rcode != dns.RcodeNameError) || (!blocked && (r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1)) {
						t.Fatal("shared-policy scope leaked on native cached query", network, source, tc.q, r)
					}
				}
			}
		}
		if fetches.Load() != before {
			t.Fatal("repeated allowed queries missed shared raw response cache")
		}
	}
	// The object command uses the same generated schema: a contradictory reserved
	// name must fail before mutation, and the existing exact scope remains active.
	var bad public.BlockList
	mountedFactorRequest(t, httpClient, base, http.MethodGet, "/objects/blocklists/default/ui-deny-kids", login.CSRF, nil, http.StatusOK, &bad)
	bad.Spec.ClientGroupRefs = []string{"adults"}
	var rejected api.Error
	controlMountedRequest(t, httpClient, base, http.MethodPut, "/objects/blocklists/default/ui-deny-kids", login.CSRF, &bad, http.StatusBadRequest, &rejected)
	if rejected.Body.Key != api.KeyInvalid || !strings.Contains(rejected.Body.Message, "original group scope") {
		t.Fatal("contradictory scope did not receive structured schema rejection")
	}
	await("127.0.0.2", "deny.scope.example.", true)
	await("127.0.0.3", "deny.scope.example.", false)
}
