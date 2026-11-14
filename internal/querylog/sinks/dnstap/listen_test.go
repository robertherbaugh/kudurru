package dnstap_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/dnstap"
)

// recorder is a Collector that keeps what it receives. With gate set,
// Enqueue waits for it: a collector that breaks the contract and blocks.
type recorder struct {
	mu     sync.Mutex
	events []*querylog.Event
	gate   chan struct{}
	after  func(*querylog.Event)
}

func (r *recorder) Enqueue(ev *querylog.Event) bool {
	if r.gate != nil {
		<-r.gate
	}
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
	if r.after != nil {
		r.after(ev)
	}
	return true
}

func (r *recorder) list() []*querylog.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*querylog.Event(nil), r.events...)
}

type listenFixture struct {
	l       *dnstap.Listener
	metrics *dnstap.ListenMetrics
	rec     *recorder
	addr    string
}

func (f *listenFixture) count(result string) float64 {
	return testutil.ToFloat64(f.metrics.Frames.WithLabelValues(f.l.Name(), result))
}

func (f *listenFixture) conns(result string) float64 {
	return testutil.ToFloat64(f.metrics.Connections.WithLabelValues(f.l.Name(), result))
}

// startListener runs a tcp listener on a loopback port with short waits.
func startListener(t *testing.T, settings config.DnstapListener, o dnstap.ListenOptions) *listenFixture {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if settings.Address == "" {
		settings.Address = "tcp:" + ln.Addr().String()
	}
	if settings.AllowFrom == nil {
		settings.AllowFrom = []string{"127.0.0.1"}
	}
	f := &listenFixture{rec: &recorder{}, metrics: dnstap.NewListenMetrics(nil), addr: ln.Addr().String()}
	if o.Collector == nil {
		o.Collector = f.rec
	}
	o.Settings, o.Metrics, o.Listener = settings, f.metrics, ln
	if o.Node == "" {
		o.Node = "dns1"
	}
	if o.Hold == 0 {
		o.Hold = 50 * time.Millisecond
	}
	if o.PairWait == 0 {
		o.PairWait = 200 * time.Millisecond
	}
	if f.l, err = dnstap.Listen(o); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.l.Serve(context.Background()) }()
	t.Cleanup(func() {
		_ = f.l.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("the listener did not stop")
		}
	})
	return f
}

// sender is a hand-driven Frame Streams writer.
type sender struct {
	t    *testing.T
	conn net.Conn
	r    *dnstap.Reader
}

func dialSender(t *testing.T, addr string) *sender {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	s := &sender{t: t, conn: conn, r: dnstap.NewReader(conn)}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	s.write(dnstap.AppendControl(nil, dnstap.ControlReady, dnstap.ContentType))
	if _, err := s.r.ReadControl(dnstap.ControlAccept); err != nil {
		t.Fatal("no ACCEPT:", err)
	}
	s.write(dnstap.AppendControl(nil, dnstap.ControlStart, dnstap.ContentType))
	_ = conn.SetDeadline(time.Time{})
	return s
}

func (s *sender) write(b []byte) {
	s.t.Helper()
	if _, err := s.conn.Write(b); err != nil {
		s.t.Fatal(err)
	}
}

func (s *sender) send(identity string, m dnstap.Message) {
	s.t.Helper()
	payload, _ := dnstap.AppendFrame(nil, nil, &dnstap.Frame{Identity: []byte(identity), Version: []byte("dnsdist 2.1.3"), Message: m})
	s.write(dnstap.AppendData(nil, payload))
}

// stop sends STOP and waits for FINISH.
func (s *sender) stop() {
	s.t.Helper()
	_ = s.conn.SetDeadline(time.Now().Add(5 * time.Second))
	s.write(dnstap.AppendControl(nil, dnstap.ControlStop))
	if _, err := s.r.ReadControl(dnstap.ControlFinish); err != nil {
		s.t.Fatal("no FINISH:", err)
	}
}

var client = netip.MustParseAddr("192.168.1.20")

func dnsQuery(t *testing.T, name string, id uint16) *dns.Msg {
	t.Helper()
	q := new(dns.Msg)
	q.SetQuestion(name, dns.TypeA)
	q.Id = id
	return q
}

func dnsAnswer(q *dns.Msg) *dns.Msg {
	a := new(dns.Msg)
	a.SetReply(q)
	a.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: []byte{192, 0, 2, 9}}}
	return a
}

// clientMessages are the CLIENT_QUERY and CLIENT_RESPONSE of one query
// from client:port, answered after 4 ms.
func clientMessages(t *testing.T, name string, id, port uint16, at time.Time) (query, response dnstap.Message) {
	t.Helper()
	q := dnsQuery(t, name, id)
	base := dnstap.Message{SocketFamily: dnstap.FamilyINET, SocketProtocol: dnstap.ProtocolUDP, QueryAddress: client.AsSlice(), QueryPort: port, QueryTime: at}
	query, response = base, base
	query.Type, query.QueryMessage = dnstap.ClientQuery, packed(t, q)
	response.Type, response.ResponseTime, response.ResponseMessage = dnstap.ClientResponse, at.Add(4*time.Millisecond), packed(t, dnsAnswer(q))
	return query, response
}

func waitEvents(t *testing.T, rec *recorder, n int) []*querylog.Event {
	t.Helper()
	waitFor(t, "recorded events", func() bool { return len(rec.list()) >= n })
	// Nothing more may follow.
	time.Sleep(400 * time.Millisecond)
	events := rec.list()
	if len(events) != n {
		t.Fatalf("%d events, want %d: %+v", len(events), n, events)
	}
	return events
}

// TestListenRecordsEachClientQueryOnce: a query and its response are one
// row, a lone response and a lone query (after the pairing wait) one each,
// a forwarder message none, and a message about a query this node already
// logged none.
func TestListenRecordsEachClientQueryOnce(t *testing.T) {
	dedup := querylog.NewDedupIndex(0, 0)
	f := startListener(t, config.DnstapListener{}, dnstap.ListenOptions{Dedup: dedup})
	now := time.Now().Add(-time.Second).Truncate(time.Microsecond)
	// This node answered d.example. from the same client and port: dnsdist
	// forwarded it to kudurru, which logged it through the collector.
	dedup.Record(&querylog.Event{Time: now.Add(time.Millisecond), Client: client, ClientPort: 40004, Name: "d.example.", QType: dns.TypeA, QClass: dns.ClassINET})
	s := dialSender(t, f.addr)
	aq, ar := clientMessages(t, "A.example.", 1, 40001, now)
	_, br := clientMessages(t, "b.example.", 2, 40002, now)
	cq, _ := clientMessages(t, "c.example.", 3, 40003, now)
	_, dr := clientMessages(t, "d.example.", 4, 40004, now)
	forward := aq
	forward.Type = dnstap.ForwarderQuery
	s.send("edge-1", aq)
	s.send("edge-1", forward)
	s.send("edge-1", ar)
	s.send("edge-1", br)
	s.send("edge-1", cq)
	s.send("edge-1", dr)
	s.stop()
	events := waitEvents(t, f.rec, 3)
	byName := map[string]*querylog.Event{}
	for _, ev := range events {
		byName[ev.Name] = ev
		if ev.Origin != "edge-1" || ev.Node != "dns1" || ev.Client != client || ev.Proto != pipeline.ProtoUDP || ev.Status != pipeline.StatusUnknown {
			t.Fatalf("event %+v", ev)
		}
	}
	a := byName["a.example."]
	if a == nil || a.Latency != 4*time.Millisecond || !a.Time.Equal(now) || a.ReplyType != querylog.ReplyIP || a.Answers != 1 || a.Answer != "192.0.2.9" || !a.Flags.RD || a.ClientPort != 40001 {
		t.Fatalf("paired row %+v", a)
	}
	if b := byName["b.example."]; b == nil || b.ReplyType != querylog.ReplyIP || b.Latency != 4*time.Millisecond {
		t.Fatalf("response-only row %+v", b)
	}
	if c := byName["c.example."]; c == nil || c.ReplyType != querylog.ReplyUnknown || c.Answers != 0 {
		t.Fatalf("query-only row %+v", c)
	}
	if byName["d.example."] != nil {
		t.Fatal("a message about a query this node logged was recorded again")
	}
	for result, want := range map[string]float64{dnstap.ResultRecorded: 3, dnstap.ResultPaired: 1, dnstap.ResultDuplicate: 1, dnstap.ResultType: 1} {
		if got := f.count(result); got != want {
			t.Errorf("%s = %v, want %v", result, got, want)
		}
	}
	if dedup.Len() != 0 {
		t.Fatal("the claimed query is still in the index")
	}
	if f.conns(dnstap.ConnAccepted) != 1 {
		t.Fatal("connection not counted")
	}
}

// TestListenDedupClaimsOneMessagePerQuery: one logged query cancels one
// message only, so a repeat that the front end answered from its cache is
// still recorded.
func TestListenDedupClaimsOneMessagePerQuery(t *testing.T) {
	dedup := querylog.NewDedupIndex(0, 0)
	f := startListener(t, config.DnstapListener{}, dnstap.ListenOptions{Dedup: dedup})
	now := time.Now().Add(-time.Second)
	dedup.Record(&querylog.Event{Time: now, Client: client, ClientPort: 5353, Name: "r.example.", QType: dns.TypeA, QClass: dns.ClassINET})
	s := dialSender(t, f.addr)
	_, first := clientMessages(t, "r.example.", 7, 5353, now)
	_, again := clientMessages(t, "r.example.", 8, 5353, now.Add(10*time.Millisecond))
	s.send("edge-1", first)
	s.send("edge-1", again)
	waitEvents(t, f.rec, 1)
	if f.count(dnstap.ResultDuplicate) != 1 {
		t.Fatal("the first message was not claimed")
	}
	// Outside the time window nothing matches.
	dedup.Record(&querylog.Event{Time: now.Add(-time.Minute), Client: client, ClientPort: 5354, Name: "old.example.", QType: dns.TypeA, QClass: dns.ClassINET})
	_, late := clientMessages(t, "old.example.", 9, 5354, now)
	s.send("edge-1", late)
	waitEvents(t, f.rec, 2)
}

// TestListenDropsSelfIdentityAndIgnoredClients.
func TestListenDropsSelfIdentityAndIgnoredClients(t *testing.T) {
	f := startListener(t, config.DnstapListener{Identities: []string{"edge-1", "dns1"}, IgnoreClients: []string{"10.0.0.0/8"}}, dnstap.ListenOptions{Self: func(id string) bool { return id == "dns1" }})
	now := time.Now()
	s := dialSender(t, f.addr)
	_, r := clientMessages(t, "x.example.", 1, 1, now)
	s.send("dns1", r)
	s.send("other", r)
	ignored := r
	ignored.QueryAddress = []byte{10, 1, 2, 3}
	s.send("edge-1", ignored)
	s.send("edge-1", r)
	waitEvents(t, f.rec, 1)
	for result, want := range map[string]float64{dnstap.ResultSelf: 1, dnstap.ResultIdentity: 1, dnstap.ResultIgnoredClient: 1, dnstap.ResultRecorded: 1} {
		if got := f.count(result); got != want {
			t.Errorf("%s = %v, want %v", result, got, want)
		}
	}
}

// TestListenBoundsMalformedAndOversizedFrames: a payload that is not
// dnstap and a frame above the bound are counted and skipped, and the
// stream goes on; a length beyond what is read past ends the connection.
func TestListenBoundsMalformedAndOversizedFrames(t *testing.T) {
	f := startListener(t, config.DnstapListener{}, dnstap.ListenOptions{})
	now := time.Now()
	s := dialSender(t, f.addr)
	s.write(dnstap.AppendData(nil, []byte{0xff, 0xff, 0xff}))
	noQuestion := dnstap.Message{Type: dnstap.ClientResponse, ResponseMessage: []byte{1, 2, 3}}
	s.send("edge-1", noQuestion)
	s.write(dnstap.AppendData(nil, make([]byte, dnstap.MaxListenFrame+1)))
	_, r := clientMessages(t, "ok.example.", 1, 1, now)
	s.send("edge-1", r)
	waitEvents(t, f.rec, 1)
	if f.count(dnstap.ResultMalformed) != 2 || f.count(dnstap.ResultOversized) != 1 {
		t.Fatalf("malformed %v oversized %v", f.count(dnstap.ResultMalformed), f.count(dnstap.ResultOversized))
	}
	// 1 GiB announced: the listener does not read it, it hangs up.
	s.write(binary.BigEndian.AppendUint32(nil, 1<<30))
	_ = s.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := s.conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("the connection stayed open")
	}
	waitFor(t, "broken connection", func() bool { return f.conns(dnstap.ConnBroken) == 1 && f.count(dnstap.ResultOversized) == 2 })
	// A sender that does not speak dnstap is refused at the handshake.
	conn, err := net.Dial("tcp", f.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write(dnstap.AppendControl(nil, dnstap.ControlReady, "protobuf:other")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "refused handshake", func() bool { return f.conns(dnstap.ConnHandshake) == 1 })
}

// TestListenNeverBlocksTheSender: with a collector that stops taking events
// the sender still writes everything at once; what does not fit is dropped
// and counted.
func TestListenNeverBlocksTheSender(t *testing.T) {
	rec := &recorder{gate: make(chan struct{})}
	f := startListener(t, config.DnstapListener{}, dnstap.ListenOptions{Collector: rec, Queue: 8, Hold: time.Millisecond})
	s := dialSender(t, f.addr)
	_, r := clientMessages(t, "flood.example.", 1, 1, time.Now())
	payload, _ := dnstap.AppendFrame(nil, nil, &dnstap.Frame{Identity: []byte("edge-1"), Message: r})
	var batch []byte
	for range 20000 {
		batch = dnstap.AppendData(batch, payload)
	}
	start := time.Now()
	_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	s.write(batch)
	s.stop()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the sender waited %v", d)
	}
	waitFor(t, "overflow", func() bool { return f.count(dnstap.ResultOverflow) > 0 })
	close(rec.gate)
	if got := f.count(dnstap.ResultOverflow); got < 20000-30 {
		t.Fatalf("overflow %v", got)
	}
}

// TestListenNeverLoops: an event the listener recorded is never sent by a
// dnstap sink, so a node whose sink feeds its own listener (or a peer whose
// listener feeds it back) records each query once.
func TestListenNeverLoops(t *testing.T) {
	var sink *dnstap.Sink
	var mu sync.Mutex
	var resent int
	rec := &recorder{}
	f := startListener(t, config.DnstapListener{}, dnstap.ListenOptions{Collector: rec})
	sink = open(t, config.QuerylogDnstap{Target: "tcp:" + f.addr, Identity: "dns2"}, dnstap.Options{})
	t.Cleanup(func() { _ = sink.Close() })
	// The collector hands every recorded event to the dnstap sink, as the
	// collector of a node with a dnstap sink does.
	rec.after = func(ev *querylog.Event) {
		mu.Lock()
		defer mu.Unlock()
		if out := sink.AppendEvent(nil, ev); len(out) > 0 {
			resent++
		}
		_ = write(t, sink, ev)
	}
	native := forwarded(t)
	native.Wire = nil
	if err := write(t, sink, native); err != nil {
		t.Fatal(err)
	}
	events := waitEvents(t, rec, 1)
	if events[0].Origin != "dns2" || events[0].Name != "www.example.com." {
		t.Fatalf("event %+v", events[0])
	}
	mu.Lock()
	defer mu.Unlock()
	if resent != 0 {
		t.Fatal("an ingested event was sent again")
	}
}

// TestListenPeersAndConnectionLimit.
func TestListenPeersAndConnectionLimit(t *testing.T) {
	refused := startListener(t, config.DnstapListener{AllowFrom: []string{"192.0.2.0/24"}}, dnstap.ListenOptions{})
	conn, err := net.Dial("tcp", refused.addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("a peer outside allowFrom was served")
	}
	_ = conn.Close()
	if refused.conns(dnstap.ConnPeer) != 1 {
		t.Fatal("refused peer not counted")
	}
	limited := startListener(t, config.DnstapListener{MaxConnections: 1}, dnstap.ListenOptions{})
	dialSender(t, limited.addr)
	second, err := net.Dial("tcp", limited.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	_ = second.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := second.Read(make([]byte, 1)); err == nil {
		t.Fatal("a second sender was served beyond maxConnections")
	}
	waitFor(t, "limit", func() bool { return limited.conns(dnstap.ConnLimit) == 1 })
}

// TestListenUnixAndMutualTLS binds the real sockets and lets the dnstap
// sink send to them: a Unix socket and TLS with a client certificate.
func TestListenUnixAndMutualTLS(t *testing.T) {
	dir, err := os.MkdirTemp("", "dnstap")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	certPEM, keyPEM := selfSignedPair(t)
	for _, f := range []struct{ name, data string }{{"cert.pem", string(certPEM)}, {"key.pem", string(keyPEM)}} {
		if err := os.WriteFile(filepath.Join(dir, f.name), []byte(f.data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	port := func() string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		return ln.Addr().String()
	}
	tlsAddr := port()
	for _, c := range []struct {
		name     string
		listener config.DnstapListener
		sink     config.QuerylogDnstap
	}{
		{"unix", config.DnstapListener{Address: "unix:" + filepath.Join(dir, "in.sock")}, config.QuerylogDnstap{Target: "unix:" + filepath.Join(dir, "in.sock"), Identity: "edge-u"}},
		{"tls", config.DnstapListener{Address: "tls:" + tlsAddr, TLS: config.TLSFiles{CertFile: filepath.Join(dir, "cert.pem"), KeyFile: filepath.Join(dir, "key.pem")}, ClientCAFile: filepath.Join(dir, "cert.pem")},
			config.QuerylogDnstap{Target: "tls:" + tlsAddr, Identity: "edge-t", ServerName: "localhost", CARef: &config.CredentialRef{File: filepath.Join(dir, "cert.pem")}, CertRef: &config.CredentialRef{File: filepath.Join(dir, "cert.pem")}, KeyRef: &config.CredentialRef{File: filepath.Join(dir, "key.pem")}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := &recorder{}
			l, err := dnstap.Listen(dnstap.ListenOptions{Settings: c.listener, Collector: rec, Node: "dns1", Hold: 10 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- l.Serve(context.Background()) }()
			defer func() {
				_ = l.Close()
				if err := <-done; err != nil {
					t.Error(err)
				}
				if c.name == "unix" {
					if _, err := os.Stat(filepath.Join(dir, "in.sock")); !os.IsNotExist(err) {
						t.Error("the socket file stayed")
					}
				}
			}()
			if c.name == "unix" {
				st, err := os.Stat(filepath.Join(dir, "in.sock"))
				if err != nil || st.Mode().Perm() != 0o660 {
					t.Fatalf("socket mode %v %v", st, err)
				}
			}
			sink := open(t, c.sink, dnstap.Options{Resolve: func(_ context.Context, ref config.CredentialRef) ([]byte, error) { return os.ReadFile(ref.File) }})
			native := forwarded(t)
			native.Wire = nil
			if err := write(t, sink, native); err != nil {
				t.Fatal(err)
			}
			_ = sink.Close()
			events := waitEvents(t, rec, 1)
			if events[0].Origin != c.sink.Identity || events[0].Name != "www.example.com." || events[0].Latency != 3*time.Millisecond {
				t.Fatalf("event %+v", events[0])
			}
		})
	}
	// Without a client certificate the TLS listener refuses the sender.
	rec := &recorder{}
	l, err := dnstap.Listen(dnstap.ListenOptions{Settings: config.DnstapListener{Address: "tls:" + tlsAddr, TLS: config.TLSFiles{CertFile: filepath.Join(dir, "cert.pem"), KeyFile: filepath.Join(dir, "key.pem")}, ClientCAFile: filepath.Join(dir, "cert.pem")}, Collector: rec, Hold: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = l.Serve(context.Background()) }()
	defer func() { _ = l.Close() }()
	sink := open(t, config.QuerylogDnstap{Target: "tls:" + tlsAddr, ServerName: "localhost", CARef: &config.CredentialRef{File: filepath.Join(dir, "cert.pem")}}, dnstap.Options{Resolve: func(_ context.Context, ref config.CredentialRef) ([]byte, error) { return os.ReadFile(ref.File) }})
	if err := write(t, sink, authoritative()); err == nil {
		// TLS 1.3 reports a refused client certificate after the
		// handshake; nothing may be recorded either way.
		time.Sleep(300 * time.Millisecond)
	}
	if len(rec.list()) != 0 {
		t.Fatal("a sender without a client certificate was recorded")
	}
}

// selfSignedPair is a CA certificate for localhost and 127.0.0.1 that also
// serves as a client certificate.
func selfSignedPair(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, IsCA: true, BasicConstraintsValid: true,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// flakyListener fails its first Accept calls the way a process out of file
// descriptors does.
type flakyListener struct {
	net.Listener
	fails atomic.Int32
}

func (f *flakyListener) Accept() (net.Conn, error) {
	if f.fails.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept4", syscall.EMFILE)}
	}
	return f.Listener.Accept()
}

// TestListenRetriesAcceptErrors: an Accept error such as EMFILE is not a
// reason to stop. The listener counts it, waits and accepts again, so the
// node (whose runner treats a failed runnable as fatal) keeps serving DNS
// and the sender gets through; Serve returns nil once it is closed.
func TestListenRetriesAcceptErrors(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	flaky := &flakyListener{Listener: ln}
	flaky.fails.Store(3)
	rec, metrics := &recorder{}, dnstap.NewListenMetrics(nil)
	l, err := dnstap.Listen(dnstap.ListenOptions{Settings: config.DnstapListener{Address: "tcp:" + ln.Addr().String(), AllowFrom: []string{"127.0.0.1"}}, Listener: flaky, Collector: rec, Metrics: metrics, Hold: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- l.Serve(context.Background()) }()
	s := dialSender(t, ln.Addr().String())
	_, r := clientMessages(t, "after-emfile.example.", 1, 1, time.Now())
	s.send("edge-1", r)
	waitEvents(t, rec, 1)
	if got := testutil.ToFloat64(metrics.Connections.WithLabelValues(l.Name(), dnstap.ConnAcceptFailed)); got != 3 {
		t.Fatalf("accept_failed = %v, want 3", got)
	}
	select {
	case err := <-served:
		t.Fatalf("Serve ended on an accept error: %v", err)
	default:
	}
	_ = l.Close()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve after Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the listener did not stop")
	}
}

// TestListenRetryWaitEndsAtClose: Close ends the wait between retries at
// once, even when every Accept fails.
func TestListenRetryWaitEndsAtClose(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	flaky := &flakyListener{Listener: ln}
	flaky.fails.Store(1 << 30)
	l, err := dnstap.Listen(dnstap.ListenOptions{Settings: config.DnstapListener{Address: "tcp:" + ln.Addr().String(), AllowFrom: []string{"127.0.0.1"}}, Listener: flaky, Collector: &recorder{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- l.Serve(ctx) }()
	time.Sleep(1500 * time.Millisecond) // the wait has reached its 1 s cap
	start := time.Now()
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the listener did not stop")
	}
	if waited := time.Since(start); waited > 500*time.Millisecond {
		t.Fatalf("Serve took %v to stop after its context ended", waited)
	}
}

// TestListenBoundsTheLogOfAFailingAccept: an Accept that fails for good
// is retried with a growing wait, so it is not called in a tight loop, and
// every failure is counted; the log gets one line, not one per failure.
func TestListenBoundsTheLogOfAFailingAccept(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	flaky := &flakyListener{Listener: ln}
	flaky.fails.Store(1 << 30)
	var (
		mu    sync.Mutex
		lines []string
	)
	logger := slog.New(slog.NewTextHandler(writerFunc(func(b []byte) {
		mu.Lock()
		lines = append(lines, string(b))
		mu.Unlock()
	}), nil))
	metrics := dnstap.NewListenMetrics(nil)
	l, err := dnstap.Listen(dnstap.ListenOptions{Settings: config.DnstapListener{Address: "tcp:" + ln.Addr().String(), AllowFrom: []string{"127.0.0.1"}}, Listener: flaky, Collector: &recorder{}, Metrics: metrics, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- l.Serve(context.Background()) }()
	time.Sleep(3 * time.Second)
	_ = l.Close()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	failed := testutil.ToFloat64(metrics.Connections.WithLabelValues(l.Name(), dnstap.ConnAcceptFailed))
	mu.Lock()
	defer mu.Unlock()
	t.Logf("%v failed Accept calls in 3 s, %d log lines", failed, len(lines))
	// 5 ms doubling to 1 s: about 11 calls in 3 s; a tight loop makes many more.
	if failed < 5 || failed > 20 {
		t.Fatalf("accept_failed = %v in 3 s; the retry wait does not back off as it should", failed)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "failures=1") {
		t.Fatalf("%d log lines for %v failures, want 1: %q", len(lines), failed, lines)
	}
}

type writerFunc func([]byte)

func (w writerFunc) Write(b []byte) (int, error) { w(b); return len(b), nil }

// TestListenUnforwardedIdentitySkipsTheDedupCheck: the managed edge logs
// only cache hits and its own answers, so the query it forwarded before
// (a cache miss, which this node logged) is never claimed by a message of
// its own. A cache hit of the same question from the same client port a
// moment later is another client query and must be recorded; a sender that
// is not listed (an operator rule that logs relayed responses) is still
// checked.
func TestListenUnforwardedIdentitySkipsTheDedupCheck(t *testing.T) {
	dedup := querylog.NewDedupIndex(0, 0)
	f := startListener(t, config.DnstapListener{UnforwardedIdentities: []string{"edge-1"}}, dnstap.ListenOptions{Dedup: dedup})
	miss := time.Now().Add(-2 * time.Second)
	for _, name := range []string{"repeat.example.", "relayed.example."} {
		dedup.Record(&querylog.Event{Time: miss, Client: client, ClientPort: 40000, Name: name, QType: dns.TypeA, QClass: dns.ClassINET})
	}
	s := dialSender(t, f.addr)
	_, hit := clientMessages(t, "repeat.example.", 1, 40000, miss.Add(1500*time.Millisecond))
	_, relayed := clientMessages(t, "relayed.example.", 2, 40000, miss)
	s.send("edge-1", hit)
	s.send("edge-relay", relayed)
	events := waitEvents(t, f.rec, 1)
	if events[0].Name != "repeat.example." || events[0].Origin != "edge-1" {
		t.Fatalf("recorded %+v, want the cache hit", events[0])
	}
	if f.count(dnstap.ResultRecorded) != 1 || f.count(dnstap.ResultDuplicate) != 1 {
		t.Fatalf("recorded %v duplicate %v, want 1 and 1", f.count(dnstap.ResultRecorded), f.count(dnstap.ResultDuplicate))
	}
	if dedup.Len() != 1 {
		t.Fatalf("index holds %d queries; the cache miss must stay unclaimed", dedup.Len())
	}
}

// TestListenSlowAnswerIsOneRow: a front end that logs CLIENT_QUERY and
// CLIENT_RESPONSE forwards a query that kudurru answers only after 2.5 s
// (an upstream timeout). The default pairing wait outlasts kudurru's own
// pipeline bound, so the query is still waiting when kudurru's event and
// then the response arrive: one claimed pair, no row from dnstap.
func TestListenSlowAnswerIsOneRow(t *testing.T) {
	if dnstap.DefaultPairWait <= dnsserver.DefaultQueryTimeout {
		t.Fatalf("DefaultPairWait %v does not outlast the pipeline bound %v", dnstap.DefaultPairWait, dnsserver.DefaultQueryTimeout)
	}
	dedup := querylog.NewDedupIndex(0, 0)
	f := startListener(t, config.DnstapListener{}, dnstap.ListenOptions{Dedup: dedup, PairWait: dnstap.DefaultPairWait})
	s := dialSender(t, f.addr)
	at := time.Now()
	q, r := clientMessages(t, "slow.example.", 9, 41000, at)
	r.ResponseTime = at.Add(2500 * time.Millisecond)
	s.send("edge-1", q)
	time.Sleep(2500 * time.Millisecond)
	dedup.Record(&querylog.Event{Time: at.Add(time.Millisecond), Client: client, ClientPort: 41000, Name: "slow.example.", QType: dns.TypeA, QClass: dns.ClassINET})
	s.send("edge-1", r)
	waitFor(t, "the pair", func() bool { return f.count(dnstap.ResultDuplicate) == 1 })
	time.Sleep(400 * time.Millisecond)
	if n := len(f.rec.list()); n != 0 || f.count(dnstap.ResultPaired) != 1 {
		t.Fatalf("%d rows from dnstap (paired %v) for a query this node logged", n, f.count(dnstap.ResultPaired))
	}
}

// TestListenShiftsASkewedClock: a sender whose clock is an hour ahead (or
// that sends a time from 1971) does not date rows there: the row gets the
// receive time, keeps its latency, and is counted.
func TestListenShiftsASkewedClock(t *testing.T) {
	f := startListener(t, config.DnstapListener{}, dnstap.ListenOptions{})
	s := dialSender(t, f.addr)
	before := time.Now()
	_, ahead := clientMessages(t, "ahead.example.", 1, 1, before.Add(time.Hour))
	_, past := clientMessages(t, "past.example.", 2, 2, time.Unix(31536000, 0))
	_, fine := clientMessages(t, "fine.example.", 3, 3, before.Add(-time.Minute))
	s.send("edge-1", ahead)
	s.send("edge-1", past)
	s.send("edge-1", fine)
	events := waitEvents(t, f.rec, 3)
	after := time.Now()
	for _, ev := range events {
		if ev.Name == "fine.example." {
			if !ev.Time.Equal(fine.QueryTime) {
				t.Fatalf("a time within the skew bound was changed: %v, sent %v", ev.Time, fine.QueryTime)
			}
			continue
		}
		if ev.Time.Before(before.Add(-time.Second)) || ev.Time.After(after) || ev.Latency != 4*time.Millisecond {
			t.Fatalf("%s dated %v (latency %v), want between %v and %v", ev.Name, ev.Time, ev.Latency, before, after)
		}
	}
	if got := testutil.ToFloat64(f.metrics.ClockSkew.WithLabelValues(f.l.Name())); got != 2 {
		t.Fatalf("clock_skew = %v, want 2", got)
	}
}

// TestListenClampsEachTimeOfAMessage: a CLIENT_RESPONSE carries two
// times, and either can be wild while the other is sane. Each is checked on
// its own: the wild one takes the value of the sane one (the latency is then
// unknown), the row is dated from the sane time, and the message is counted.
func TestListenClampsEachTimeOfAMessage(t *testing.T) {
	f := startListener(t, config.DnstapListener{}, dnstap.ListenOptions{})
	s := dialSender(t, f.addr)
	sane := time.Now().Add(-time.Second).Truncate(time.Microsecond)
	wild := map[string]time.Time{
		"future-query.example.": sane.AddDate(50, 0, 0),
		"past-query.example.":   time.Unix(31536000, 0),
	}
	for name, at := range wild {
		_, r := clientMessages(t, name, 1, 1, at)
		r.ResponseTime = sane
		s.send("edge-1", r)
	}
	_, wildResponse := clientMessages(t, "future-response.example.", 2, 2, sane)
	wildResponse.ResponseTime = sane.AddDate(50, 0, 0)
	s.send("edge-1", wildResponse)
	events := waitEvents(t, f.rec, 3)
	for _, ev := range events {
		if !ev.Time.Equal(sane) || ev.Latency != 0 {
			t.Errorf("%s dated %v (latency %v), want %v and no latency", ev.Name, ev.Time, ev.Latency, sane)
		}
	}
	if got := testutil.ToFloat64(f.metrics.ClockSkew.WithLabelValues(f.l.Name())); got != 3 {
		t.Fatalf("clock_skew = %v, want 3", got)
	}
}

// TestListenDedupWindowUsesTheClampedTime: a front end relays the
// response to a query this node answered, with a sane response time and a
// wild query time. The dedup window is built from the clamped time, so the
// node's own event claims the message and the client query stays one row.
func TestListenDedupWindowUsesTheClampedTime(t *testing.T) {
	dedup := querylog.NewDedupIndex(0, 0)
	f := startListener(t, config.DnstapListener{}, dnstap.ListenOptions{Dedup: dedup})
	at := time.Now().Add(-time.Second)
	for i, query := range []time.Time{at.AddDate(50, 0, 0), time.Unix(31536000, 0)} {
		port := uint16(42000 + i)
		dedup.Record(&querylog.Event{Time: at, Client: client, ClientPort: port, Name: "relayed.example.", QType: dns.TypeA, QClass: dns.ClassINET})
		_, r := clientMessages(t, "relayed.example.", uint16(i+1), port, query)
		r.ResponseTime = at.Add(4 * time.Millisecond)
		s := dialSender(t, f.addr)
		s.send("edge-1", r)
		s.stop()
	}
	waitFor(t, "both claims", func() bool { return f.count(dnstap.ResultDuplicate)+f.count(dnstap.ResultRecorded) == 2 })
	time.Sleep(400 * time.Millisecond)
	if n := len(f.rec.list()); n != 0 || f.count(dnstap.ResultDuplicate) != 2 {
		t.Fatalf("%d rows from dnstap (duplicate %v) for queries this node logged", n, f.count(dnstap.ResultDuplicate))
	}
	if dedup.Len() != 0 {
		t.Fatalf("index still holds %d queries", dedup.Len())
	}
}
