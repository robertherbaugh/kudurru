package dnsserver

import "github.com/miekg/dns"

// acceptDNSMessage lets bounded UPDATEs reach TSIG verification and the
// authoritative callback. miekg's default rejects UPDATE before either runs.
// All other opcodes retain the library's existing admission rules.
func acceptDNSMessage(h dns.Header) dns.MsgAcceptAction {
	if int(h.Bits>>11)&0xf != dns.OpcodeUpdate {
		return dns.DefaultMsgAcceptFunc(h)
	}
	if h.Bits&(1<<15) != 0 {
		return dns.MsgIgnore
	}
	// Match the update planner's finite default before decoding RDATA. Its
	// full parser subsequently validates classes, types, owner and wire size.
	if h.Qdcount != 1 || uint32(h.Ancount)+uint32(h.Nscount) > 4096 || h.Arcount > 2 {
		return dns.MsgReject
	}
	return dns.MsgAccept
}
