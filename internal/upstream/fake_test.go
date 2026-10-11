package upstream

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// behaviour is what a fake server does with a query.
type behaviour int

const (
	answer   behaviour = iota // A record with the server's identity IP
	drop                      // never reply (client times out)
	servfail                  // SERVFAIL
	refused                   // REFUSED
	truncate                  // UDP: TC=1 without answers; TCP: full answer
)

// fake is an in-process DNS server on 127.0.0.1 serving UDP and TCP on one
// port, or DoT when started with startDoT.
type fake struct {
	addr  string // host:port
	ip    net.IP // identity returned in A answers
	calls atomic.Int64
	tcp   atomic.Int64 // queries that arrived over TCP or TLS

	mu       sync.Mutex
	mode     behaviour
	delay    time.Duration
	answerIP net.IP   // what A answers carry; ip unless changed
	last     *dns.Msg // last query received
}

func (f *fake) set(mode behaviour, delay time.Duration) {
	f.mu.Lock()
	f.mode, f.delay = mode, delay
	f.mu.Unlock()
}

func (f *fake) lastQuery() *dns.Msg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

func (f *fake) handler(proto string) dns.HandlerFunc {
	return func(w dns.ResponseWriter, r *dns.Msg) {
		f.calls.Add(1)
		if proto != "udp" {
			f.tcp.Add(1)
		}
		f.mu.Lock()
		mode, delay, ip := f.mode, f.delay, f.answerIP
		f.last = r.Copy()
		f.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		m := new(dns.Msg)
		m.SetReply(r)
		switch mode {
		case drop:
			return
		case servfail:
			m.Rcode = dns.RcodeServerFailure
		case refused:
			m.Rcode = dns.RcodeRefused
		case truncate:
			if proto == "udp" {
				m.Truncated = true
				break
			}
			fallthrough
		default:
			q := r.Question[0]
			m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300}, A: ip}}
			if q.Name == "big.test." {
				// Many records so the answer only fits over TCP.
				for i := range 60 {
					m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300}, A: net.IPv4(10, 0, 0, byte(i))})
				}
			}
		}
		if opt := r.IsEdns0(); opt != nil {
			m.SetEdns0(opt.UDPSize(), opt.Do())
			for _, o := range opt.Option {
				if s, ok := o.(*dns.EDNS0_SUBNET); ok {
					echo := *s
					echo.SourceScope = s.SourceNetmask
					m.IsEdns0().Option = append(m.IsEdns0().Option, &echo)
				}
			}
		}
		if proto == "udp" {
			size := dns.MinMsgSize
			if opt := r.IsEdns0(); opt != nil {
				size = int(opt.UDPSize())
			}
			m.Truncate(size)
		}
		_ = w.WriteMsg(m)
	}
}

var identity atomic.Uint32

func nextIP() net.IP {
	n := identity.Add(1)
	return net.IPv4(192, 0, 2, byte(n%250+1))
}

// startFake serves UDP and TCP on the same loopback port.
func startFake(t testing.TB) *fake {
	t.Helper()
	f := &fake{ip: nextIP()}
	f.answerIP = f.ip
	var pc net.PacketConn
	var ln net.Listener
	for range 20 {
		var err error
		pc, err = net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := pc.LocalAddr().(*net.UDPAddr).Port
		ln, err = net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err == nil {
			break
		}
		_ = pc.Close()
		pc = nil
	}
	if pc == nil {
		t.Fatal("no free udp+tcp port pair")
	}
	f.addr = pc.LocalAddr().String()
	udp := &dns.Server{PacketConn: pc, Handler: f.handler("udp")}
	tcp := &dns.Server{Listener: ln, Handler: f.handler("tcp")}
	serve(t, udp, tcp)
	return f
}

// startDoT serves DNS over TLS with a throw-away certificate trusted by the
// returned pool.
func startDoT(t testing.TB) (*fake, *x509.CertPool) {
	t.Helper()
	cert, pool := selfSigned(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{ip: nextIP(), addr: ln.Addr().String()}
	f.answerIP = f.ip
	serve(t, &dns.Server{Listener: ln, Net: "tcp-tls", Handler: f.handler("tls")})
	return f, pool
}

func serve(t testing.TB, servers ...*dns.Server) {
	t.Helper()
	for _, s := range servers {
		started := make(chan struct{})
		s.NotifyStartedFunc = func() { close(started) }
		go func() { _ = s.ActivateAndServe() }()
		<-started
		t.Cleanup(func() { _ = s.Shutdown() })
	}
}

func selfSigned(t testing.TB) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "kudurru upstream test"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// fakeClock is a settable clock for backoff tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_800_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func query(name string, qtype uint16) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	return m
}

// answerIP returns the first A record's address, or nil.
func answerIP(m *dns.Msg) net.IP {
	if m == nil {
		return nil
	}
	for _, rr := range m.Answer {
		if a, ok := rr.(*dns.A); ok {
			return a.A
		}
	}
	return nil
}
