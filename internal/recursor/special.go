package recursor

import (
	"net"
	"strconv"
	"strings"

	"github.com/miekg/dns"
)

// localZones are answered by the resolver itself and never sent to the
// internet: they either cannot exist in the global DNS or would only leak
// what the network looks like. Conditional forwarding and forward zones
// run before the resolver, so a router or a local server can still answer
// them.
//
//   - RFC 6303: private, loopback, link-local, documentation and other
//     locally served reverse zones;
//   - RFC 6761: test, invalid, localhost (answered with the loopback
//     addresses); RFC 6762: local (mDNS); RFC 7686: onion; RFC 8375:
//     home.arpa; RFC 9476: alt; and internal, which ICANN reserved for
//     private use in 2024.
var localZones = func() map[string]bool {
	z := map[string]bool{
		"test.": true, "invalid.": true, "local.": true, "onion.": true, "alt.": true,
		"internal.": true, "home.arpa.": true, "localhost.": true,
		"0.in-addr.arpa.": true, "10.in-addr.arpa.": true, "127.in-addr.arpa.": true,
		"254.169.in-addr.arpa.": true, "168.192.in-addr.arpa.": true,
		"2.0.192.in-addr.arpa.": true, "100.51.198.in-addr.arpa.": true,
		"113.0.203.in-addr.arpa.": true, "255.255.255.255.in-addr.arpa.": true,
		// ::/128 and ::1/128
		"0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.ip6.arpa.": true,
		"1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.ip6.arpa.": true,
		"d.f.ip6.arpa.":   true,                                                                            // fd00::/8 (ULA)
		"c.f.ip6.arpa.":   true,                                                                            // fc00::/8 (ULA, unassigned)
		"8.e.f.ip6.arpa.": true, "9.e.f.ip6.arpa.": true, "a.e.f.ip6.arpa.": true, "b.e.f.ip6.arpa.": true, // fe80::/10
		"8.b.d.0.1.0.0.2.ip6.arpa.": true, // 2001:db8::/32
	}
	for i := 16; i <= 31; i++ { // 172.16.0.0/12
		z[strconv.Itoa(i)+".172.in-addr.arpa."] = true
	}
	for i := 64; i <= 127; i++ { // 100.64.0.0/10 (RFC 6598)
		z[strconv.Itoa(i)+".100.in-addr.arpa."] = true
	}
	return z
}()

// localZone returns the locally served zone that contains name, if any.
func localZone(name string) (string, bool) {
	n := canon(name)
	for {
		if localZones[n] {
			return n, true
		}
		if n == "." {
			return "", false
		}
		n = parentName(n)
	}
}

// localAnswer answers a name in a locally served zone: the loopback
// addresses for localhost, else NXDOMAIN (NODATA at the apex) with the
// RFC 6303 SOA.
func localAnswer(zone string, q dns.Question) *answer {
	a := &answer{sec: SecInsecure}
	name := canon(q.Name)
	if zone == "localhost." && name == "localhost." {
		switch q.Qtype {
		case dns.TypeA:
			a.records = []dns.RR{&dns.A{Hdr: hdr(q.Name, dns.TypeA, 3600), A: []byte{127, 0, 0, 1}}}
			return a
		case dns.TypeAAAA:
			a.records = []dns.RR{&dns.AAAA{Hdr: hdr(q.Name, dns.TypeAAAA, 3600), AAAA: net.IPv6loopback}}
			return a
		}
	}
	soa := &dns.SOA{
		Hdr: hdr(zone, dns.TypeSOA, 10800), Ns: zone, Mbox: "nobody.invalid.",
		Serial: 1, Refresh: 3600, Retry: 1200, Expire: 604800, Minttl: 10800,
	}
	if q.Qtype == dns.TypeSOA && strings.EqualFold(name, zone) {
		a.records = []dns.RR{soa}
		return a
	}
	a.authority = []dns.RR{soa}
	if !strings.EqualFold(name, zone) {
		a.rcode = dns.RcodeNameError
	}
	return a
}

func hdr(name string, t uint16, ttl uint32) dns.RR_Header {
	return dns.RR_Header{Name: name, Rrtype: t, Class: dns.ClassINET, Ttl: ttl}
}
