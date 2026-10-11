package dnsserver

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AdguardTeam/dnsproxy/upstream"
	"github.com/miekg/dns"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

var allSpecs = func() []Spec {
	var out []Spec
	for _, p := range Protocols() {
		out = append(out, Spec{Protocol: p})
	}
	return out
}

// TestTransportsEndToEnd is the spike gate: every transport answers through
// the same chain (ingress → recorder → upstream to a fake server), using
// dnsproxy's upstream package as an independent client, and the pipeline
// sees the right Client.Proto. Transports whose clients send a fixed ID
// must never leak it into the pipeline or onto the upstream wire.
func TestTransportsEndToEnd(t *testing.T) {
	f := startFixture(t, fixtureOpts{specs: allSpecs()})
	tests := []struct {
		proto     Protocol
		versions  []upstream.HTTPVersion
		want      pipeline.Proto
		fixedID   bool
		wantLabel string
	}{
		{proto: ProtoUDP, want: pipeline.ProtoUDP, wantLabel: "udp"},
		{proto: ProtoTCP, want: pipeline.ProtoTCP, wantLabel: "tcp"},
		{proto: ProtoDoT, want: pipeline.ProtoDoT, wantLabel: "dot"},
		{proto: ProtoDoH, versions: []upstream.HTTPVersion{upstream.HTTPVersion2}, want: pipeline.ProtoDoH, fixedID: true, wantLabel: "doh"},
		{proto: ProtoDoH, versions: []upstream.HTTPVersion{upstream.HTTPVersion11}, want: pipeline.ProtoDoH, fixedID: true, wantLabel: "doh"},
		{proto: ProtoDoQ, want: pipeline.ProtoDoQ, fixedID: true, wantLabel: "doq"},
		{proto: ProtoDoH3, want: pipeline.ProtoDoH, fixedID: true, wantLabel: "doh"},
	}
	for _, tt := range tests {
		name := string(tt.proto)
		if len(tt.versions) > 0 {
			name += "/" + string(tt.versions[0])
		}
		t.Run(name, func(t *testing.T) {
			c := f.dnsproxyClient(t, tt.proto, tt.versions...)
			for i := range 3 {
				q := query("example.com")
				q.Id = uint16(0x4000 + i)
				resp, err := c.Exchange(context.Background(), q)
				if err != nil {
					t.Fatalf("%s exchange: %v", c.Address(), err)
				}
				checkA(t, resp)
				if resp.Id != q.Id {
					t.Fatalf("response ID %d, query ID %d", resp.Id, q.Id)
				}
				client, id := f.rec.last(t)
				if client.Proto != tt.want || client.Proto.String() != tt.wantLabel {
					t.Fatalf("pipeline saw proto %s, want %s", client.Proto, tt.want)
				}
				if got := client.Addr.Addr().String(); got != "127.0.0.1" {
					t.Fatalf("pipeline saw client %s", client.Addr)
				}
				if client.ProxiedFrom.IsValid() {
					t.Fatal("no PROXY header was sent")
				}
				if tt.fixedID && id == 0 {
					t.Fatal("the pipeline must never see the client's fixed ID 0")
				}
			}
		})
	}
	for _, id := range f.up.seen() {
		if id == 0 {
			t.Fatal("a query reached the upstream with ID 0")
		}
	}
}

func TestDoQFreshIDsPerQuery(t *testing.T) {
	f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: ProtoDoQ}}})
	conn, err := doqDial(t, f.addr(ProtoDoQ), &tlsPool{f.cert})
	if err != nil {
		t.Fatal(err)
	}
	defer closeQUIC(conn)
	const n = 32
	for range n {
		q := query("example.com")
		q.Id = 0
		resp, err := doqExchange(conn, q)
		if err != nil {
			t.Fatal(err)
		}
		checkA(t, resp)
		if resp.Id != 0 {
			t.Fatalf("DoQ response ID %d, must be 0", resp.Id)
		}
	}
	ids := map[uint16]bool{}
	for _, id := range f.up.seen() {
		ids[id] = true
	}
	// 32 random 16-bit IDs collide rarely; a fixed ID would give 1.
	if len(ids) < n-4 || ids[0] {
		t.Fatalf("upstream saw %d distinct IDs for %d queries (zero present: %v)", len(ids), n, ids[0])
	}
}

func TestDoQProtocolErrors(t *testing.T) {
	f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: ProtoDoQ}}})
	pool := &tlsPool{f.cert}
	valid := func() []byte {
		q := query("example.com")
		q.Id = 0
		b, _ := q.Pack()
		return b
	}
	withPrefix := func(b []byte) [][]byte { return [][]byte{lenPrefix(b), b} }
	nonZero := query("example.com")
	nonZero.Id = 7
	nz, _ := nonZero.Pack()
	keep := query("example.com")
	keep.Id = 0
	keep.SetEdns0(1232, false)
	keep.IsEdns0().Option = append(keep.IsEdns0().Option, &dns.EDNS0_TCP_KEEPALIVE{Code: dns.EDNS0TCPKEEPALIVE})
	kp, _ := keep.Pack()

	tests := map[string][][]byte{
		"non-zero message ID":   withPrefix(nz),
		"edns-tcp-keepalive":    withPrefix(kp),
		"zero length prefix":    {{0, 0}, valid()},
		"draft framing":         {valid()},
		"FIN before full query": {{0, 200}, valid()},
		"two messages":          append(withPrefix(valid()), withPrefix(valid())...),
		"garbage":               withPrefix([]byte("not a dns message")),
	}
	for name, parts := range tests {
		t.Run(name, func(t *testing.T) {
			conn, err := doqDial(t, f.addr(ProtoDoQ), pool)
			if err != nil {
				t.Fatal(err)
			}
			defer closeQUIC(conn)
			_, err = doqExchangeRaw(conn, parts...)
			var appErr *quic.ApplicationError
			if !errors.As(err, &appErr) || appErr.ErrorCode != DoQProtocolError || !appErr.Remote {
				t.Fatalf("want CONNECTION_CLOSE DOQ_PROTOCOL_ERROR, got %v", err)
			}
		})
	}

	t.Run("wrong ALPN is refused", func(t *testing.T) {
		if _, err := doqDial(t, f.addr(ProtoDoQ), pool, "h3"); err == nil {
			t.Fatal("a DoQ listener must not accept ALPN h3")
		}
	})
	t.Run("unidirectional streams are not allowed", func(t *testing.T) {
		conn, err := doqDial(t, f.addr(ProtoDoQ), pool)
		if err != nil {
			t.Fatal(err)
		}
		defer closeQUIC(conn)
		if _, err := conn.OpenUniStream(); err == nil {
			t.Fatal("DoQ must not grant unidirectional streams")
		}
	})
	t.Run("valid query after errors", func(t *testing.T) {
		conn, err := doqDial(t, f.addr(ProtoDoQ), pool)
		if err != nil {
			t.Fatal(err)
		}
		defer closeQUIC(conn)
		resp, err := doqExchangeRaw(conn, withPrefix(valid())...)
		if err != nil {
			t.Fatal(err)
		}
		checkA(t, resp)
	})
}

// TestDoQStreamAndWorkerLimits checks the per-connection stream cap (the
// client cannot open stream 257 while 256 are in flight) and that the
// listener-wide worker pool bounds concurrent queries.
func TestDoQStreamAndWorkerLimits(t *testing.T) {
	g := newGate(512)
	f := startFixture(t, fixtureOpts{
		specs:  []Spec{{Protocol: ProtoDoQ}},
		stages: []pipeline.Stage{g},
		limits: Limits{QUICWorkers: 300},
	})
	conn, err := doqDial(t, f.addr(ProtoDoQ), &tlsPool{f.cert})
	if err != nil {
		t.Fatal(err)
	}
	defer closeQUIC(conn)

	var wg sync.WaitGroup
	errs := make(chan error, DefaultQUICMaxStreams)
	for range DefaultQUICMaxStreams {
		wg.Go(func() {
			q := query("example.com")
			q.Id = 0
			resp, err := doqExchange(conn, q)
			if err == nil && len(resp.Answer) != 1 {
				err = errors.New("empty answer")
			}
			if err != nil {
				errs <- err
			}
		})
	}
	for range DefaultQUICMaxStreams {
		select {
		case <-g.arrived:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d streams reached the pipeline", g.inflight.Load())
		}
	}
	if _, err := conn.OpenStream(); err == nil {
		t.Fatal("stream 257 must exceed MaxIncomingStreams")
	} else if !errors.As(err, new(*quic.StreamLimitReachedError)) {
		t.Fatalf("unexpected error: %v", err)
	}
	close(g.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if p := g.peak.Load(); p != DefaultQUICMaxStreams {
		t.Fatalf("peak in-flight %d, want %d", p, DefaultQUICMaxStreams)
	}

	// Worker pool: 4 workers, 2 connections x 20 streams.
	g2 := newGate(64)
	f2 := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: ProtoDoQ}}, stages: []pipeline.Stage{g2}, limits: Limits{QUICWorkers: 4}})
	var wg2 sync.WaitGroup
	for range 2 {
		c, err := doqDial(t, f2.addr(ProtoDoQ), &tlsPool{f2.cert})
		if err != nil {
			t.Fatal(err)
		}
		defer closeQUIC(c)
		for range 20 {
			wg2.Go(func() {
				q := query("example.com")
				q.Id = 0
				_, _ = doqExchange(c, q)
			})
		}
	}
	for range 4 {
		<-g2.arrived
	}
	time.Sleep(100 * time.Millisecond)
	if n := g2.inflight.Load(); n != 4 {
		t.Fatalf("%d queries in flight with 4 workers", n)
	}
	close(g2.release)
	wg2.Wait()
	if p := g2.peak.Load(); p != 4 {
		t.Fatalf("peak %d with 4 workers", p)
	}
}

func TestQUICConnectionLimit(t *testing.T) {
	f := startFixture(t, fixtureOpts{
		specs:  []Spec{{Protocol: ProtoDoQ}},
		stages: []pipeline.Stage{localAnswer{}},
		limits: Limits{QUICMaxConns: 1},
	})
	pool := &tlsPool{f.cert}
	first, err := doqDial(t, f.addr(ProtoDoQ), pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeQUIC(first)
	q := query("example.com")
	q.Id = 0
	if _, err := doqExchange(first, q); err != nil {
		t.Fatal(err)
	}
	second, err := doqDial(t, f.addr(ProtoDoQ), pool)
	if err == nil {
		_, err = doqExchange(second, q)
		closeQUIC(second)
	}
	var appErr *quic.ApplicationError
	if !errors.As(err, &appErr) || appErr.ErrorCode != DoQExcessiveLoad {
		t.Fatalf("second DoQ connection: want DOQ_EXCESSIVE_LOAD, got %v", err)
	}
}

func dohClient(t *testing.T, f *fixture, h2 bool) *http.Client {
	t.Helper()
	tr := &http.Transport{TLSClientConfig: (&tlsPool{f.cert}).client(), ForceAttemptHTTP2: h2}
	if !h2 {
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}
}

func TestDoHHTTPSemantics(t *testing.T) {
	f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: ProtoDoH}, {Protocol: ProtoDoH3, Path: "/custom"}}, stages: []pipeline.Stage{localAnswer{}}})
	base := "https://" + f.addr(ProtoDoH)
	q := query("example.com")
	q.Id = 0
	wire, _ := q.Pack()
	get := base + DefaultDoHPath + "?dns=" + base64.RawURLEncoding.EncodeToString(wire)

	for _, h2 := range []bool{true, false} {
		c := dohClient(t, f, h2)
		resp, err := c.Get(get)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		wantProto := map[bool]string{true: "HTTP/2.0", false: "HTTP/1.1"}[h2]
		if resp.StatusCode != http.StatusOK || resp.Proto != wantProto {
			t.Fatalf("GET: %s %s", resp.Proto, resp.Status)
		}
		if ct := resp.Header.Get("Content-Type"); ct != dohMIME {
			t.Fatalf("content-type %q", ct)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "max-age=60" {
			t.Fatalf("cache-control %q", cc)
		}
		m := new(dns.Msg)
		if err := m.Unpack(body); err != nil {
			t.Fatal(err)
		}
		checkA(t, m)
		if m.Id != 0 {
			t.Fatalf("DoH response ID %d, request ID 0", m.Id)
		}
	}

	c := dohClient(t, f, true)
	do := func(method, url, ctype string, body []byte) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, url, bytes.NewReader(body))
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp
	}
	tests := []struct {
		name, method, url, ctype string
		body                     []byte
		want                     int
	}{
		{"POST", http.MethodPost, base + DefaultDoHPath, dohMIME, wire, http.StatusOK},
		{"POST with charset", http.MethodPost, base + DefaultDoHPath, dohMIME + "; charset=binary", wire, http.StatusOK},
		{"GET padded base64", http.MethodGet, base + DefaultDoHPath + "?dns=" + base64.URLEncoding.EncodeToString(wire), "", nil, http.StatusOK},
		{"HEAD", http.MethodHead, get, "", nil, http.StatusOK},
		{"wrong path", http.MethodGet, base + "/resolve?dns=AAAA", "", nil, http.StatusNotFound},
		{"PUT", http.MethodPut, base + DefaultDoHPath, dohMIME, wire, http.StatusMethodNotAllowed},
		{"POST wrong type", http.MethodPost, base + DefaultDoHPath, "application/json", wire, http.StatusUnsupportedMediaType},
		{"POST too large", http.MethodPost, base + DefaultDoHPath, dohMIME, make([]byte, maxDNSMessage+1), http.StatusRequestEntityTooLarge},
		{"GET missing param", http.MethodGet, base + DefaultDoHPath, "", nil, http.StatusBadRequest},
		{"GET bad base64", http.MethodGet, base + DefaultDoHPath + "?dns=***", "", nil, http.StatusBadRequest},
		{"GET malformed dns", http.MethodGet, base + DefaultDoHPath + "?dns=AAAA", "", nil, http.StatusBadRequest},
		{"POST empty", http.MethodPost, base + DefaultDoHPath, dohMIME, nil, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if resp := do(tt.method, tt.url, tt.ctype, tt.body); resp.StatusCode != tt.want {
				t.Fatalf("status %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}

	t.Run("DoH3 shares the handler and honours its path", func(t *testing.T) {
		tr := &http3.Transport{TLSClientConfig: (&tlsPool{f.cert}).client()}
		defer func() { _ = tr.Close() }()
		h3 := &http.Client{Transport: tr, Timeout: 5 * time.Second}
		url := "https://" + f.addr(ProtoDoH3)
		resp, err := h3.Post(url+"/custom", dohMIME, bytes.NewReader(wire))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Proto != "HTTP/3.0" || resp.Header.Get("Cache-Control") != "max-age=60" {
			t.Fatalf("DoH3 POST: %s %s %v", resp.Proto, resp.Status, resp.Header)
		}
		m := new(dns.Msg)
		if err := m.Unpack(body); err != nil {
			t.Fatal(err)
		}
		checkA(t, m)
		resp, err = h3.Get(url + DefaultDoHPath + "?dns=" + base64.RawURLEncoding.EncodeToString(wire))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("DoH3 default path on a /custom listener: %d", resp.StatusCode)
		}
	})
}

func TestCacheControl(t *testing.T) {
	a := func(ttl uint32) dns.RR {
		return &dns.A{Hdr: dns.RR_Header{Name: "x.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl}, A: net.IPv4(192, 0, 2, 1)}
	}
	soa := func(ttl, minttl uint32) dns.RR {
		return &dns.SOA{Hdr: dns.RR_Header{Name: "x.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: ttl}, Ns: "ns.x.", Mbox: "h.x.", Minttl: minttl}
	}
	msg := func(rcode int, answer, ns []dns.RR) *dns.Msg {
		m := new(dns.Msg)
		m.Rcode, m.Answer, m.Ns = rcode, answer, ns
		m.SetEdns0(1232, false) // OPT has no TTL semantics and must be ignored
		return m
	}
	tests := []struct {
		name string
		m    *dns.Msg
		want string
	}{
		{"smallest answer TTL", msg(dns.RcodeSuccess, []dns.RR{a(300), a(60), a(120)}, nil), "max-age=60"},
		{"zero TTL", msg(dns.RcodeSuccess, []dns.RR{a(0)}, nil), "max-age=0"},
		{"NXDOMAIN uses SOA minimum", msg(dns.RcodeNameError, nil, []dns.RR{soa(3600, 300)}), "max-age=300"},
		{"NODATA uses SOA TTL when lower", msg(dns.RcodeSuccess, nil, []dns.RR{soa(30, 300)}), "max-age=30"},
		{"no records", msg(dns.RcodeSuccess, nil, nil), "max-age=0"},
		{"SERVFAIL", msg(dns.RcodeServerFailure, []dns.RR{a(60)}, nil), "no-store"},
		{"REFUSED", msg(dns.RcodeRefused, nil, nil), "no-store"},
	}
	for _, tt := range tests {
		if got := cacheControl(tt.m); got != tt.want {
			t.Errorf("%s: %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestDoTALPNAndPlainTLSClient(t *testing.T) {
	f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: ProtoDoT}}, stages: []pipeline.Stage{localAnswer{}}})
	for _, alpn := range [][]string{{"dot"}, nil} {
		c := &dns.Client{Net: "tcp-tls", TLSConfig: (&tlsPool{f.cert}).client(alpn...), Timeout: 3 * time.Second}
		resp, _, err := c.Exchange(query("example.com"), f.addr(ProtoDoT))
		if err != nil {
			t.Fatalf("ALPN %v: %v", alpn, err)
		}
		checkA(t, resp)
	}
	c := &dns.Client{Net: "tcp-tls", TLSConfig: (&tlsPool{f.cert}).client("h2"), Timeout: 3 * time.Second}
	if _, _, err := c.Exchange(query("example.com"), f.addr(ProtoDoT)); err == nil || !strings.Contains(err.Error(), "protocol") {
		t.Fatalf("ALPN h2 on DoT should be refused, got %v", err)
	}
}

// TestLongLivedStreamConnections: a TCP or DoT connection is not closed
// after miekg's default 128 queries, unless Limits.MaxTCPQueries asks for
// it.
func TestLongLivedStreamConnections(t *testing.T) {
	for _, limit := range []int{0, 10} {
		f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: ProtoTCP}, {Protocol: ProtoDoT}}, stages: []pipeline.Stage{localAnswer{}}, limits: Limits{MaxTCPQueries: limit}})
		for _, p := range []Protocol{ProtoTCP, ProtoDoT} {
			c := &dns.Client{Net: "tcp", Timeout: 2 * time.Second}
			if p == ProtoDoT {
				c.Net, c.TLSConfig = "tcp-tls", (&tlsPool{f.cert}).client("dot")
			}
			co, err := c.Dial(f.addr(p))
			if err != nil {
				t.Fatal(err)
			}
			want := 300
			if limit > 0 {
				want = limit
			}
			served := 0
			for range 300 {
				if _, _, err := c.ExchangeWithConn(query("example.com"), co); err != nil {
					break
				}
				served++
			}
			_ = co.Close()
			if served != want {
				t.Fatalf("%s with MaxTCPQueries=%d: %d queries served on one connection, want %d", p, limit, served, want)
			}
		}
	}
}

func TestUDPLargeQueryNotTruncated(t *testing.T) {
	f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: ProtoUDP}}, stages: []pipeline.Stage{localAnswer{}}})
	q := query("example.com")
	q.SetEdns0(4096, false)
	// 900 bytes of EDNS padding: larger than miekg's 512-byte default
	// read buffer.
	q.IsEdns0().Option = append(q.IsEdns0().Option, &dns.EDNS0_PADDING{Padding: make([]byte, 900)})
	c := &dns.Client{Net: "udp", UDPSize: 4096, Timeout: 2 * time.Second}
	resp, _, err := c.Exchange(q, f.addr(ProtoUDP))
	if err != nil {
		t.Fatal(err)
	}
	checkA(t, resp)
}
