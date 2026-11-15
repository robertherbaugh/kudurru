package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/listfetcher"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestRunServePublishedListDigestWithoutLocalBodyServesEmptyUntilTheBodyArrives
// mounts the serve command on a data directory whose BlockList status
// names a published digest while the lists directory holds no body for it
// (a lost or never filled cache), and the list URL accepts the connection
// and does not answer. The node becomes ready and answers, with the list
// served empty, instead of holding back every serving generation for the
// body (D373). Once the URL answers, the body is stored and the list
// blocks again.
func TestRunServePublishedListDigestWithoutLocalBodyServesEmptyUntilTheBodyArrives(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	answer := func() { once.Do(func() { close(release) }) }
	var hits atomic.Int64
	listHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = io.WriteString(w, "lost.example\n")
	}))
	defer listHost.Close()
	defer answer()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	up := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		a := new(dns.Msg)
		a.SetReply(q)
		a.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 1}, A: net.IPv4(192, 0, 2, 91)}}
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
	cfg.Blocking.DefaultResponse = "nxdomain"
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{pc.LocalAddr().String()}, Strategy: "strict", Timeout: config.Duration(time.Second)}}
	logger := slog.New(slog.DiscardHandler)
	// The digest the leader published for the body, computed in a cache
	// that is not the node's: the node's lists directory stays empty.
	elsewhere, err := listfetcher.OpenDiskCache(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	fetched := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
	entry, _, err := elsewhere.Put(listHost.URL, strings.NewReader("lost.example\n"), listfetcher.PutOptions{Fetched: fetched, ETag: `"v1"`, Origin: listfetcher.OriginFetch})
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
		answer()
		select {
		case err := <-done:
			if err != nil {
				t.Error("lost list body mount shutdown", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("lost list body mount failed to join")
		}
	}()
	query := func(name string, want int) bool {
		t.Helper()
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeA)
		a, _, err := (&dns.Client{Timeout: time.Second}).Exchange(q, cfg.Listeners.DNS[0].Address)
		return err == nil && a != nil && a.Rcode == want
	}
	// The same bound as the mounted test that writes while a list URL is
	// silent (serve_upgrade_test.go): the node is ready and answers within
	// it, and the list does not block while its body is missing here.
	const bound = 25 * time.Second
	startedAt := time.Now()
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK || !query("lost.example.", dns.RcodeSuccess) {
		if time.Since(startedAt) > bound {
			t.Fatalf("not ready and answering %s after start while a published list body is missing and its URL is silent (readiness %d, %d list requests)", bound, offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready"), hits.Load())
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("timing: ready and answering %s after start", time.Since(startedAt).Round(time.Millisecond))
	for deadline := time.Now().Add(bound); hits.Load() == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the fetcher never asked the list URL for the missing body")
		}
	}
	if !query("lost.example.", dns.RcodeSuccess) {
		t.Fatal("lost.example. is not answered while the list body is missing")
	}
	answer()
	released := time.Now()
	for !query("lost.example.", dns.RcodeNameError) {
		if time.Since(released) > bound {
			t.Fatalf("lost.example. is not blocked %s after its list body was served", bound)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("timing: blocked %s after the URL answered", time.Since(released).Round(time.Millisecond))
}
