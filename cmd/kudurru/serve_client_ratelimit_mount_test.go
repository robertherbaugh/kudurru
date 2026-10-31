package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/proxyproto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRunServeClientGroupRateLimitExemptNativeTransports(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	upstream := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		answer := new(dns.Msg)
		answer.SetReply(q)
		answer.RecursionAvailable = true
		answer.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 0}, A: net.IPv4(192, 0, 2, 81)}}
		_ = w.WriteMsg(answer)
	})}
	go func() { _ = upstream.ActivateAndServe() }()
	<-started
	defer func() { _ = upstream.Shutdown() }()
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.Enabled, cfg.DNSSEC.Enabled = false, false
	cfg.Cache.MaxEntries = 0
	cfg.RateLimit.Enabled, cfg.RateLimit.Slip = true, 100
	cfg.RateLimit.IPv4 = map[string]int{"32": 2, "24": 2}
	cfg.RateLimit.Exempt = []string{"198.51.100.0/24"}
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{"controlled=" + pc.LocalAddr().String()}, Strategy: "strict", Timeout: config.Duration(time.Second)}}
	secret := bytes.Repeat([]byte{0x74}, 32)
	cfg.Listeners.ProxyProtocolFrom = []string{"127.0.0.1/32"}
	cfg.Listeners.ProxyProtocolSecretFile = filepath.Join(t.TempDir(), "proxy-secret")
	if err := os.WriteFile(cfg.Listeners.ProxyProtocolSecretFile, secret, 0o600); err != nil {
		t.Fatal("write fixture proxy secret")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("client rate-limit mount shutdown failed", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("client rate-limit mount shutdown did not join")
		}
	}()
	deadline := time.Now().Add(20 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("client rate-limit mount never ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	var login api.SessionInfo
	controlMountedRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
	path := "/objects/clientgroups/default/busy"
	group := &public.ClientGroup{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "ClientGroup"}, ObjectMeta: metav1.ObjectMeta{Name: "busy", Namespace: "default"}, Spec: public.ClientGroupSpec{RateLimitExempt: true, Clients: []public.ClientMatcher{{IP: "203.0.113.7"}}}}
	var stored public.ClientGroup
	controlMountedRequest(t, client, base, http.MethodPut, path, login.CSRF, group, http.StatusCreated, &stored)
	if !stored.Spec.RateLimitExempt || stored.UID == "" {
		t.Fatal("mounted CRD/command pruned exemption or lost identity")
	}
	awaitGroup := func(exempt bool) {
		t.Helper()
		until := time.Now().Add(5 * time.Second)
		for {
			stored = public.ClientGroup{} // Absent optional false fields must clear the previous observation.
			mountedFactorRequest(t, client, base, http.MethodGet, path, login.CSRF, nil, http.StatusOK, &stored)
			if stored.Spec.RateLimitExempt == exempt {
				return
			}
			if time.Now().After(until) {
				t.Fatal("canonical group exemption never published")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	awaitGroup(true)
	exchange := func(source string, tcp, authenticate bool) (*dns.Msg, error) {
		t.Helper()
		network, transport := "udp", proxyproto.TransportDatagram
		if tcp {
			network, transport = "tcp", proxyproto.TransportStream
		}
		raw, err := net.DialTimeout(network, cfg.Listeners.DNS[0].Address, time.Second)
		if err != nil {
			return nil, err
		}
		defer func() { _ = raw.Close() }()
		_ = raw.SetDeadline(time.Now().Add(500 * time.Millisecond))
		token := secret
		if !authenticate {
			token = bytes.Repeat([]byte{0x75}, 32)
		}
		h := proxyproto.Header{Command: proxyproto.CommandProxy, Family: proxyproto.FamilyINET, Transport: transport, Source: netip.AddrPortFrom(netip.MustParseAddr(source), 5555), Destination: netip.MustParseAddrPort(cfg.Listeners.DNS[0].Address), RawTLVs: proxyproto.AppendTLV(nil, proxyproto.TLVTypeSecret, token)}
		header, err := h.Append(nil)
		if err != nil {
			return nil, err
		}
		q := new(dns.Msg)
		q.SetQuestion("ratelimit-operator.example.org.", dns.TypeA)
		if tcp {
			if _, err := raw.Write(header); err != nil {
				return nil, err
			}
			co := &dns.Conn{Conn: raw}
			if err := co.WriteMsg(q); err != nil {
				return nil, err
			}
			return co.ReadMsg()
		}
		wire, err := q.Pack()
		if err != nil {
			return nil, err
		}
		if _, err := raw.Write(append(header, wire...)); err != nil {
			return nil, err
		}
		buf := make([]byte, 4096)
		n, err := raw.Read(buf)
		if err != nil {
			return nil, err
		}
		answer := new(dns.Msg)
		return answer, answer.Unpack(buf[:n])
	}
	answer := func(source string, tcp bool, wantSlip bool) {
		t.Helper()
		r, err := exchange(source, tcp, true)
		if err != nil || r == nil || r.Rcode != dns.RcodeSuccess {
			t.Fatal("native client query failed", err)
		}
		if r.Truncated != wantSlip || (!wantSlip && len(r.Answer) != 1) || (wantSlip && len(r.Answer) != 0) {
			t.Fatal("wrong native rate-limit decision")
		}
	}
	for range 20 {
		answer("203.0.113.7", false, false)
	}
	// Exempt clients do not consume the shared /24 budget. A non-exempt
	// neighbor gets exactly two queries, then a truncated response.
	answer("203.0.113.8", false, false)
	answer("203.0.113.8", false, false)
	answer("203.0.113.8", false, true)
	for range 4 {
		answer("203.0.113.8", true, false)
		answer("198.51.100.1", false, false)
	}
	if _, err := exchange("203.0.113.7", false, false); err == nil {
		t.Fatal("unauthenticated proxy header granted client identity")
	}
	// Each mutation is one current command attempt. Observe fresh reads only;
	// production never repairs a refused request with a successor proof.
	group.Spec.RateLimitExempt = false
	controlMountedRequest(t, client, base, http.MethodPut, path, login.CSRF, group, http.StatusOK, &stored)
	awaitGroup(false)
	answer("203.0.113.7", false, true)
	group.Spec.RateLimitExempt = true
	controlMountedRequest(t, client, base, http.MethodPut, path, login.CSRF, group, http.StatusOK, &stored)
	awaitGroup(true)
	answer("203.0.113.7", false, false)
	answer("203.0.113.8", false, true)
	bad := map[string]any{"apiVersion": public.GroupVersion.String(), "kind": "ClientGroup", "metadata": map[string]any{"name": "busy", "namespace": "default"}, "spec": map[string]any{"rateLimitExempt": "true"}}
	controlMountedRequest(t, client, base, http.MethodPut, path, login.CSRF, bad, http.StatusBadRequest, nil)
	answer("203.0.113.7", false, false)
}
