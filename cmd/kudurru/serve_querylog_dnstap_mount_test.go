package main

import (
	"context"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/dnstap"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/dnstap/dnstaptest"
)

// TestRunServeQuerylogDnstapSinkPrivacyAndRaw is the mounted proof of the
// dnstap sink of D372. It runs the actual serve command on the complete
// SQLite runtime under querylog.privacy hide_domains_clients, adds a dnstap
// sink through the settings command and reads what real UDP queries produce
// with a Frame Streams reader on a Unix socket. Without raw every message
// carries what the local log keeps: the client 0.0.0.0, the name hidden,
// no port and no answer. After the administrator turns raw on, the messages
// are the DNS messages of the exchange with the real client address and
// port. Excluded domains are never sent, raw or not.
func TestRunServeQuerylogDnstapSinkPrivacyAndRaw(t *testing.T) {
	collector := dnstaptest.Listen(t, "unix")
	upstream := sinkUpstream(t, net.IPv4(192, 0, 2, 93))
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Cache.MaxEntries = 0
	cfg.DNSSEC.Enabled = false
	cfg.RateLimit.Enabled = false
	cfg.Querylog.Enabled = true
	cfg.Querylog.Privacy = "hide_domains_clients"
	cfg.Querylog.Exclude.SUDN = false
	cfg.Querylog.Exclude.Domains = []string{"*.skip.example"}
	cfg.Listeners.Metrics.Address = offlineFreeAddress(t, "tcp")
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{upstream}, Strategy: "strict", Timeout: config.Duration(4 * time.Second)}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error("dnstap mount shutdown", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("dnstap mount failed to join")
			}
		})
	}
	defer stop()
	wait := func(what string, fn func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !fn() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s (collector errors %v, %d frames)", what, collector.Errors(), len(collector.Frames()))
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	ready := func() bool { return offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") == http.StatusOK }
	wait("the mount to become ready", ready)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	var login api.SessionInfo
	mountedFactorRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
	patchSink := func(raw bool) {
		t.Helper()
		var result api.SettingsBody
		mountedFactorRequest(t, client, base, http.MethodPatch, "/settings", login.CSRF, map[string]any{"querylog": map[string]any{"sinks": []map[string]any{{
			"name": "collector", "type": "dnstap", "batchSize": 1, "flushInterval": "100ms",
			"dnstap": map[string]any{"target": collector.Target(), "identity": "mounted", "raw": raw},
		}}}}, http.StatusOK, &result)
		if result.Persisted == nil || !*result.Persisted || len(result.RestartRequired) != 0 {
			t.Fatal("a querylog.sinks change was not a durable hot setting")
		}
		wait("the successor publication", ready)
	}
	var lastID uint16
	query := func(name string) {
		t.Helper()
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeA)
		lastID = q.Id
		a, _, err := (&dns.Client{Net: "udp", Timeout: 5 * time.Second}).Exchange(q, cfg.Listeners.DNS[0].Address)
		if err != nil || a == nil || a.Rcode != dns.RcodeSuccess || len(a.Answer) != 1 {
			t.Fatalf("UDP query %s was not answered: %v", name, err)
		}
	}
	// unpack reads the question and the message of a frame.
	unpack := func(f dnstaptest.Frame) (string, *dns.Msg) {
		t.Helper()
		wire := f.Message.QueryMessage
		if f.Message.Type == int(dnstap.ClientResponse) {
			wire = f.Message.ResponseMessage
		}
		m := new(dns.Msg)
		if err := m.Unpack(wire); err != nil || len(m.Question) != 1 {
			t.Fatalf("frame %+v: %v", f.Message, err)
		}
		return m.Question[0].Name, m
	}
	// framesAfter waits for the query and response of the next query.
	framesAfter := func(from int) []dnstaptest.Frame {
		t.Helper()
		return collector.WaitFrames(from+2, 20*time.Second)[from:]
	}

	query("before-any-sink.example.")
	patchSink(false)
	query("a.skip.example.")
	query("visible.example.")
	frames := framesAfter(0)
	for _, f := range frames {
		name, m := unpack(f)
		if f.Identity != "mounted" || !strings.HasPrefix(f.Version, "kudurru/") {
			t.Fatalf("identity %q version %q", f.Identity, f.Version)
		}
		if name != "hidden." || f.Message.QueryAddress != netip.IPv4Unspecified() || f.Message.QueryPort != 0 || m.Id != 0 || len(m.Answer) != 0 {
			t.Fatalf("a message that is not raw shows more than hide_domains_clients keeps: %s %+v %v", name, f.Message, m)
		}
	}
	if frames[0].Message.Type != int(dnstap.ClientQuery) || frames[1].Message.Type != int(dnstap.ClientResponse) {
		t.Fatalf("types %d %d", frames[0].Message.Type, frames[1].Message.Type)
	}
	if extra := frames[0].Extra; !strings.Contains(extra, "status=forwarded") || strings.Contains(extra, "example") {
		t.Fatalf("extra %q", extra)
	}

	// The administrator turns raw on: the exchange as it was.
	patchSink(true)
	before := len(collector.Frames())
	query("b.skip.example.")
	query("raw.example.")
	frames = framesAfter(before)
	queryName, q := unpack(frames[0])
	_, a := unpack(frames[1])
	if queryName != "raw.example." || q.Id != lastID || a.Id != lastID || len(a.Answer) != 1 || a.Answer[0].(*dns.A).A.String() != "192.0.2.93" {
		t.Fatalf("raw messages: %v / %v", q, a)
	}
	if frames[0].Message.QueryAddress != netip.MustParseAddr("127.0.0.1") || frames[0].Message.QueryPort == 0 {
		t.Fatalf("raw client %v:%d", frames[0].Message.QueryAddress, frames[0].Message.QueryPort)
	}
	// Turning raw off is an ordinary change again.
	patchSink(false)
	before = len(collector.Frames())
	query("c.skip.example.")
	query("after.example.")
	frames = framesAfter(before)
	if name, _ := unpack(frames[0]); name != "hidden." || frames[0].Message.QueryPort != 0 {
		t.Fatalf("raw off still sends %s port %d", name, frames[0].Message.QueryPort)
	}
	for _, f := range collector.Frames() {
		if name, _ := unpack(f); strings.HasSuffix(name, "skip.example.") || name == "before-any-sink.example." {
			t.Fatalf("an excluded or earlier query was sent: %s", name)
		}
	}
	if errs := collector.Errors(); len(errs) > 0 {
		t.Fatal(errs)
	}
	if sent, _ := mountedMetric(t, cfg.Listeners.Metrics.Address, "kudurru_sink_sent_total", `sink="collector"`); sent < 1 {
		t.Fatalf("kudurru_sink_sent_total %v", sent)
	}
	stop()
	// Every replaced sink ended its stream with STOP and FINISH.
	wait("the streams to finish", func() bool { return collector.Finished() >= 3 })
}
