package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRunServeSQLiteReversePTRPolicy(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var negative atomic.Bool
	var requests atomic.Int32
	started := make(chan struct{})
	server := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		a := new(dns.Msg)
		a.SetReply(q)
		switch q.Question[0].Qtype {
		case dns.TypePTR:
			requests.Add(1)
			if negative.Load() {
				a.Rcode = dns.RcodeNameError
			} else {
				a.Answer = []dns.RR{&dns.PTR{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 1}, Ptr: "phone.ptr.lan."}}
			}
		case dns.TypeA:
			a.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 0}, A: net.IPv4(192, 0, 2, 90)}}
		}
		_ = w.WriteMsg(a)
	})}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.ActivateAndServe() }()
	<-started
	t.Cleanup(func() {
		_ = server.Shutdown()
		if err := <-serverDone; err != nil {
			t.Error(err)
		}
	})
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.DefaultResponse = "nxdomain"
	cfg.Cache.MaxEntries = 0
	ptr := &cfg.ClientNames.ReversePTR
	ptr.Enabled = true
	ptr.Servers = []string{pc.LocalAddr().String()}
	ptr.Networks = []string{"127.0.0.0/8"}
	ptr.MaxAge = config.Duration(time.Second)
	ptr.NegativeTTL = config.Duration(time.Second)
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
	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "default"} }
	for _, obj := range []store.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "root"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypeForward, ZoneName: "."}},
		&public.ClientGroup{ObjectMeta: meta("ptr"), Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{NameGlob: "*.ptr.lan"}}}},
		&public.BlockList{ObjectMeta: meta("blocked"), Spec: public.BlockListSpec{Sources: []public.ListSource{{Inline: []string{"blocked.example"}, Format: "domains"}}}},
		&public.Policy{ObjectMeta: meta("ptr"), Spec: public.PolicySpec{Priority: 10, ClientGroupRefs: []string{"ptr"}, BlockListRefs: []string{"blocked"}}},
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
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(15 * time.Second):
			t.Error("mounted PTR worker did not join")
		}
	})
	until := time.Now().Add(30 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		if time.Now().After(until) {
			t.Fatal("mount not ready")
		}
		time.Sleep(25 * time.Millisecond)
	}
	waitMountedDomainDNS(t, cfg, true)
	if requests.Load() == 0 {
		t.Fatal("mounted identification did not acquire reverse DNS")
	}
	negative.Store(true)
	waitMountedDomainDNS(t, cfg, false)
	if requests.Load() < 2 {
		t.Fatal("expired identity did not reacquire")
	}
}

func TestReversePTRDisabledMount(t *testing.T) {
	stage := identify.New(nil, nil)
	run, err := mountReversePTR(config.ReversePTR{}, stage)
	if err != nil || run != nil || stage.FallbackHostnames != nil {
		t.Fatal("disabled PTR acquired a lifecycle")
	}
}
