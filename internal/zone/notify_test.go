package zone

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// secondary is a fake secondary: it records the NOTIFY messages it gets
// and answers with rcode (unless silent), signing with the request's key
// when the request was signed and verified.
type secondary struct {
	addr   netip.AddrPort
	mu     sync.Mutex
	got    []*dns.Msg
	status []error
	rcode  int
	silent bool
	unsign bool
}

func startSecondary(t *testing.T, secrets map[string]string) *secondary {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &secondary{addr: netip.MustParseAddrPort(pc.LocalAddr().String())}
	srv := &dns.Server{PacketConn: pc, TsigSecret: secrets, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		s.mu.Lock()
		s.got = append(s.got, r)
		s.status = append(s.status, w.TsigStatus())
		silent, rcode, unsign := s.silent, s.rcode, s.unsign
		s.mu.Unlock()
		if silent {
			return
		}
		m := new(dns.Msg)
		m.SetRcode(r, rcode)
		if ts := r.IsTsig(); ts != nil && w.TsigStatus() == nil && !unsign {
			m.SetTsig(ts.Hdr.Name, ts.Algorithm, 300, time.Now().Unix())
		}
		_ = w.WriteMsg(m)
	})}
	started := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(started) }
	go func() { _ = srv.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = srv.Shutdown() })
	return s
}

func (s *secondary) set(rcode int, silent, unsign bool) {
	s.mu.Lock()
	s.rcode, s.silent, s.unsign = rcode, silent, unsign
	s.mu.Unlock()
}

func (s *secondary) last(t *testing.T) (*dns.Msg, error) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.got) == 0 {
		t.Fatal("secondary got nothing")
	}
	return s.got[len(s.got)-1], s.status[len(s.status)-1]
}

func TestNotifyMessage(t *testing.T) {
	z := xfrZone(t, 1)
	m := NotifyMessage(Notify{Zone: "example.org", Serial: 42, SOA: z.SOA()})
	if m.Opcode != dns.OpcodeNotify || !m.Authoritative || len(m.Question) != 1 || m.Question[0].Qtype != dns.TypeSOA || m.Question[0].Name != "example.org." {
		t.Fatalf("header/question %v", m)
	}
	soa, ok := m.Answer[0].(*dns.SOA)
	if !ok || soa.Serial != 42 || soa.Ns != z.SOA().Ns {
		t.Fatalf("answer %v", m.Answer)
	}
	if z.SOA().Serial == 42 {
		t.Fatal("the zone's SOA must not be modified")
	}
	if bare := NotifyMessage(Notify{Zone: "example.org.", Serial: 7}); bare.Answer[0].(*dns.SOA).Serial != 7 {
		t.Fatal("synthesised SOA carries the serial")
	}
}

func TestSendNotify(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	sec := startSecondary(t, map[string]string{"xfr-key.": secret})
	ctx := context.Background()
	base := Notify{Zone: "example.org.", Serial: 2026101101, Target: sec.addr, Timeout: 300 * time.Millisecond}

	if err := SendNotify(ctx, base); err != nil {
		t.Fatalf("unsigned: %v", err)
	}
	if m, _ := sec.last(t); m.Opcode != dns.OpcodeNotify || m.Answer[0].(*dns.SOA).Serial != 2026101101 {
		t.Fatalf("secondary got %v", m)
	}

	signed := base
	signed.KeyName, signed.Algorithm, signed.TSIG = "XFR-Key", "hmac-sha256", secretProvider{"xfr-key.": secret}
	if err := SendNotify(ctx, signed); err != nil {
		t.Fatalf("signed: %v", err)
	}
	if m, status := sec.last(t); m.IsTsig() == nil || status != nil {
		t.Fatalf("secondary verification: %v %v", m.IsTsig(), status)
	}

	for _, tc := range []struct {
		name   string
		n      Notify
		rcode  int
		silent bool
		unsign bool
		class  string
	}{
		{"refused", base, dns.RcodeRefused, false, false, "rcode"},
		{"silence", base, 0, true, false, "timeout"},
		{"unsigned-reply", signed, 0, false, true, "tsig"},
		{"wrong-secret", func() Notify {
			n := signed
			n.TSIG = secretProvider{"xfr-key.": base64.StdEncoding.EncodeToString([]byte("wrong-wrong-wrong-wrong-wrong!!!"))}
			return n
		}(), 0, false, false, "tsig"},
		{"no-provider", func() Notify { n := signed; n.TSIG = nil; return n }(), 0, false, false, "tsig"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sec.set(tc.rcode, tc.silent, tc.unsign)
			defer sec.set(0, false, false)
			err := SendNotify(ctx, tc.n)
			var ne *NotifyError
			if !errors.As(err, &ne) || ne.Class != tc.class {
				t.Fatalf("error %v, want class %s", err, tc.class)
			}
			if ne.Error() == "" || (tc.class != "rcode" && ne.Unwrap() == nil) {
				t.Fatal("error must describe itself and wrap the cause")
			}
		})
	}
}

// secretProvider is an HMAC-SHA256 TsigProvider over base64 secrets by
// key name, for the client side of the tests.
type secretProvider map[string]string

func (p secretProvider) Generate(msg []byte, t *dns.TSIG) ([]byte, error) {
	s, ok := p[dns.CanonicalName(t.Hdr.Name)]
	if !ok {
		return nil, dns.ErrSecret
	}
	key, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	h := hmac.New(sha256.New, key)
	h.Write(msg)
	return h.Sum(nil), nil
}

func (p secretProvider) Verify(msg []byte, t *dns.TSIG) error {
	mac, err := p.Generate(msg, t)
	if err != nil {
		return err
	}
	if hex.EncodeToString(mac) != strings.ToLower(t.MAC) {
		return dns.ErrSig
	}
	return nil
}
