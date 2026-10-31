package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/listfetcher"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRunServeBlockingRefreshSettingsActualDownloadAndPublication(t *testing.T) {
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	var hits atomic.Int64
	var conditional atomic.Bool
	listHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			conditional.Store(r.Header.Get("If-None-Match") == `"v1"`)
			close(entered)
		}
		select {
		case <-resume:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("ETag", `"v2"`)
		_, _ = io.WriteString(w, "refreshed.example\n")
	}))
	defer listHost.Close()
	defer unblock()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	up := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		a := new(dns.Msg)
		a.SetReply(q)
		a.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET}, A: net.IPv4(192, 0, 2, 90)}}
		_ = w.WriteMsg(a)
	})}
	go func() { _ = up.ActivateAndServe() }()
	<-started
	defer func() { _ = up.Shutdown() }()
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Cache.MaxEntries = 0
	cfg.DNSSEC.Enabled = false
	cfg.Blocking.AllowLocalListURLs = true
	cfg.Blocking.InitStrategy = "blocking"
	cfg.Blocking.AllowDegraded = false
	cfg.Blocking.DefaultResponse = "nxdomain"
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{pc.LocalAddr().String()}, Strategy: "strict", Timeout: config.Duration(time.Second)}}
	logger := slog.New(slog.DiscardHandler)
	cache, err := listfetcher.OpenDiskCache(cfg.BlockingListsDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	// A real last-good body is older than the new one-minute default but much
	// younger than the startup 24-hour default. No fake scheduler clock.
	fetched := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
	entry, _, err := cache.Put(listHost.URL, strings.NewReader("previous.example\n"), listfetcher.PutOptions{Fetched: fetched, ETag: `"v1"`, Origin: listfetcher.OriginFetch})
	if err != nil {
		t.Fatal(err)
	}
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st, local, _, err := openStore(t.Context(), cfg, v, prometheus.NewRegistry(), logger)
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range []store.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "root"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypeForward, ZoneName: "."}},
		&public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "remote", Namespace: "default"}, Spec: public.BlockListSpec{Sources: []public.ListSource{{URL: listHost.URL, Format: "domains"}}}},
		&public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}, Spec: public.PolicySpec{BlockListRefs: []string{"remote"}}},
	} {
		if _, err := st.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	bl := &public.BlockList{}
	if err := st.Get(t.Context(), store.Key{Kind: "BlockList", Namespace: "default", Name: "remote"}, bl); err != nil {
		t.Fatal(err)
	}
	bl.Status.Sources = []public.SourceStatus{{Name: listHost.URL, Digest: entry.Digest, ETag: entry.ETag, LastFetched: &metav1.Time{Time: fetched}}}
	if err := st.UpdateStatus(t.Context(), bl); err != nil {
		t.Fatal(err)
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
	defer func() {
		cancel()
		unblock()
		select {
		case err := <-done:
			if err != nil {
				t.Error("blocking reload mount shutdown", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("blocking reload mount failed to join")
		}
	}()
	deadline := time.Now().Add(25 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("blocking reload mount never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	query := func(name string, want int) bool {
		t.Helper()
		for _, network := range []string{"udp", "tcp"} {
			q := new(dns.Msg)
			q.SetQuestion(name, dns.TypeA)
			a, _, err := (&dns.Client{Net: network, Timeout: time.Second}).Exchange(q, cfg.Listeners.DNS[0].Address)
			if err != nil || a == nil || a.Rcode != want {
				return false
			}
		}
		return true
	}
	if hits.Load() != 0 || !query("previous.example.", dns.RcodeNameError) || !query("refreshed.example.", dns.RcodeSuccess) {
		t.Fatal("startup body or default refresh behavior incorrect")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	var login api.SessionInfo
	mountedFactorRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
	patch := func(interval string, allow bool) {
		t.Helper()
		var result api.SettingsBody
		// Mutations are single attempts, using the normal current-command fence.
		mountedFactorRequest(t, client, base, http.MethodPatch, "/settings", login.CSRF,
			map[string]any{"blocking": map[string]any{"refreshInterval": interval, "allowDegraded": allow}}, http.StatusOK, &result)
		if len(result.RestartRequired) != 0 || result.Persisted == nil || !*result.Persisted {
			t.Fatal("blocking controls were not admitted as durable hot settings")
		}
	}
	patch("1m", true)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("shorter interval did not wake idle production fetcher")
	}
	if !conditional.Load() || !query("previous.example.", dns.RcodeNameError) {
		t.Fatal("reload lost original conditional source or old complete publication")
	}
	// While the real HTTP download is pending, lengthening and changing
	// degraded readiness must not replace/cancel it or install a partial list.
	patch("24h", false)
	if hits.Load() != 1 || !query("refreshed.example.", dns.RcodeSuccess) {
		t.Fatal("second settings update restarted a request or exposed partial body")
	}
	unblock()
	deadline = time.Now().Add(10 * time.Second)
	for !query("refreshed.example.", dns.RcodeNameError) || !query("previous.example.", dns.RcodeSuccess) {
		if time.Now().After(deadline) {
			t.Fatal("new downloaded digest never reached a complete DNS publication")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if hits.Load() != 1 || cfg.Blocking.RefreshInterval.Std() != 24*time.Hour || cfg.Blocking.AllowDegraded {
		t.Fatal("reload replaced source request or mutated startup config")
	}
}
