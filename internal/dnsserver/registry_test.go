package dnsserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/internal/testcert"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func TestProtocolTable(t *testing.T) {
	tests := []struct {
		p       Protocol
		network string
		tls     bool
		alpn    []string
		proto   pipeline.Proto
	}{
		{ProtoUDP, "udp", false, nil, pipeline.ProtoUDP},
		{ProtoTCP, "tcp", false, nil, pipeline.ProtoTCP},
		{ProtoDoT, "tcp", true, []string{"dot"}, pipeline.ProtoDoT},
		{ProtoDoH, "tcp", true, []string{"h2", "http/1.1"}, pipeline.ProtoDoH},
		{ProtoDoQ, "udp", true, []string{"doq"}, pipeline.ProtoDoQ},
		{ProtoDoH3, "udp", true, []string{"h3"}, pipeline.ProtoDoH3},
	}
	if len(tests) != len(Protocols()) || len(factories) != len(Protocols()) {
		t.Fatal("every protocol needs a factory and a row here")
	}
	for _, tt := range tests {
		if tt.p.Network() != tt.network || tt.p.TLS() != tt.tls || !reflect.DeepEqual(tt.p.ALPN(), tt.alpn) || tt.p.Pipeline() != tt.proto {
			t.Errorf("%s: network=%s tls=%v alpn=%v proto=%s", tt.p, tt.p.Network(), tt.p.TLS(), tt.p.ALPN(), tt.p.Pipeline())
		}
	}
}

func TestLimitsDefaults(t *testing.T) {
	l := Limits{MaxBodyBytes: 1 << 21, QUICMaxConns: -3}.withDefaults()
	if l.QUICMaxStreams != 256 || l.QUICWorkers != 1024 || l.MaxBodyBytes != 65535 || l.QUICMaxConns != 0 ||
		l.ReadTimeout != 2*time.Second || l.QUICIdleTimeout != 30*time.Second || l.MaxHeaderBytes != 16<<10 {
		t.Fatalf("defaults wrong: %+v", l)
	}
}

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := NewRegistry(Options{Handler: &Handler{Chain: pipeline.MustNew(localAnswer{})}, Logger: quietLogger})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRegistryValidation(t *testing.T) {
	if _, err := NewRegistry(Options{}); err == nil {
		t.Fatal("a registry without a handler must be refused")
	}
	pair, err := testcert.Write(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		spec Spec
		want string
	}{
		{"unknown protocol", Spec{Protocol: "dnscrypt", Address: ":5443"}, "unknown protocol"},
		{"missing address", Spec{Protocol: ProtoUDP}, "needs an address"},
		{"tls without files", Spec{Protocol: ProtoDoQ, Address: ":853"}, "needs tls.certFile"},
		{"relative path", Spec{Protocol: ProtoDoH, Address: ":443", Path: "dns-query", CertFile: pair.CertFile, KeyFile: pair.KeyFile}, "must start with /"},
		{"missing cert", Spec{Protocol: ProtoDoT, Address: ":853", CertFile: filepath.Join(t.TempDir(), "x.pem"), KeyFile: pair.KeyFile}, "no such file"},
	}
	for _, tt := range tests {
		if err := newTestRegistry(t).Add(tt.spec); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
		}
	}
	r := newTestRegistry(t)
	if err := r.Add(Spec{Protocol: ProtoUDP, Address: "127.0.0.1:0"}, Spec{Protocol: ProtoUDP, Address: "127.0.0.1:0"}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate listener: %v", err)
	}
	if len(r.Listeners()) != 0 {
		t.Fatal("a failed Add must not register anything")
	}
}

// TestRegistryBindIsAllOrNothing: when one listener cannot bind, the ones
// already bound are released.
func TestRegistryBindIsAllOrNothing(t *testing.T) {
	first := newTestRegistry(t)
	if err := first.Add(Spec{Protocol: ProtoTCP, Address: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	if err := first.Bind(); err != nil {
		t.Fatal(err)
	}
	taken := first.Addr(ProtoTCP).String()
	defer func() { _ = first.Listeners()[0].Close() }()

	second := newTestRegistry(t)
	if err := second.Add(Spec{Protocol: ProtoUDP, Address: "127.0.0.1:0"}, Spec{Protocol: ProtoTCP, Address: taken}); err != nil {
		t.Fatal(err)
	}
	err := second.Bind()
	if err == nil || !strings.Contains(err.Error(), "bind tcp:"+taken) {
		t.Fatalf("Bind error = %v", err)
	}
	udp := second.Lookup(ProtoUDP)[0].(*dnsListener)
	udp.mu.Lock()
	closed := udp.closer == nil
	udp.mu.Unlock()
	if !closed {
		t.Fatal("the UDP socket bound before the failure must be closed")
	}
	if err := second.Add(Spec{Protocol: ProtoUDP, Address: "127.0.0.1:1"}); err != nil {
		t.Fatalf("Add after a failed Bind: %v", err)
	}
}

func TestRegistryReadiness(t *testing.T) {
	pair, err := testcert.Write(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	r := newTestRegistry(t)
	var specs []Spec
	for _, p := range Protocols() {
		specs = append(specs, Spec{Protocol: p, Address: "127.0.0.1:0", CertFile: pair.CertFile, KeyFile: pair.KeyFile})
	}
	if err := r.Add(specs...); err != nil {
		t.Fatal(err)
	}
	if ok, bad := r.Ready(); ok || len(bad) != 6 {
		t.Fatalf("not started: ready=%v unready=%v", ok, bad)
	}
	if len(r.Certs().Reloaders()) != 1 {
		t.Fatal("four TLS listeners on one pair must share one reloader")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for ok, _ := r.Ready(); !ok; ok, _ = r.Ready() {
		if time.Now().After(deadline) {
			_, bad := r.Ready()
			t.Fatalf("never ready: %v", bad)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := r.Add(Spec{Protocol: ProtoUDP, Address: "127.0.0.1:0"}); err == nil {
		t.Fatal("Add after Bind must fail")
	}
	for _, p := range Protocols() {
		if r.Addr(p) == nil {
			t.Fatalf("%s has no bound address", p)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if ok, bad := r.Ready(); ok || len(bad) != 6 {
		t.Fatalf("after shutdown: ready=%v unready=%v", ok, bad)
	}
}

// TestSharedCertificateReload rotates the certificate behind DoT, DoH, DoQ
// and DoH3 listeners and checks that new handshakes on every transport see
// the new certificate without a restart.
func TestSharedCertificateReload(t *testing.T) {
	dir := t.TempDir()
	a, err := testcert.Write(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	f := startFixture(t, fixtureOpts{
		specs:  []Spec{{Protocol: ProtoDoT}, {Protocol: ProtoDoH}, {Protocol: ProtoDoQ}, {Protocol: ProtoDoH3}},
		stages: []pipeline.Stage{localAnswer{}},
		cert:   a,
	})
	b, err := testcert.Write(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(a.Cert)
	pool.AddCert(b.Cert)
	cfg := func(alpn ...string) *tls.Config {
		return &tls.Config{RootCAs: pool, ServerName: "localhost", NextProtos: alpn, MinVersion: tls.VersionTLS12}
	}
	serials := func() map[Protocol]string {
		out := map[Protocol]string{}
		c, err := tls.Dial("tcp", f.addr(ProtoDoT), cfg("dot"))
		if err != nil {
			t.Fatal(err)
		}
		out[ProtoDoT] = c.ConnectionState().PeerCertificates[0].SerialNumber.String()
		_ = c.Close()
		c, err = tls.Dial("tcp", f.addr(ProtoDoH), cfg("h2"))
		if err != nil {
			t.Fatal(err)
		}
		out[ProtoDoH] = c.ConnectionState().PeerCertificates[0].SerialNumber.String()
		_ = c.Close()
		for _, p := range []Protocol{ProtoDoQ, ProtoDoH3} {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			q, err := quic.DialAddr(ctx, f.addr(p), cfg(p.ALPN()...), nil)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			out[p] = q.ConnectionState().TLS.PeerCertificates[0].SerialNumber.String()
			_ = q.CloseWithError(0, "")
		}
		return out
	}
	for p, s := range serials() {
		if s != a.Cert.SerialNumber.String() {
			t.Fatalf("%s served %s before rotation", p, s)
		}
	}
	for _, kv := range [][2]string{{a.CertFile, string(b.CertPEM)}, {a.KeyFile, string(b.KeyPEM)}} {
		if err := os.WriteFile(kv[0]+".new", []byte(kv[1]), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(kv[0]+".new", kv[0]); err != nil {
			t.Fatal(err)
		}
	}
	want := b.Cert.SerialNumber.String()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := serials()
		if got[ProtoDoT] == want && got[ProtoDoH] == want && got[ProtoDoQ] == want && got[ProtoDoH3] == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rotation not visible on every transport: %v", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The listeners still answer after the swap.
	tr := &http3.Transport{TLSClientConfig: cfg()}
	defer func() { _ = tr.Close() }()
	wire, _ := query("example.com").Pack()
	resp, err := (&http.Client{Transport: tr, Timeout: 3 * time.Second}).Post("https://"+f.addr(ProtoDoH3)+DefaultDoHPath, dohMIME, strings.NewReader(string(wire)))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DoH3 after rotation: %s", resp.Status)
	}
}

func TestSpecsAndPolicyFromConfig(t *testing.T) {
	tf := config.TLSFiles{CertFile: "/c", KeyFile: "/k"}
	l := config.Listeners{
		DNS:               []config.DNSListener{{Address: ":53"}, {Address: ":5353", Protocols: []string{"udp"}}},
		DoT:               []config.TLSListener{{Address: ":853", TLS: tf}},
		DoH:               []config.HTTPListener{{Address: ":443", Path: "/q", TLS: tf}},
		DoQ:               []config.TLSListener{{Address: ":853", TLS: tf}},
		DoH3:              []config.HTTPListener{{Address: ":443", TLS: tf}},
		ProxyProtocolFrom: []string{"127.0.0.1/32"},
	}
	want := []Spec{
		{Protocol: ProtoUDP, Address: ":53"},
		{Protocol: ProtoTCP, Address: ":53"},
		{Protocol: ProtoUDP, Address: ":5353"},
		{Protocol: ProtoDoT, Address: ":853", CertFile: "/c", KeyFile: "/k"},
		{Protocol: ProtoDoH, Address: ":443", Path: "/q", CertFile: "/c", KeyFile: "/k"},
		{Protocol: ProtoDoQ, Address: ":853", CertFile: "/c", KeyFile: "/k"},
		{Protocol: ProtoDoH3, Address: ":443", CertFile: "/c", KeyFile: "/k"},
	}
	if got := SpecsFromConfig(l); !reflect.DeepEqual(got, want) {
		t.Fatalf("SpecsFromConfig:\n got %+v\nwant %+v", got, want)
	}
	p, err := PolicyFromConfig(l)
	if err != nil || !p.Enabled() {
		t.Fatalf("loopback policy: %v", err)
	}
	l.ProxyProtocolFrom = []string{"10.0.0.11/32"}
	if _, err := PolicyFromConfig(l); err == nil {
		t.Fatal("non-loopback peer without a secret must fail")
	}
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte(strings.Repeat("ab", 32)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l.ProxyProtocolSecretFile = secret
	if p, err = PolicyFromConfig(l); err != nil || !p.Trusted(netip.MustParseAddr("10.0.0.11")) {
		t.Fatalf("policy with secret: %v", err)
	}
	l.ProxyProtocolSecretFile = filepath.Join(t.TempDir(), "missing")
	if _, err := PolicyFromConfig(l); err == nil {
		t.Fatal("missing secret file must fail")
	}
}

// TestServerWithProxy covers the Do53-only Server type with a policy.
func TestServerWithProxy(t *testing.T) {
	rec := &recorder{}
	s := New("127.0.0.1:0", nil, pipeline.MustNew(rec, localAnswer{}), quietLogger)
	s.Proxy = mustPolicy(t, []string{"127.0.0.1"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Start: %v", err)
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for len(s.Addrs()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	wire, _ := query("example.com").Pack()
	resp, err := udpFrom(t, "127.0.0.1", s.Addrs()["udp"], append(ppHeader(t, 2, s.Addrs()["udp"], nil), wire...), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	checkA(t, resp)
	if c, _ := rec.last(t); c.Addr != realClient {
		t.Fatalf("client %s", c.Addr)
	}
	if err := New("127.0.0.1:0", []string{"sctp"}, pipeline.MustNew(localAnswer{}), nil).Start(context.Background()); err == nil {
		t.Fatal("unsupported protocol must fail")
	}
	// ServeDNS remains usable as a plain dns.Handler.
	var _ dns.Handler = s
}
