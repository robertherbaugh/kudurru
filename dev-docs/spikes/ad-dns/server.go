package main

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"log/slog"
	"math"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

const gssAlgorithm = "gss-tsig."

// probe deliberately refuses every UPDATE. It can establish and authenticate
// contexts without pretending that authentication implements authorization,
// atomic writes, persistence or Windows DNS replacement.
type probe struct {
	acceptor *acceptor
	logger   *slog.Logger
	allowed  []netip.Prefix
	zone     string
	hostname string
	mu       sync.Mutex
	contexts map[string]*securityContext
	// onVerified is test instrumentation; no token or key material is emitted.
	onVerified func(string)
}

func (p *probe) lookup(t *dns.TSIG) (*securityContext, error) {
	if dns.CanonicalName(t.Algorithm) != gssAlgorithm {
		return nil, dns.ErrKeyAlg
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.contexts[dns.CanonicalName(t.Hdr.Name)]
	if c == nil || !time.Now().Before(c.expires) {
		return nil, dns.ErrSecret
	}
	return c, nil
}

func (p *probe) Generate(payload []byte, t *dns.TSIG) ([]byte, error) {
	c, err := p.lookup(t)
	if err != nil {
		return nil, err
	}
	return c.sign(payload)
}

func (p *probe) Verify(payload []byte, t *dns.TSIG) error {
	c, err := p.lookup(t)
	if err != nil {
		return err
	}
	mac, err := hex.DecodeString(t.MAC)
	if err != nil || c.verify(payload, mac) != nil {
		return dns.ErrSig
	}
	return nil
}

func (p *probe) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetRcode(r, dns.RcodeRefused)
	peer, err := netip.ParseAddrPort(w.RemoteAddr().String())
	permitted := false
	if err == nil {
		for _, prefix := range p.allowed {
			permitted = permitted || prefix.Contains(peer.Addr().Unmap())
		}
	}
	if !permitted {
		p.write(w, m)
		return
	}
	p.logger.Debug("probe request", "opcode", r.Opcode, "signed", r.IsTsig() != nil, "questions", len(r.Question))
	if t := r.IsTsig(); t != nil {
		if w.TsigStatus() != nil {
			// Invalid signatures must never reach the authenticated observation.
			m.Rcode = dns.RcodeNotAuth
			failure := &dns.TSIG{
				Hdr:       dns.RR_Header{Name: t.Hdr.Name, Rrtype: dns.TypeTSIG, Class: dns.ClassANY},
				Algorithm: t.Algorithm, OrigId: r.Id, TimeSigned: t.TimeSigned, Fudge: t.Fudge, Error: dns.RcodeBadKey,
			}
			if errors.Is(w.TsigStatus(), dns.ErrTime) {
				failure.Error = dns.RcodeBadTime
				var now [8]byte
				binary.BigEndian.PutUint64(now[:], uint64(time.Now().Unix())) //nolint:gosec // G115: current Unix time is positive.
				failure.OtherData = hex.EncodeToString(now[2:])
				failure.OtherLen = 6
			}
			m.Extra = []dns.RR{failure}
			p.write(w, m)
			return
		}
		m.SetTsig(t.Hdr.Name, gssAlgorithm, 300, time.Now().Unix())
		if r.Opcode == dns.OpcodeUpdate {
			if c, err := p.lookup(t); err == nil {
				p.logger.Info("authenticated update refused", "principal", c.principal, "changes", len(r.Ns))
				if p.onVerified != nil {
					p.onVerified(c.principal)
				}
			}
		}
		p.write(w, m)
		return
	}
	if r.Opcode == dns.OpcodeQuery && len(r.Question) == 1 && r.Question[0].Qtype == dns.TypeTKEY {
		p.negotiate(m, r)
	} else if r.Opcode == dns.OpcodeQuery && len(r.Question) == 1 && p.zone != "" {
		q := r.Question[0]
		if q.Qclass == dns.ClassINET && dns.IsSubDomain(p.zone, dns.CanonicalName(q.Name)) {
			soa := &dns.SOA{Hdr: dns.RR_Header{Name: p.zone, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 60},
				Ns: p.hostname, Mbox: "hostmaster." + p.zone, Serial: 1, Refresh: 60, Retry: 60, Expire: 3600, Minttl: 60}
			m.Authoritative = true
			m.Rcode = dns.RcodeSuccess
			if dns.CanonicalName(q.Name) == p.zone && q.Qtype == dns.TypeSOA {
				m.Answer = []dns.RR{soa}
			} else {
				m.Ns = []dns.RR{soa}
				if dns.CanonicalName(q.Name) != p.zone {
					m.Rcode = dns.RcodeNameError
				}
			}
		}
	}
	p.write(w, m)
}

func (p *probe) write(w dns.ResponseWriter, m *dns.Msg) {
	if err := w.WriteMsg(m); err != nil {
		p.logger.Debug("DNS response write failed")
	}
}

func (p *probe) negotiate(m, r *dns.Msg) {
	m.Rcode = dns.RcodeFormatError
	var t *dns.TKEY
	for _, rr := range r.Extra {
		if found, ok := rr.(*dns.TKEY); ok {
			if t != nil {
				return
			}
			t = found
		}
	}
	if t == nil || t.Hdr.Class != dns.ClassANY || t.Hdr.Ttl != 0 ||
		!strings.EqualFold(t.Hdr.Name, r.Question[0].Name) ||
		t.Error != 0 || t.OtherLen != 0 || t.OtherData != "" || len(r.Answer) != 0 || len(r.Ns) != 0 {
		return
	}
	name := dns.CanonicalName(t.Hdr.Name)
	if _, ok := dns.IsDomainName(name); !ok || name == "." {
		return
	}
	response := &dns.TKEY{
		Hdr:       dns.RR_Header{Name: t.Hdr.Name, Rrtype: dns.TypeTKEY, Class: dns.ClassANY},
		Algorithm: gssAlgorithm, Mode: t.Mode,
	}
	m.Rcode = dns.RcodeSuccess
	m.Answer = []dns.RR{response}
	if t.Mode != 3 {
		response.Error = dns.RcodeBadMode
		return
	}
	if dns.CanonicalName(t.Algorithm) != gssAlgorithm {
		response.Error = dns.RcodeBadAlg
		return
	}
	input, err := hex.DecodeString(t.Key)
	if err != nil || len(input) == 0 || len(input) > 32768 || len(input) != int(t.KeySize) {
		m.Rcode = dns.RcodeFormatError
		m.Answer = nil
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for key, c := range p.contexts {
		if !now.Before(c.expires) {
			delete(p.contexts, key)
		}
	}
	if _, exists := p.contexts[name]; exists || len(p.contexts) >= 128 {
		response.Error = dns.RcodeBadName
		return
	}
	c, output, err := p.acceptor.accept(input)
	if err != nil {
		response.Error = dns.RcodeBadKey
		p.logger.Info("context negotiation refused", "reason", err.Error())
		return
	}
	// Bound context state independently of a long-lived service ticket.
	if maximum := now.Add(time.Hour); c.expires.After(maximum) {
		c.expires = maximum
	}
	inception, expiration := now.Unix(), c.expires.Unix()
	if inception < 0 || inception > math.MaxUint32 || expiration < 0 || expiration > math.MaxUint32 || len(output) > math.MaxUint16 {
		response.Error = dns.RcodeBadKey
		return
	}
	p.contexts[name] = c
	response.Inception = uint32(inception)
	response.Expiration = uint32(expiration)
	response.Key = hex.EncodeToString(output)
	response.KeySize = uint16(len(output)) //nolint:gosec // G115: output length checked against MaxUint16 above.
	m.SetTsig(t.Hdr.Name, gssAlgorithm, 300, now.Unix())
	p.logger.Info("context established", "principal", c.principal, "expires", c.expires)
}
