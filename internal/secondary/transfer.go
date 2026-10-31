package secondary

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
)

// TransferClient polls and transfers from one primary over TCP or verified TLS. A zero value
// uses a 10-second operation deadline, one million records and 64 MiB. Configure
// it before use; concurrent calls do not share connections or transfer state.
type TransferClient struct {
	Keyring    dnsserver.TSIGKeyring
	Timeout    time.Duration
	MaxRecords int
	// MaxBytes bounds both cumulative wire bytes (including TCP framing) and
	// cumulative expanded DNS message sizes, independently. Compression cannot
	// bypass the limit. These are serialized sizes, not an exact Go heap budget.
	MaxBytes int64
	// RootCAs optionally trusts a private CA for TLS. Nil uses system trust.
	// The endpoint hostname/IP is always verified. Configure before use.
	RootCAs *x509.CertPool
}

// SOA returns an authenticated, authoritative apex SOA when TSIGKeyRef is set.
func (c *TransferClient) SOA(ctx context.Context, dz *v1alpha1.DNSZone, primary string) (*dns.SOA, error) {
	s, err := c.open(ctx, dz, primary, dns.TypeSOA)
	if err != nil {
		return nil, err
	}
	defer s.close()
	m, wire, err := s.read()
	if err != nil {
		return nil, err
	}
	if err := s.authenticate(m, wire, true); err != nil {
		return nil, err
	}
	if err := s.validate(m, true); err != nil {
		return nil, err
	}
	if len(m.Answer) != 1 {
		return nil, fmt.Errorf("secondary: SOA reply must contain exactly one answer")
	}
	soa, ok := m.Answer[0].(*dns.SOA)
	if !ok || !s.apexSOA(soa) {
		return nil, fmt.Errorf("secondary: SOA reply does not contain the zone's IN SOA")
	}
	return soa, nil
}

// AXFR returns raw records, with exactly one initial SOA and no closing
// duplicate. It returns no partial data on failure. The caller builds and
// publishes an immutable zone only after this method succeeds.
func (c *TransferClient) AXFR(ctx context.Context, dz *v1alpha1.DNSZone, primary string) ([]dns.RR, error) {
	return c.axfr(ctx, dz, primary, 0, 0)
}

func (c *TransferClient) axfr(ctx context.Context, dz *v1alpha1.DNSZone, primary string, wireBytes, decodeBytes int64) ([]dns.RR, error) {
	s, err := c.open(ctx, dz, primary, dns.TypeAXFR)
	if err != nil {
		return nil, err
	}
	defer s.close()
	s.wireBytes, s.decodeBytes = wireBytes, decodeBytes
	var records []dns.RR
	var initial *dns.SOA
	for {
		m, wire, err := s.read()
		if err != nil {
			return nil, fmt.Errorf("secondary: incomplete AXFR: %w", err)
		}
		first := initial == nil
		if err := s.authenticate(m, wire, first); err != nil {
			return nil, err
		}
		if err := s.validate(m, first); err != nil {
			return nil, err
		}
		if len(m.Answer) == 0 || len(m.Ns) != 0 {
			return nil, fmt.Errorf("secondary: AXFR requires answers and an empty authority section")
		}
		for i, rr := range m.Answer {
			if err := s.validateRR(rr); err != nil {
				return nil, err
			}
			soa, isSOA := rr.(*dns.SOA)
			if initial == nil {
				if !isSOA || !s.apexSOA(soa) {
					return nil, fmt.Errorf("secondary: AXFR must start with the zone's SOA")
				}
				initial = soa
			} else if isSOA {
				if !sameTransferSOA(initial, soa) || i != len(m.Answer)-1 {
					return nil, fmt.Errorf("secondary: mismatched or misplaced closing AXFR SOA")
				}
				if s.key != "" && m.IsTsig() == nil {
					return nil, fmt.Errorf("secondary: closing AXFR message: %w", dns.ErrNoSig)
				}
				if err := s.ctx.Err(); err != nil {
					return nil, err
				}
				return records, nil
			}
			if len(records) >= s.maxRecords {
				return nil, fmt.Errorf("secondary: AXFR exceeds record limit (%d)", s.maxRecords)
			}
			records = append(records, rr)
		}
	}
}

type transferSession struct {
	ctx         context.Context
	cancel      context.CancelFunc
	conn        net.Conn
	stop        func() bool
	closed      chan struct{}
	closeOnce   sync.Once
	query       *dns.Msg
	origin      string
	key, alg    string
	provider    dns.TsigProvider
	mac         string
	unsigned    []byte
	unsignedN   int
	seen        bool
	maxRecords  int
	maxBytes    int64
	wireBytes   int64
	decodeBytes int64
}

func (c *TransferClient) open(ctx context.Context, dz *v1alpha1.DNSZone, primary string, qtype uint16) (*transferSession, error) {
	return c.openQuery(ctx, dz, primary, qtype, nil)
}

func (c *TransferClient) openQuery(ctx context.Context, dz *v1alpha1.DNSZone, primary string, qtype uint16, base *dns.SOA) (*transferSession, error) {
	if dz == nil || dz.Spec.Secondary == nil {
		return nil, fmt.Errorf("secondary: missing secondary configuration")
	}
	protocol := dz.Spec.Secondary.TransferProtocol
	if protocol != "" && protocol != "tcp" && protocol != "tls" {
		return nil, fmt.Errorf("secondary: unsupported transfer protocol %q", protocol)
	}
	origin := dns.CanonicalName(dz.Spec.ZoneName)
	if _, ok := dns.IsDomainName(origin); !ok || dz.Spec.ZoneName == "" {
		return nil, fmt.Errorf("secondary: invalid zone name")
	}
	address, err := transferAddress(primary)
	if err != nil {
		return nil, err
	}
	timeout, maxRecords, maxBytes := c.Timeout, c.MaxRecords, c.MaxBytes
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if maxRecords <= 0 {
		maxRecords = 1_000_000
	}
	if maxBytes <= 0 {
		maxBytes = 64 << 20
	}
	s := &transferSession{origin: origin, maxRecords: maxRecords, maxBytes: maxBytes, provider: dnsserver.TSIGProvider(c.Keyring)}
	s.query = new(dns.Msg)
	s.query.SetQuestion(origin, qtype)
	s.query.RecursionDesired = false
	if base != nil {
		s.query.Ns = []dns.RR{dns.Copy(base)}
	}
	var wire []byte
	if ref := dz.Spec.Secondary.TSIGKeyRef; ref != "" {
		s.key = dnsserver.TSIGKeyName(ref)
		if c.Keyring == nil {
			return nil, fmt.Errorf("secondary: TSIG key unavailable: %w", dns.ErrSecret)
		}
		_, algorithm, ok := c.Keyring.Lookup(s.key)
		if !ok {
			return nil, fmt.Errorf("secondary: TSIG key %q unavailable: %w", s.key, dns.ErrSecret)
		}
		if s.alg, ok = dnsserver.TSIGAlgorithm(algorithm); !ok {
			return nil, fmt.Errorf("secondary: unsupported TSIG algorithm: %w", dns.ErrKeyAlg)
		}
		s.query.SetTsig(s.key, s.alg, 300, time.Now().Unix())
		wire, s.mac, err = dns.TsigGenerateWithProvider(s.query, s.provider, "", false)
	} else {
		wire, err = s.query.Pack()
	}
	if err != nil {
		return nil, fmt.Errorf("secondary: pack query: %w", err)
	}
	if len(wire) > dns.MaxMsgSize {
		return nil, fmt.Errorf("secondary: query exceeds DNS frame size")
	}
	s.ctx, s.cancel = context.WithTimeout(ctx, timeout)
	if protocol == "tls" {
		host, _, _ := net.SplitHostPort(address)
		dialer := &tls.Dialer{Config: &tls.Config{
			MinVersion: tls.VersionTLS12, RootCAs: c.RootCAs,
			ServerName: strings.TrimSuffix(host, "."), NextProtos: []string{"dot"},
		}}
		s.conn, err = dialer.DialContext(s.ctx, "tcp", address)
	} else {
		s.conn, err = (&net.Dialer{}).DialContext(s.ctx, "tcp", address)
	}
	if err != nil {
		err = s.ioError(err)
		s.cancel()
		return nil, fmt.Errorf("secondary: connect: %w", err)
	}
	s.closed = make(chan struct{})
	s.stop = context.AfterFunc(s.ctx, func() {
		_ = s.conn.Close()
		close(s.closed)
	})
	deadline, _ := s.ctx.Deadline()
	if err = s.conn.SetDeadline(deadline); err == nil {
		frame := make([]byte, 2, len(wire)+2)
		binary.BigEndian.PutUint16(frame, uint16(len(wire))) //nolint:gosec // G115: checked against MaxMsgSize above.
		frame = append(frame, wire...)
		var n int
		n, err = s.conn.Write(frame)
		if err == nil && n != len(frame) {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		err = s.ioError(err)
		s.close()
		return nil, fmt.Errorf("secondary: write query: %w", err)
	}
	return s, nil
}

func transferAddress(primary string) (string, error) {
	if primary == "" || strings.ContainsAny(primary, "/ \t\r\n") {
		return "", fmt.Errorf("secondary: primary must be a host/IP with an optional port")
	}
	host, port, err := net.SplitHostPort(primary)
	if err != nil {
		host, port = primary, "53"
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			host = host[1 : len(host)-1]
		}
		if _, err := netip.ParseAddr(host); err != nil && strings.ContainsAny(host, ":[]") {
			return "", fmt.Errorf("secondary: invalid primary address %q", primary)
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || host == "" {
		return "", fmt.Errorf("secondary: invalid primary host or port %q", primary)
	}
	if strings.HasPrefix(primary, "[") {
		if _, err := netip.ParseAddr(host); err != nil {
			return "", fmt.Errorf("secondary: bracketed primary must be an IP address")
		}
	}
	return net.JoinHostPort(host, port), nil
}

func (s *transferSession) close() {
	s.closeOnce.Do(func() {
		if !s.stop() {
			<-s.closed
		}
		_ = s.conn.Close()
		s.cancel()
	})
}

func (s *transferSession) ioError(err error) error {
	if ctxErr := s.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if deadline, ok := s.ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return err
}

func (s *transferSession) read() (*dns.Msg, []byte, error) {
	var size [2]byte
	if _, err := io.ReadFull(s.conn, size[:]); err != nil {
		return nil, nil, s.ioError(err)
	}
	n := int(binary.BigEndian.Uint16(size[:]))
	if n < 12 {
		return nil, nil, fmt.Errorf("secondary: malformed DNS TCP frame")
	}
	if int64(n)+2 > s.maxBytes-s.wireBytes {
		return nil, nil, fmt.Errorf("secondary: transfer exceeds wire byte limit (%d)", s.maxBytes)
	}
	s.wireBytes += int64(n) + 2
	wire := make([]byte, n)
	if _, err := io.ReadFull(s.conn, wire); err != nil {
		return nil, nil, s.ioError(err)
	}
	m, err := s.unpack(wire)
	return m, wire, err
}

// unpack is deliberately strict: Msg.Unpack accepts some truncated section
// counts and trailing bytes. Decode one RR at a time so expanded/compressed
// records are charged before a whole message can be accumulated.
func (s *transferSession) unpack(wire []byte) (*dns.Msg, error) {
	bits := binary.BigEndian.Uint16(wire[2:4])
	m := &dns.Msg{MsgHdr: dns.MsgHdr{
		Id: binary.BigEndian.Uint16(wire[:2]), Response: bits&0x8000 != 0,
		Opcode: int(bits>>11) & 15, Authoritative: bits&0x0400 != 0,
		Truncated: bits&0x0200 != 0, RecursionDesired: bits&0x0100 != 0,
		RecursionAvailable: bits&0x0080 != 0, Zero: bits&0x0040 != 0,
		AuthenticatedData: bits&0x0020 != 0, CheckingDisabled: bits&0x0010 != 0,
		Rcode: int(bits & 15),
	}}
	charge := func(n int) error {
		if int64(n) > s.maxBytes-s.decodeBytes {
			return fmt.Errorf("secondary: transfer exceeds expanded byte limit (%d)", s.maxBytes)
		}
		s.decodeBytes += int64(n)
		return nil
	}
	if err := charge(12); err != nil {
		return nil, err
	}
	qd := int(binary.BigEndian.Uint16(wire[4:6]))
	if qd > 1 {
		return nil, fmt.Errorf("secondary: too many response questions")
	}
	off := 12
	if qd == 1 {
		name, next, err := dns.UnpackDomainName(wire, off)
		if err != nil || next+4 > len(wire) {
			return nil, fmt.Errorf("secondary: malformed response question")
		}
		m.Question = []dns.Question{{Name: name, Qtype: binary.BigEndian.Uint16(wire[next:]), Qclass: binary.BigEndian.Uint16(wire[next+2:])}}
		off = next + 4
		if err := charge(len(name) + 5); err != nil {
			return nil, err
		}
	}
	sections := []*[]dns.RR{&m.Answer, &m.Ns, &m.Extra}
	for i, section := range sections {
		count := int(binary.BigEndian.Uint16(wire[6+i*2:]))
		for range count {
			rr, next, err := dns.UnpackRR(wire, off)
			if err != nil {
				return nil, fmt.Errorf("secondary: malformed response record: %w", err)
			}
			if next <= off {
				return nil, fmt.Errorf("secondary: response record did not advance the frame")
			}
			// UnpackRR accepts zero RDLENGTH even for types that require data.
			// Empty NULL, APL and OPT records, and opaque unknown types, are
			// legitimate; known types such as A, TXT and SOA require RDATA.
			if h := rr.Header(); h.Rdlength == 0 {
				switch h.Rrtype {
				case dns.TypeNULL, dns.TypeAPL, dns.TypeOPT:
				default:
					if _, known := dns.TypeToRR[h.Rrtype]; known {
						return nil, fmt.Errorf("secondary: missing response record data")
					}
				}
			}
			if err := charge(dns.Len(rr)); err != nil {
				return nil, err
			}
			*section = append(*section, rr)
			off = next
		}
	}
	if off != len(wire) {
		return nil, fmt.Errorf("secondary: trailing bytes in DNS frame")
	}
	return m, nil
}

func (s *transferSession) authenticate(m *dns.Msg, wire []byte, required bool) error {
	var ts *dns.TSIG
	for i, rr := range m.Extra {
		if sig, ok := rr.(*dns.TSIG); ok {
			if ts != nil || i != len(m.Extra)-1 {
				return fmt.Errorf("secondary: TSIG must appear once, last in additional")
			}
			ts = sig
		}
	}
	if s.key == "" {
		if ts != nil {
			return fmt.Errorf("secondary: unexpected TSIG on unsigned transfer")
		}
		return nil
	}
	if ts == nil {
		if required {
			return fmt.Errorf("secondary: required TSIG missing: %w", dns.ErrNoSig)
		}
		s.unsignedN++
		if s.unsignedN >= 100 {
			return fmt.Errorf("secondary: more than 99 unsigned transfer intermediate messages")
		}
		s.unsigned = append(s.unsigned, wire...)
		return nil
	}
	if dnsserver.TSIGKeyName(ts.Hdr.Name) != s.key || dns.CanonicalName(ts.Algorithm) != s.alg {
		return fmt.Errorf("secondary: response uses the wrong TSIG key or algorithm: %w", dns.ErrKeyAlg)
	}
	switch ts.Error {
	case dns.RcodeBadTime:
		return fmt.Errorf("secondary: TSIG BADTIME: %w", dns.ErrTime)
	case dns.RcodeBadSig:
		return fmt.Errorf("secondary: TSIG BADSIG: %w", dns.ErrSig)
	case dns.RcodeBadKey:
		return fmt.Errorf("secondary: TSIG BADKEY: %w", dns.ErrSecret)
	case 0:
	default:
		return fmt.Errorf("secondary: TSIG error %d", ts.Error)
	}
	if ts.TimeSigned == 0 {
		// miekg defaults a zero timestamp while building the verification
		// buffer. It must not turn an expired wire timestamp into a fresh one.
		return fmt.Errorf("secondary: zero TSIG timestamp: %w", dns.ErrTime)
	}
	if ts.Hdr.Class != dns.ClassANY || ts.Hdr.Ttl != 0 || ts.OrigId != s.query.Id || ts.MACSize == 0 || ts.OtherLen != 0 {
		return fmt.Errorf("secondary: malformed TSIG")
	}
	provider := s.provider
	if len(s.unsigned) != 0 {
		provider = intermediateProvider{TsigProvider: provider, unsigned: s.unsigned, prefix: 2 + len(s.mac)/2}
	}
	if err := dns.TsigVerifyWithProvider(wire, provider, s.mac, s.seen); err != nil {
		return fmt.Errorf("secondary: verify TSIG: %w", err)
	}
	s.mac, s.seen = ts.MAC, true
	s.unsigned, s.unsignedN = nil, 0
	return nil
}

// RFC 8945 §5.3.1: unsigned intermediate messages participate in the next
// digest, after the previous MAC and before the signed message and its timers.
// The provider remains the existing keyring adapter. Accumulation is bounded
// by MaxBytes and the mandatory signature interval (at most 99 messages).
type intermediateProvider struct {
	dns.TsigProvider
	unsigned []byte
	prefix   int
}

func (p intermediateProvider) Verify(buf []byte, ts *dns.TSIG) error {
	if len(buf) < p.prefix {
		return dns.ErrSig
	}
	combined := make([]byte, 0, len(buf)+len(p.unsigned))
	combined = append(combined, buf[:p.prefix]...)
	combined = append(combined, p.unsigned...)
	combined = append(combined, buf[p.prefix:]...)
	return p.TsigProvider.Verify(combined, ts)
}

func (s *transferSession) validate(m *dns.Msg, first bool) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if !m.Response || m.Id != s.query.Id || m.Opcode != dns.OpcodeQuery || m.Truncated || m.Zero {
		return fmt.Errorf("secondary: invalid response header or truncated response")
	}
	if first && len(m.Question) != 1 {
		return fmt.Errorf("secondary: first response must echo the question")
	}
	if len(m.Question) == 1 {
		q, want := m.Question[0], s.query.Question[0]
		if dns.CanonicalName(q.Name) != s.origin || q.Qtype != want.Qtype || q.Qclass != want.Qclass {
			return fmt.Errorf("secondary: response question does not match request")
		}
	}
	optSeen := false
	for _, rr := range m.Extra {
		switch rr := rr.(type) {
		case *dns.TSIG:
		case *dns.OPT:
			if optSeen || rr.Hdr.Name != "." || rr.ExtendedRcode() != 0 {
				return fmt.Errorf("secondary: invalid response OPT")
			}
			optSeen = true
		default:
			if s.query.Question[0].Qtype != dns.TypeSOA {
				return fmt.Errorf("secondary: unexpected transfer additional record")
			}
		}
	}
	if m.Rcode != dns.RcodeSuccess {
		return &transferRcodeError{code: m.Rcode}
	}
	if !m.Authoritative {
		return fmt.Errorf("secondary: non-authoritative response")
	}
	return nil
}

type transferRcodeError struct{ code int }

func (e *transferRcodeError) Error() string {
	return fmt.Sprintf("secondary: primary returned DNS rcode %s (%d)", dns.RcodeToString[e.code], e.code)
}

func (s *transferSession) validateRR(rr dns.RR) error {
	h := rr.Header()
	if h.Class != dns.ClassINET || !dns.IsSubDomain(s.origin, h.Name) {
		return fmt.Errorf("secondary: transfer record %q has the wrong class or is out of zone", h.Name)
	}
	switch h.Rrtype {
	case 0, dns.TypeOPT, dns.TypeTSIG, dns.TypeTKEY, dns.TypeAXFR, dns.TypeIXFR, dns.TypeANY, dns.TypeMAILA, dns.TypeMAILB:
		return fmt.Errorf("secondary: transfer contains a meta-type record")
	}
	return nil
}

func (s *transferSession) apexSOA(soa *dns.SOA) bool {
	return soa.Hdr.Class == dns.ClassINET && dns.CanonicalName(soa.Hdr.Name) == s.origin
}

func sameTransferSOA(a, b *dns.SOA) bool {
	return dns.CanonicalName(a.Hdr.Name) == dns.CanonicalName(b.Hdr.Name) &&
		a.Hdr.Class == b.Hdr.Class && a.Hdr.Ttl == b.Hdr.Ttl &&
		dns.CanonicalName(a.Ns) == dns.CanonicalName(b.Ns) && dns.CanonicalName(a.Mbox) == dns.CanonicalName(b.Mbox) &&
		a.Serial == b.Serial && a.Refresh == b.Refresh && a.Retry == b.Retry && a.Expire == b.Expire && a.Minttl == b.Minttl
}
