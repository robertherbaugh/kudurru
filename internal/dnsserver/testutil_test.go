package dnsserver

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AdguardTeam/dnsproxy/upstream"
	"github.com/miekg/dns"
	"github.com/quic-go/quic-go"

	"github.com/robertherbaugh/kudurru/internal/dnsserver/internal/testcert"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/proxyproto"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
)

var quietLogger = slog.New(slog.DiscardHandler)

// recorder is an "identify"-position stage that records what the pipeline
// sees for every query: the client and the message ID.
type recorder struct {
	mu      sync.Mutex
	clients []pipeline.Client
	ids     []uint16
}

func (r *recorder) Name() string { return "identify" }

func (r *recorder) Serve(req *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	r.mu.Lock()
	r.clients = append(r.clients, req.Client)
	r.ids = append(r.ids, req.Msg.Id)
	r.mu.Unlock()
	return next(req)
}

func (r *recorder) last(t *testing.T) (pipeline.Client, uint16) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.clients) == 0 {
		t.Fatal("no query reached the pipeline")
	}
	return r.clients[len(r.clients)-1], r.ids[len(r.ids)-1]
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.clients)
}

// recordingUpstream answers every A query with 192.0.2.1 (TTL 60) and keeps
// the message IDs it received, so tests can check what went on the wire.
type recordingUpstream struct {
	addr string
	mu   sync.Mutex
	ids  []uint16
}

func newRecordingUpstream(t *testing.T) *recordingUpstream {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &recordingUpstream{addr: pc.LocalAddr().String()}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		u.mu.Lock()
		u.ids = append(u.ids, r.Id)
		u.mu.Unlock()
		m := new(dns.Msg)
		m.SetReply(r)
		m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP("192.0.2.1")}}
		_ = w.WriteMsg(m)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return u
}

func (u *recordingUpstream) seen() []uint16 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]uint16(nil), u.ids...)
}

// localAnswer is a "local"-position stage that answers every A query with
// 192.0.2.1 without leaving the process, so transport costs dominate.
type localAnswer struct{}

func (localAnswer) Name() string { return "local" }

func (localAnswer) Serve(req *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	q := req.Question()
	if q.Qtype != dns.TypeA {
		return next(req)
	}
	r := pipeline.Reply(req, dns.RcodeSuccess, pipeline.StatusLocalRecord)
	r.Msg.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(192, 0, 2, 1)}}
	return r, nil
}

// gate is a "local"-position stage that holds every query until released
// and tracks how many are in flight.
type gate struct {
	release  chan struct{}
	inflight atomic.Int64
	peak     atomic.Int64
	arrived  chan struct{}
}

func newGate(buffer int) *gate {
	return &gate{release: make(chan struct{}), arrived: make(chan struct{}, buffer)}
}

func (g *gate) Name() string { return "local" }

func (g *gate) Serve(req *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	n := g.inflight.Add(1)
	for {
		p := g.peak.Load()
		if n <= p || g.peak.CompareAndSwap(p, n) {
			break
		}
	}
	select {
	case g.arrived <- struct{}{}:
	default:
	}
	<-g.release
	g.inflight.Add(-1)
	return localAnswer{}.Serve(req, next)
}

// fixture is a running registry plus everything a test needs to talk to it.
type fixture struct {
	reg  *Registry
	rec  *recorder
	up   *recordingUpstream
	cert *testcert.Pair
}

type fixtureOpts struct {
	specs  []Spec
	proxy  *proxyproto.Policy
	limits Limits
	stages []pipeline.Stage // replaces the default ingress/recorder/upstream chain
	cert   *testcert.Pair
	tsig   TSIGKeyring
}

// startFixture runs a registry on 127.0.0.1 with ephemeral ports. TLS specs
// without files get the fixture certificate.
func startFixture(t *testing.T, o fixtureOpts) *fixture {
	t.Helper()
	f := &fixture{rec: &recorder{}, cert: o.cert}
	if f.cert == nil {
		var err error
		if f.cert, err = testcert.Write(t.TempDir(), ""); err != nil {
			t.Fatal(err)
		}
	}
	stages := o.stages
	if stages == nil {
		f.up = newRecordingUpstream(t)
		fwd, err := upstreamstage.New("default", []string{f.up.addr}, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		stages = []pipeline.Stage{&ingress.Stage{NodeID: "test-node", Version: "test"}, f.rec, fwd}
	}
	chain := pipeline.MustNew(stages...)
	reg, err := NewRegistry(Options{
		Handler: &Handler{Chain: chain, Logger: quietLogger},
		Logger:  quietLogger,
		Proxy:   o.proxy,
		Limits:  o.limits,
		TSIG:    o.tsig,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range o.specs {
		if o.specs[i].Address == "" {
			o.specs[i].Address = "127.0.0.1:0"
		}
		if o.specs[i].Protocol.TLS() && o.specs[i].CertFile == "" {
			o.specs[i].CertFile, o.specs[i].KeyFile = f.cert.CertFile, f.cert.KeyFile
		}
	}
	if err := reg.Add(o.specs...); err != nil {
		t.Fatal(err)
	}
	if err := reg.Bind(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reg.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("registry Run: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("registry did not shut down")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok, _ := reg.Ready(); ok {
			f.reg = reg
			return f
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, bad := reg.Ready()
	t.Fatalf("listeners not ready: %v", bad)
	return nil
}

func (f *fixture) addr(p Protocol) string { return f.reg.Addr(p).String() }

// upstreamURL is the dnsproxy address of a listener.
func (f *fixture) upstreamURL(p Protocol) string {
	a := f.addr(p)
	switch p {
	case ProtoUDP:
		return "udp://" + a
	case ProtoTCP:
		return "tcp://" + a
	case ProtoDoT:
		return "tls://" + a
	case ProtoDoH:
		return "https://" + a + DefaultDoHPath
	case ProtoDoQ:
		return "quic://" + a
	case ProtoDoH3:
		return "h3://" + a + DefaultDoHPath
	}
	panic("unknown protocol")
}

// dnsproxyClient returns a dnsproxy upstream for one listener, trusting the
// fixture certificate.
func (f *fixture) dnsproxyClient(t testing.TB, p Protocol, httpVersions ...upstream.HTTPVersion) upstream.Upstream {
	t.Helper()
	u, err := upstream.AddressToUpstream(f.upstreamURL(p), &upstream.Options{
		Logger:       quietLogger,
		RootCAs:      f.cert.Pool,
		Timeout:      3 * time.Second,
		HTTPVersions: httpVersions,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = u.Close() })
	return u
}

func query(name string) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	return m
}

func checkA(t testing.TB, resp *dns.Msg) {
	t.Helper()
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
		t.Fatalf("unexpected answer: %v", resp)
	}
	if a, ok := resp.Answer[0].(*dns.A); !ok || a.A.String() != "192.0.2.1" {
		t.Fatalf("wrong record %v", resp.Answer[0])
	}
}

// doqDial opens a raw DoQ connection trusting pool.
func doqDial(t testing.TB, addr string, pool *tlsPool, alpn ...string) (*quic.Conn, error) {
	t.Helper()
	if len(alpn) == 0 {
		alpn = []string{"doq"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return quic.DialAddr(ctx, addr, pool.client(alpn...), &quic.Config{MaxIdleTimeout: 5 * time.Second})
}

// tlsPool wraps the fixture certificate pool for raw clients.
type tlsPool struct{ pair *testcert.Pair }

func (p *tlsPool) client(alpn ...string) *tls.Config {
	return &tls.Config{RootCAs: p.pair.Pool, ServerName: "localhost", NextProtos: alpn, MinVersion: tls.VersionTLS12}
}

// doqExchange sends m (whose ID it does not touch) on a new stream of conn
// and returns the decoded response.
func doqExchange(conn *quic.Conn, m *dns.Msg) (*dns.Msg, error) {
	wire, err := m.Pack()
	if err != nil {
		return nil, err
	}
	return doqExchangeRaw(conn, lenPrefix(wire), wire)
}

// lenPrefix returns the 2-byte big-endian length of b (DoQ and DNS-over-TCP
// framing).
func lenPrefix(b []byte) []byte {
	if len(b) > 0xFFFF {
		panic("message too long")
	}
	return binary.BigEndian.AppendUint16(nil, uint16(len(b))) //nolint:gosec // checked above
}

func closeQUIC(c *quic.Conn) { _ = c.CloseWithError(0, "") }

func doqExchangeRaw(conn *quic.Conn, parts ...[]byte) (*dns.Msg, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	st, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	_ = st.SetDeadline(time.Now().Add(3 * time.Second))
	for _, p := range parts {
		if _, err := st.Write(p); err != nil {
			return nil, err
		}
	}
	_ = st.Close()
	b, err := io.ReadAll(st)
	if err != nil {
		return nil, err
	}
	if len(b) < 2 || int(binary.BigEndian.Uint16(b)) != len(b)-2 {
		return nil, errors.New("bad DoQ framing in response")
	}
	resp := new(dns.Msg)
	return resp, resp.Unpack(b[2:])
}
