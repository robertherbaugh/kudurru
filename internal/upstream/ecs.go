package upstream

import (
	"net"
	"net/netip"

	"github.com/miekg/dns"
)

// EDNSBufferSize is the UDP payload size kudurru advertises upstream and to
// clients (DNS flag day 2020).
const EDNSBufferSize = 1232

// cgnat is RFC 6598 shared address space; like RFC 1918 it is never sent
// upstream as a client subnet.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// subnet returns the client subnet PrepareQuery sends upstream for m from
// client, or an invalid prefix when none is sent.
func (e ECSConfig) subnet(m *dns.Msg, client netip.Addr) netip.Prefix {
	if e.Mode != ECSForward && e.Mode != ECSAdd {
		return netip.Prefix{}
	}
	if s := clientECS(m); s != nil {
		return e.clamp(s)
	}
	if e.Mode != ECSAdd || !client.IsValid() {
		return netip.Prefix{}
	}
	client = client.Unmap()
	if !client.IsGlobalUnicast() || client.IsPrivate() || cgnat.Contains(client) {
		return netip.Prefix{}
	}
	bits := e.IPv6Prefix
	if client.Is4() {
		bits = e.IPv4Prefix
	}
	p, err := client.Prefix(bits)
	if err != nil {
		return netip.Prefix{}
	}
	return p
}

// clamp converts the client's option to a prefix no longer than the limits.
func (e ECSConfig) clamp(s *dns.EDNS0_SUBNET) netip.Prefix {
	addr, ok := netip.AddrFromSlice(s.Address)
	if !ok {
		return netip.Prefix{}
	}
	limit := e.IPv6Prefix
	if s.Family == 1 {
		addr = addr.Unmap()
		if !addr.Is4() {
			return netip.Prefix{}
		}
		limit = e.IPv4Prefix
	} else if addr.Is4() {
		addr = netip.AddrFrom16(addr.As16())
	}
	bits := min(int(s.SourceNetmask), limit)
	p, err := addr.Prefix(bits)
	if err != nil {
		return netip.Prefix{}
	}
	return p
}

// clientECS returns the client's ECS option, if any.
func clientECS(m *dns.Msg) *dns.EDNS0_SUBNET {
	opt := m.IsEdns0()
	if opt == nil {
		return nil
	}
	for _, o := range opt.Option {
		if s, ok := o.(*dns.EDNS0_SUBNET); ok {
			return s
		}
	}
	return nil
}

// ecsOption builds the option for p.
func ecsOption(p netip.Prefix, scope uint8) *dns.EDNS0_SUBNET {
	family := uint16(2)
	if p.Addr().Is4() {
		family = 1
	}
	return &dns.EDNS0_SUBNET{
		Code:          dns.EDNS0SUBNET,
		Family:        family,
		SourceNetmask: uint8(p.Bits()), //nolint:gosec // 0..128
		SourceScope:   scope,
		Address:       net.IP(p.Addr().AsSlice()),
	}
}

// FinishReply adapts resp, produced for an upstream or the cache, to the
// client query req: the ID, question and RD bit come from req, and the
// upstream's OPT record is replaced by one for the client. A client that
// sent EDNS gets an OPT with EDNSBufferSize, its own DO bit, any extended
// errors (RFC 8914) the upstream attached and, when it sent ECS, its own
// subnet echoed with scope ecsScope (negative: the upstream's scope, else
// 0). A client without EDNS gets no OPT. resp is modified in place.
func FinishReply(req, resp *dns.Msg, ecsScope int) {
	resp.Id = req.Id
	resp.RecursionDesired = req.RecursionDesired
	if len(req.Question) > 0 {
		resp.Question = []dns.Question{req.Question[0]}
	}
	var upstreamScope uint8
	var ede []dns.EDNS0
	extra := resp.Extra[:0]
	for _, rr := range resp.Extra {
		opt, ok := rr.(*dns.OPT)
		if !ok {
			extra = append(extra, rr)
			continue
		}
		for _, o := range opt.Option {
			switch o := o.(type) {
			case *dns.EDNS0_SUBNET:
				upstreamScope = o.SourceScope
			case *dns.EDNS0_EDE:
				ede = append(ede, o)
			}
		}
	}
	resp.Extra = extra

	ropt := req.IsEdns0()
	if ropt == nil {
		if resp.Rcode > 0xF {
			resp.Rcode = dns.RcodeServerFailure
		}
		return
	}
	opt := &dns.OPT{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT}}
	opt.SetUDPSize(EDNSBufferSize)
	opt.SetDo(ropt.Do())
	opt.Option = append(opt.Option, ede...)
	if s := clientECS(req); s != nil {
		scope := upstreamScope
		if ecsScope >= 0 {
			scope = uint8(min(ecsScope, 128)) //nolint:gosec // bounded
		}
		opt.Option = append(opt.Option, &dns.EDNS0_SUBNET{
			Code:          dns.EDNS0SUBNET,
			Family:        s.Family,
			SourceNetmask: s.SourceNetmask,
			SourceScope:   scope,
			Address:       s.Address,
		})
	}
	resp.Extra = append(resp.Extra, opt)
}

// AddEDE attaches an extended DNS error (RFC 8914) to resp's OPT record, if
// it has one.
func AddEDE(resp *dns.Msg, code uint16, text string) {
	if opt := resp.IsEdns0(); opt != nil {
		opt.Option = append(opt.Option, &dns.EDNS0_EDE{InfoCode: code, ExtraText: text})
	}
}
