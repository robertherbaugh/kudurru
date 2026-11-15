package main

import (
	"context"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/dnstap"
)

// TestRunServeQuerylogDnstapListen is the mounted proof of dnstap listen
// (D372). The actual serve command on the complete SQLite runtime binds
// listeners.dnstap and also runs a dnstap sink that sends to that same
// listener, the loop an operator could configure by mistake. A real UDP
// query from a fixed client port is answered and logged once: the sink's
// messages about it come back with the node's own identity and are
// dropped. A front end then reports, over Frame Streams, the same query
// (it forwarded it to kudurru) and a query it answered from its own cache:
// the first is recognized by client address, port, name, type and time and
// dropped, the second becomes one row. The ingested row is never sent by
// the sink, so nothing comes back a second time.
func TestRunServeQuerylogDnstapListen(t *testing.T) {
	upstream := sinkUpstream(t, net.IPv4(192, 0, 2, 94))
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Cache.MaxEntries = 0
	cfg.DNSSEC.Enabled = false
	cfg.RateLimit.Enabled = false
	cfg.Querylog.Enabled = true
	cfg.Querylog.Exclude.SUDN = false
	cfg.Listeners.Metrics.Address = offlineFreeAddress(t, "tcp")
	listen := offlineFreeAddress(t, "tcp")
	cfg.Listeners.Dnstap = []config.DnstapListener{{Address: "tcp:" + listen, AllowFrom: []string{"127.0.0.1"}}}
	cfg.Querylog.Sinks = []config.QuerylogSink{{Name: "loop", Type: config.QuerylogSinkDnstap, BatchSize: 1, FlushInterval: config.Duration(100 * time.Millisecond), Dnstap: &config.QuerylogDnstap{Target: "tcp:" + listen}}}
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{upstream}, Strategy: "strict", Timeout: config.Duration(4 * time.Second)}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
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
					t.Error("dnstap listen mount shutdown", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("dnstap listen mount failed to join")
			}
		})
	}
	defer stop()
	wait := func(what string, fn func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !fn() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	wait("the mount to become ready", func() bool {
		return offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") == http.StatusOK
	})
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	var login api.SessionInfo
	mountedFactorRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
	rows := func() api.QueriesBody {
		t.Helper()
		var out api.QueriesBody
		mountedFactorRequest(t, client, base, http.MethodGet, "/queries?limit=50", "", nil, http.StatusOK, &out)
		return out
	}
	frames := func(result string) float64 {
		v, _ := mountedMetric(t, cfg.Listeners.Metrics.Address, "kudurru_querylog_dnstap_frames_total", `result="`+result+`"`)
		return v
	}

	// A native query from a fixed client port.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(pc.LocalAddr().(*net.UDPAddr).Port) //nolint:gosec // G115: a port
	_ = pc.Close()
	sent := time.Now()
	q := new(dns.Msg)
	q.SetQuestion("native.example.", dns.TypeA)
	c := &dns.Client{Net: "udp", Timeout: 5 * time.Second, Dialer: &net.Dialer{LocalAddr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)}}}
	if a, _, err := c.Exchange(q, cfg.Listeners.DNS[0].Address); err != nil || a == nil || len(a.Answer) != 1 {
		t.Fatalf("UDP query was not answered: %v", err)
	}
	wait("the native row", func() bool { return len(rows().Queries) == 1 })
	// The sink's CLIENT_QUERY and CLIENT_RESPONSE come back as the node's own.
	wait("the looped messages", func() bool { return frames(dnstap.ResultSelf) == 2 })

	// The front end's messages: the forwarded query, and a cache hit.
	conn, err := net.Dial("tcp", listen)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	reader := dnstap.NewReader(conn)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	write := func(b []byte) {
		t.Helper()
		if _, err := conn.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	write(dnstap.AppendControl(nil, dnstap.ControlReady, dnstap.ContentType))
	if _, err := reader.ReadControl(dnstap.ControlAccept); err != nil {
		t.Fatal(err)
	}
	write(dnstap.AppendControl(nil, dnstap.ControlStart, dnstap.ContentType))
	send := func(name string, address netip.Addr, port uint16, at time.Time) {
		t.Helper()
		query := new(dns.Msg)
		query.SetQuestion(name, dns.TypeA)
		answer := new(dns.Msg)
		answer.SetReply(query)
		answer.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: []byte{192, 0, 2, 94}}}
		qw, _ := query.Pack()
		aw, _ := answer.Pack()
		base := dnstap.Message{SocketFamily: dnstap.FamilyINET, SocketProtocol: dnstap.ProtocolUDP, QueryAddress: address.AsSlice(), QueryPort: port, QueryTime: at}
		for _, m := range []dnstap.Message{
			{Type: dnstap.ClientQuery, QueryMessage: qw},
			{Type: dnstap.ClientResponse, ResponseTime: at.Add(time.Millisecond), ResponseMessage: aw},
		} {
			m.SocketFamily, m.SocketProtocol, m.QueryAddress, m.QueryPort, m.QueryTime = base.SocketFamily, base.SocketProtocol, base.QueryAddress, base.QueryPort, base.QueryTime
			payload, _ := dnstap.AppendFrame(nil, nil, &dnstap.Frame{Identity: []byte("edge-1"), Version: []byte("dnsdist"), Message: m})
			write(dnstap.AppendData(nil, payload))
		}
	}
	send("native.example.", netip.MustParseAddr("127.0.0.1"), port, sent)
	send("cached.example.", netip.MustParseAddr("192.0.2.50"), 41000, time.Now())
	write(dnstap.AppendControl(nil, dnstap.ControlStop))
	if _, err := reader.ReadControl(dnstap.ControlFinish); err != nil {
		t.Fatal(err)
	}
	wait("the ingested row", func() bool { return len(rows().Queries) == 2 })
	wait("the counters", func() bool { return frames(dnstap.ResultDuplicate) == 1 && frames(dnstap.ResultRecorded) == 1 })
	// Give the sink time to send the ingested row, which it must not.
	time.Sleep(time.Second)
	got := rows().Queries
	if len(got) != 2 {
		t.Fatalf("%d rows, want one per client query: %+v", len(got), got)
	}
	names := map[string]string{}
	for _, row := range got {
		names[row.Domain] = row.Client.IP
	}
	if names["native.example"] != "127.0.0.1" || names["cached.example"] != "192.0.2.50" {
		t.Fatalf("rows %v", names)
	}
	if frames(dnstap.ResultSelf) != 2 || frames(dnstap.ResultRecorded) != 1 {
		t.Fatalf("the sink sent the ingested row back: self %v recorded %v", frames(dnstap.ResultSelf), frames(dnstap.ResultRecorded))
	}
	stop()
}
