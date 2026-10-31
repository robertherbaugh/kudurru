//go:build licensetest

package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The throwaway license key is trusted only by explicit licensetest builds.
// Exercise the actual serving mount, block-page submission, native scoped DNS
// and restart with the public workflow's real request and manager provenance.
func TestRunServeSQLiteUnblockRequestsDNSRestart(t *testing.T) {
	key, err := os.ReadFile("../../internal/license/testdata/e2e-license.key")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(license.EnvKey, strings.TrimSpace(string(key)))
	t.Setenv(license.EnvKeyFile, "")
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	upstream := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
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
	cfg.DNSSEC.Enabled = false
	cfg.BlockPage.Enabled = true
	cfg.BlockPage.IPv4 = "127.0.0.1"
	cfg.Listeners.BlockPage.HTTP = offlineFreeAddress(t, "tcp")
	cfg.Listeners.BlockPage.HTTPS = ""
	cfg.Blocking.DefaultResponse = "nxdomain"
	cfg.Cache.MaxEntries = 1000
	cfg.Cache.Prefetch.Threshold = 0
	group := cfg.Upstreams.Groups["default"]
	group.Servers = []string{pc.LocalAddr().String()}
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
		&public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "blocked", Namespace: "default"}, Spec: public.BlockListSpec{Sources: []public.ListSource{{Name: "inline", Inline: []string{"requested.example"}, Format: "domains"}}}},
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
	var requestName string
	var expires time.Time
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
					t.Error(err)
				}
			case <-time.After(15 * time.Second):
				t.Error("unblock server did not join shutdown")
			}
		}
		t.Cleanup(stop)
		until := time.Now().Add(30 * time.Second)
		for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
			select {
			case err := <-done:
				t.Fatal("unblock server startup ended", err)
			case <-time.After(20 * time.Millisecond):
			}
			if time.Now().After(until) {
				t.Fatal("unblock server never became ready")
			}
		}
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		client := &contractClient{t: t, base: "http://" + cfg.Listeners.API.Address + api.BasePath, http: &http.Client{Jar: jar, Timeout: 10 * time.Second, Transport: mountedRetryTransport{}}}
		var login api.SessionInfo
		client.call(http.MethodPost, "/auth/login", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
		client.csrf = login.CSRF
		if restart == 0 {
			awaitMountedUnblockDNS(t, cfg, true)
			// Only the public block-page form writes the request; the admin command
			// must decide that exact current request and its bounded derived effects.
			form := url.Values{"name": {"Mounted user"}, "reason": {"needed for work"}}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+cfg.Listeners.BlockPage.HTTP+"/.kudurru/request", strings.NewReader(form.Encode()))
			if err != nil {
				t.Fatal(err)
			}
			req.Host = "requested.example"
			req.Header.Set("Origin", "http://requested.example")
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			publicClient := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			response, err := publicClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/?kudurru-request=sent" {
				t.Fatalf("public request failed %d: %s", response.StatusCode, body)
			}
			var requests api.UnblockRequestList
			for {
				client.call(http.MethodGet, "/unblockrequests", nil, http.StatusOK, &requests)
				if len(requests.Items) > 0 {
					break
				}
				if time.Now().After(until) {
					t.Fatal("submitted request did not reach admin reads")
				}
				time.Sleep(20 * time.Millisecond)
			}
			requestName = requests.Items[0].Name
			var decision api.UnblockDecisionResult
			client.call(http.MethodPost, "/unblockrequests/default/"+requestName+"/approve", api.UnblockApproval{Duration: "1h"}, http.StatusOK, &decision)
			if decision.Request.Allow == nil || !decision.Request.Allow.Added || decision.Request.Allow.ExpiresAt == nil || decision.Request.Allow.Removed != nil {
				t.Fatal("mounted timed approval missing ownership/deadline")
			}
			expires = *decision.Request.Allow.ExpiresAt
		}
		awaitMountedUnblockDNS(t, cfg, false)
		if restart == 1 {
			var current api.UnblockRequest
			client.call(http.MethodGet, "/unblockrequests/default/"+requestName, nil, http.StatusOK, &current)
			if current.Allow == nil || current.Allow.ExpiresAt == nil || !current.Allow.ExpiresAt.Equal(expires) {
				t.Fatal("restart renewed or lost approval deadline")
			}
			client.call(http.MethodPost, "/unblockrequests/default/"+requestName+"/deny", api.UnblockDenial{Note: "done"}, http.StatusOK, nil)
			awaitMountedUnblockDNS(t, cfg, true)
			client.call(http.MethodDelete, "/unblockrequests/default/"+requestName, nil, http.StatusNoContent, nil)
		}
		stop()
	}
}

// Recursive upstream answers need not carry AA. Assert the actual block/allow
// result and readiness without borrowing the primary-zone test's AA predicate.
func awaitMountedUnblockDNS(t *testing.T, cfg *config.Config, blocked bool) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		query := new(dns.Msg)
		query.SetQuestion("requested.example.", dns.TypeA)
		answer, _, err := (&dns.Client{Timeout: time.Second}).Exchange(query, cfg.Listeners.DNS[0].Address)
		matches := false
		if err == nil && answer != nil {
			if blocked {
				matches = answer.Rcode == dns.RcodeNameError && len(answer.Answer) == 0
			} else if answer.Rcode == dns.RcodeSuccess && len(answer.Answer) == 1 {
				a, ok := answer.Answer[0].(*dns.A)
				matches = ok && a.A.Equal(net.IPv4(192, 0, 2, 80))
			}
		}
		if matches && offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("mounted recursive unblock answer blocked=%v: %v %v", blocked, answer, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
