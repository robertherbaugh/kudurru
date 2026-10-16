package recursor

import (
	"context"
	"crypto"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// An in-memory DNS: authoritative test zones, optionally signed with NSEC
// or NSEC3 (with opt-out), served by fake servers that a fakeNet routes
// to by address.

type tzone struct {
	name   string
	rrs    []dns.RR
	signed bool
	nsec3  bool
	optOut bool
	iter   uint16

	ksk, zsk       *dns.DNSKEY
	kskKey, zskKey crypto.Signer

	// built by finalize
	sets  map[string]map[uint16][]dns.RR // authoritative and delegation data
	sigs  map[string]map[uint16][]dns.RR // RRSIGs by owner and covered type
	glue  map[string][]dns.RR            // A/AAAA below cuts
	cuts  map[string]bool
	names map[string]bool // existing names, empty non-terminals included
	nsecs []*dns.NSEC
	n3    []*dns.NSEC3

	// knobs
	corrupt   map[string]bool // "owner|TYPE": signature made invalid
	expired   bool            // every signature expired yesterday
	noDenial  bool            // omit NSEC/NSEC3 from negative answers
	stripSigs map[string]bool // "owner|TYPE": no RRSIG served
}

func newZone(name string, signed bool) *tzone {
	z := &tzone{name: canon(name), signed: signed, corrupt: map[string]bool{}, stripSigs: map[string]bool{}}
	z.add(fmt.Sprintf("%s 3600 IN SOA %s %s 1 7200 3600 1209600 300", z.name, dns.Fqdn("ns1."+strings.TrimSuffix(z.name, ".")), dns.Fqdn("hostmaster."+strings.TrimSuffix(z.name, "."))))
	return z
}

func (z *tzone) add(lines ...string) *tzone {
	for _, l := range lines {
		rr, err := dns.NewRR(l)
		if err != nil {
			panic(fmt.Sprintf("%q: %v", l, err))
		}
		rr.Header().Name = canon(rr.Header().Name)
		z.rrs = append(z.rrs, rr)
	}
	return z
}

// serveNS adds the apex NS and glue for the zone's own servers.
func (z *tzone) serveNS(nsName, ip string) *tzone {
	z.add(fmt.Sprintf("%s 3600 IN NS %s", z.name, nsName))
	if isSubOrEqual(z.name, nsName) && ip != "" {
		z.add(fmt.Sprintf("%s 3600 IN A %s", nsName, ip))
	}
	return z
}

// delegate adds a delegation to child with its name server (and glue when
// the name is inside z) and, for a signed child, its DS.
func (z *tzone) delegate(child *tzone, nsName, ip string) *tzone {
	z.add(fmt.Sprintf("%s 86400 IN NS %s", child.name, nsName))
	if isSubOrEqual(z.name, nsName) && ip != "" {
		z.add(fmt.Sprintf("%s 86400 IN A %s", nsName, ip))
	}
	if child.signed && child.ksk != nil {
		ds := child.ksk.ToDS(dns.SHA256)
		ds.Hdr.Ttl = 86400
		z.rrs = append(z.rrs, ds)
	}
	return z
}

func (z *tzone) genKeys(t testing.TB) {
	if !z.signed || z.ksk != nil {
		return
	}
	mk := func(flags uint16) (*dns.DNSKEY, crypto.Signer) {
		k := &dns.DNSKEY{Hdr: dns.RR_Header{Name: z.name, Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 3600}, Flags: flags, Protocol: 3, Algorithm: dns.ECDSAP256SHA256}
		priv, err := k.Generate(256)
		if err != nil {
			t.Fatal(err)
		}
		return k, priv.(crypto.Signer)
	}
	z.ksk, z.kskKey = mk(257)
	z.zsk, z.zskKey = mk(256)
}

func setKey(name string, t uint16) string { return name + "|" + dns.TypeToString[t] }

func (z *tzone) finalize(t testing.TB) {
	z.sets = map[string]map[uint16][]dns.RR{}
	z.sigs = map[string]map[uint16][]dns.RR{}
	z.glue = map[string][]dns.RR{}
	z.cuts = map[string]bool{}
	z.names = map[string]bool{z.name: true}
	z.nsecs, z.n3 = nil, nil
	for _, rr := range z.rrs {
		if h := rr.Header(); h.Rrtype == dns.TypeNS && h.Name != z.name {
			z.cuts[h.Name] = true
		}
	}
	below := func(n string) bool {
		for c := range z.cuts {
			if isBelow(c, n) {
				return true
			}
		}
		return false
	}
	put := func(rr dns.RR) {
		h := rr.Header()
		if z.sets[h.Name] == nil {
			z.sets[h.Name] = map[uint16][]dns.RR{}
		}
		z.sets[h.Name][h.Rrtype] = append(z.sets[h.Name][h.Rrtype], rr)
	}
	for _, rr := range z.rrs {
		h := rr.Header()
		if below(h.Name) {
			z.glue[h.Name] = append(z.glue[h.Name], rr)
			continue
		}
		put(rr)
		for n := h.Name; n != z.name && isBelow(z.name, n); n = parentName(n) {
			z.names[n] = true
		}
	}
	if z.signed {
		put(z.ksk)
		put(z.zsk)
	}
	if !z.signed {
		return
	}
	// The denial chain.
	var owners []string
	for n := range z.sets {
		owners = append(owners, n)
	}
	sort.Slice(owners, func(i, j int) bool { return canonicalCompare(owners[i], owners[j]) < 0 })
	types := func(n string) []uint16 {
		var ts []uint16
		for tp := range z.sets[n] {
			if z.cuts[n] && tp != dns.TypeNS && tp != dns.TypeDS {
				continue
			}
			ts = append(ts, tp)
		}
		if !z.cuts[n] || z.sets[n][dns.TypeDS] != nil {
			ts = append(ts, dns.TypeRRSIG)
		}
		return ts
	}
	if !z.nsec3 {
		for i, n := range owners {
			next := owners[(i+1)%len(owners)]
			bm := append(types(n), dns.TypeNSEC)
			slices.Sort(bm)
			nsec := &dns.NSEC{Hdr: dns.RR_Header{Name: n, Rrtype: dns.TypeNSEC, Class: dns.ClassINET, Ttl: 300}, NextDomain: next, TypeBitMap: bm}
			z.nsecs = append(z.nsecs, nsec)
			put(nsec)
		}
	} else {
		type h3 struct {
			hash, name string
		}
		var hs []h3
		for n := range z.names {
			if z.optOut && z.cuts[n] && z.sets[n][dns.TypeDS] == nil {
				continue
			}
			hs = append(hs, h3{dns.HashName(n, dns.SHA1, z.iter, ""), n})
		}
		sort.Slice(hs, func(i, j int) bool { return hs[i].hash < hs[j].hash })
		for i, x := range hs {
			var bm []uint16
			if z.sets[x.name] != nil {
				bm = types(x.name)
			}
			slices.Sort(bm)
			r := &dns.NSEC3{Hdr: dns.RR_Header{Name: strings.ToLower(x.hash) + "." + z.name, Rrtype: dns.TypeNSEC3, Class: dns.ClassINET, Ttl: 300},
				Hash: dns.SHA1, Iterations: z.iter, SaltLength: 0, Salt: "", HashLength: 20, NextDomain: hs[(i+1)%len(hs)].hash, TypeBitMap: bm}
			if z.optOut {
				r.Flags = 1
			}
			z.n3 = append(z.n3, r)
			put(r)
		}
	}
	// Signatures.
	now := time.Now()
	inc, exp := now.Add(-time.Hour), now.Add(48*time.Hour)
	if z.expired {
		inc, exp = now.Add(-72*time.Hour), now.Add(-24*time.Hour)
	}
	for n, byType := range z.sets {
		for tp, set := range byType {
			if z.cuts[n] && tp != dns.TypeDS && tp != dns.TypeNSEC {
				continue
			}
			key, priv := z.zsk, z.zskKey
			if tp == dns.TypeDNSKEY {
				key, priv = z.ksk, z.kskKey
			}
			sig := &dns.RRSIG{Hdr: dns.RR_Header{Ttl: set[0].Header().Ttl}, KeyTag: key.KeyTag(), SignerName: z.name, Algorithm: key.Algorithm,
				Inception: uint32(inc.Unix()), Expiration: uint32(exp.Unix())} //nolint:gosec // test times
			if err := sig.Sign(priv, set); err != nil {
				t.Fatalf("sign %s %d: %v", n, tp, err)
			}
			if z.corrupt[setKey(n, tp)] {
				b := []byte(sig.Signature)
				b[5] ^= 0x01
				if b[5] == '+' || b[5] == '/' {
					b[5] = 'A'
				}
				sig.Signature = string(b)
			}
			if z.sigs[n] == nil {
				z.sigs[n] = map[uint16][]dns.RR{}
			}
			z.sigs[n][tp] = []dns.RR{sig}
		}
	}
}

func (z *tzone) withSigs(out []dns.RR, n string, tp uint16, do bool) []dns.RR {
	out = append(out, z.sets[n][tp]...)
	if do && z.signed && !z.stripSigs[setKey(n, tp)] {
		out = append(out, z.sigs[n][tp]...)
	}
	return out
}

// cutFor returns the delegation at or above name inside z.
func (z *tzone) cutFor(name string) string {
	best := ""
	for c := range z.cuts {
		if isSubOrEqual(c, name) && (best == "" || labels(c) < labels(best)) {
			best = c
		}
	}
	return best
}

func (z *tzone) closestEncloser(name string) string {
	n := name
	for !z.names[n] {
		n = parentName(n)
	}
	return n
}

// denial adds the records that prove what (NXDOMAIN, NODATA or a wildcard
// expansion) for name.
func (z *tzone) denial(out []dns.RR, name string, nodata bool, do bool) []dns.RR {
	if !z.signed || !do || z.noDenial {
		return out
	}
	add := func(rr dns.RR) {
		for _, x := range out {
			if x == rr {
				return
			}
		}
		h := rr.Header()
		out = append(out, rr)
		out = append(out, z.sigs[h.Name][h.Rrtype]...)
	}
	if !z.nsec3 {
		for _, n := range z.nsecs {
			if nodata && equalName(n.Hdr.Name, name) {
				add(n)
				return out
			}
		}
		for _, n := range z.nsecs {
			if nsecCovers(n, name) {
				add(n)
			}
		}
		if !nodata {
			wc := wildcardOf(z.closestEncloser(name))
			for _, n := range z.nsecs {
				if nsecCovers(n, wc) || equalName(n.Hdr.Name, wc) {
					add(n)
				}
			}
		}
		return out
	}
	budget := 1000
	s := &nsec3Set{recs: z.n3, alg: dns.SHA1, iter: z.iter, hashes: map[string]string{}, budget: &budget}
	if nodata {
		if m := s.match(name); m != nil {
			add(m)
			return out
		}
	}
	ce, _, _ := s.closestEncloser(name)
	if m := s.match(ce); m != nil {
		add(m)
	}
	if ce != name {
		if c := s.cover(childToward(ce, name)); c != nil {
			add(c)
		}
	}
	if !nodata {
		if c := s.cover(wildcardOf(ce)); c != nil {
			add(c)
		}
	}
	return out
}

// answer is what an authoritative server for z says to name/qtype.
func (z *tzone) answer(name string, qtype uint16, do bool) *dns.Msg {
	m := new(dns.Msg)
	m.Authoritative = true
	if cut := z.cutFor(name); cut != "" && (qtype != dns.TypeDS || cut != name) {
		m.Authoritative = false
		m.Ns = append(m.Ns, z.sets[cut][dns.TypeNS]...)
		if z.sets[cut][dns.TypeDS] != nil {
			m.Ns = z.withSigs(m.Ns, cut, dns.TypeDS, do)
		} else {
			m.Ns = z.denial(m.Ns, cut, true, do)
		}
		for _, ns := range z.sets[cut][dns.TypeNS] {
			target := canon(ns.(*dns.NS).Ns)
			m.Extra = append(m.Extra, z.glue[target]...)
			m.Extra = append(m.Extra, z.sets[target][dns.TypeA]...)
		}
		return m
	}
	for n := name; isBelow(z.name, n); {
		n = parentName(n)
		if d := z.sets[n][dns.TypeDNAME]; d != nil && isBelow(n, name) {
			m.Answer = z.withSigs(m.Answer, n, dns.TypeDNAME, do)
			target, _ := substitute(name, d[0].(*dns.DNAME))
			m.Answer = append(m.Answer, &dns.CNAME{Hdr: hdr(name, dns.TypeCNAME, d[0].Header().Ttl), Target: target})
			return m
		}
	}
	if z.names[name] {
		if z.sets[name][qtype] != nil {
			m.Answer = z.withSigs(m.Answer, name, qtype, do)
			return m
		}
		if c := z.sets[name][dns.TypeCNAME]; c != nil && qtype != dns.TypeCNAME {
			m.Answer = z.withSigs(m.Answer, name, dns.TypeCNAME, do)
			target := canon(c[0].(*dns.CNAME).Target)
			if isSubOrEqual(z.name, target) && z.cutFor(target) == "" && z.sets[target][qtype] != nil {
				m.Answer = z.withSigs(m.Answer, target, qtype, do)
			}
			return m
		}
		m.Ns = z.withSigs(m.Ns, z.name, dns.TypeSOA, do)
		m.Ns = z.denial(m.Ns, name, true, do)
		return m
	}
	ce := z.closestEncloser(name)
	wc := wildcardOf(ce)
	if z.sets[wc] != nil {
		if set := z.sets[wc][qtype]; set != nil {
			for _, rr := range set {
				c := dns.Copy(rr)
				c.Header().Name = name
				m.Answer = append(m.Answer, c)
			}
			if do && z.signed {
				for _, s := range z.sigs[wc][qtype] {
					c := dns.Copy(s)
					c.Header().Name = name
					m.Answer = append(m.Answer, c)
				}
				m.Ns = z.denial(m.Ns, name, true, do)
				if !z.nsec3 {
					m.Ns = nil
					for _, n := range z.nsecs {
						if nsecCovers(n, name) && !z.noDenial {
							m.Ns = append(m.Ns, n)
							m.Ns = append(m.Ns, z.sigs[n.Hdr.Name][dns.TypeNSEC]...)
						}
					}
				}
			}
			return m
		}
	}
	m.Rcode = dns.RcodeNameError
	m.Ns = z.withSigs(m.Ns, z.name, dns.TypeSOA, do)
	m.Ns = z.denial(m.Ns, name, false, do)
	return m
}

// fakeServer is one authoritative server.
type fakeServer struct {
	zones    []*tzone
	refuse   bool
	silent   bool // never answers
	truncate bool // UDP answers are truncated
	noCase   bool // lower-cases the question (no 0x20 echo)
	tamper   func(q, r *dns.Msg)
}

type timeoutErr struct{}

func (timeoutErr) Error() string { return "i/o timeout" }
func (timeoutErr) Timeout() bool { return true }

// fakeNet routes queries to fake servers by address and logs them.
type fakeNet struct {
	mu      sync.Mutex
	servers map[netip.Addr]*fakeServer
	log     []string
	tcp     int
}

func newFakeNet() *fakeNet { return &fakeNet{servers: map[netip.Addr]*fakeServer{}} }

func (n *fakeNet) host(ip string, zones ...*tzone) *fakeServer {
	s := &fakeServer{zones: zones}
	n.servers[netip.MustParseAddr(ip)] = s
	return s
}

func (n *fakeNet) queries() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.log...)
}

func (n *fakeNet) reset() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.log, n.tcp = nil, 0
}

func (n *fakeNet) Exchange(ctx context.Context, m *dns.Msg, server netip.AddrPort, tcp bool) (*dns.Msg, error) {
	q := m.Question[0]
	n.mu.Lock()
	n.log = append(n.log, fmt.Sprintf("%s %s %s", server.Addr(), canon(q.Name), dns.TypeToString[q.Qtype]))
	if tcp {
		n.tcp++
	}
	s := n.servers[server.Addr()]
	n.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil {
		return nil, timeoutErr{}
	}
	if s.silent {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	r := new(dns.Msg)
	var zone *tzone
	for _, z := range s.zones {
		// DS is the parent's: a server hosting both sides answers from
		// the parent, as real servers do.
		if q.Qtype == dns.TypeDS && equalName(z.name, q.Name) && q.Name != "." && len(s.zones) > 1 {
			continue
		}
		if isSubOrEqual(z.name, q.Name) && (zone == nil || labels(z.name) > labels(zone.name)) {
			zone = z
		}
	}
	switch {
	case s.refuse || zone == nil:
		r.Rcode = dns.RcodeRefused
	default:
		do := false
		if o := m.IsEdns0(); o != nil {
			do = o.Do()
		}
		r = zone.answer(canon(q.Name), q.Qtype, do).Copy()
	}
	r.Id, r.Response, r.Opcode = m.Id, true, m.Opcode
	r.Question = []dns.Question{q}
	if s.noCase {
		r.Question[0].Name = strings.ToLower(q.Name)
	}
	if s.truncate && !tcp {
		r.Answer, r.Ns, r.Extra = nil, nil, nil
		r.Truncated = true
	}
	if s.tamper != nil {
		s.tamper(m, r)
	}
	return r, nil
}

// world is a small signed internet:
//
//	.                 10.0.0.1  signed (NSEC)
//	com.              10.0.1.1  signed (NSEC3, opt-out)
//	example.com.      10.0.2.1  signed (NSEC), wildcard, CNAMEs, DNAME
//	insecure.com.     10.0.2.9  unsigned, opt-out delegation from com
//	org.              10.0.3.1  signed (NSEC)
//	unsigned.org.     10.0.4.1  unsigned, NSEC-proven insecure delegation
//	nsec3.org.        10.0.5.1  signed (NSEC3, no opt-out)
//	net.              10.0.6.1  signed; hosts name servers for others
//	glueless.org.     10.0.7.1  signed, served by ns.hosting.net (no glue)
type world struct {
	t                                       testing.TB
	net                                     *fakeNet
	root, com, example, insecure            *tzone
	org, unsigned, nsec3, netz, glueless    *tzone
	hosting                                 *tzone
	rootSrv, exampleSrv, comSrv, gluelessNS *fakeServer
}

func newWorld(t testing.TB) *world {
	w := &world{t: t, net: newFakeNet()}
	w.root = newZone(".", true)
	w.com = newZone("com.", true)
	w.com.nsec3, w.com.optOut = true, true
	w.example = newZone("example.com.", true)
	w.insecure = newZone("insecure.com.", false)
	w.org = newZone("org.", true)
	w.unsigned = newZone("unsigned.org.", false)
	w.nsec3 = newZone("nsec3.org.", true)
	w.nsec3.nsec3 = true
	w.netz = newZone("net.", true)
	w.hosting = newZone("hosting.net.", true)
	w.glueless = newZone("glueless.org.", true)

	w.example.serveNS("ns1.example.com.", "10.0.2.1").add(
		"example.com. 300 IN A 192.0.2.1",
		"www.example.com. 300 IN A 192.0.2.10",
		"www.example.com. 300 IN AAAA 2001:db8::10",
		"alias.example.com. 300 IN CNAME www.example.com.",
		"far.example.com. 300 IN CNAME www.unsigned.org.",
		"loop1.example.com. 300 IN CNAME loop2.example.com.",
		"loop2.example.com. 300 IN CNAME loop1.example.com.",
		"*.wild.example.com. 300 IN A 192.0.2.99",
		"a.b.deep.example.com. 300 IN A 192.0.2.20",
		"old.example.com. 300 IN DNAME nsec3.org.",
		"txt.example.com. 300 IN TXT \"hello\"",
	)
	w.insecure.serveNS("ns.insecure.com.", "10.0.2.9").add("www.insecure.com. 300 IN A 192.0.2.30")
	w.unsigned.serveNS("ns.unsigned.org.", "10.0.4.1").add("www.unsigned.org. 300 IN A 192.0.2.40")
	w.nsec3.serveNS("ns.nsec3.org.", "10.0.5.1").add("www.nsec3.org. 300 IN A 192.0.2.50", "*.wild.nsec3.org. 300 IN A 192.0.2.51")
	w.hosting.serveNS("ns.hosting.net.", "10.0.7.1")
	w.glueless.serveNS("ns.hosting.net.", "").add("www.glueless.org. 300 IN A 192.0.2.60")
	w.org.serveNS("a0.org.", "10.0.3.1")
	w.netz.serveNS("a0.net.", "10.0.6.1")
	w.com.serveNS("a.gtld.com.", "10.0.1.1")
	w.root.serveNS("a.root.test.", "")

	for _, z := range []*tzone{w.root, w.com, w.example, w.org, w.nsec3, w.netz, w.hosting, w.glueless} {
		z.genKeys(t)
	}
	w.com.delegate(w.example, "ns1.example.com.", "10.0.2.1").delegate(w.insecure, "ns.insecure.com.", "10.0.2.9")
	w.org.delegate(w.unsigned, "ns.unsigned.org.", "10.0.4.1").delegate(w.nsec3, "ns.nsec3.org.", "10.0.5.1").
		delegate(w.glueless, "ns.hosting.net.", "")
	w.netz.delegate(w.hosting, "ns.hosting.net.", "10.0.7.1")
	w.root.delegate(w.com, "a.gtld.com.", "10.0.1.1").delegate(w.org, "a0.org.", "10.0.3.1").delegate(w.netz, "a0.net.", "10.0.6.1")
	w.root.add("a.root.test. 3600 IN A 10.0.0.1")
	w.finalize()

	w.rootSrv = w.net.host("10.0.0.1", w.root)
	w.comSrv = w.net.host("10.0.1.1", w.com)
	w.exampleSrv = w.net.host("10.0.2.1", w.example)
	w.net.host("10.0.2.9", w.insecure)
	w.net.host("10.0.3.1", w.org)
	w.net.host("10.0.4.1", w.unsigned)
	w.net.host("10.0.5.1", w.nsec3)
	w.net.host("10.0.6.1", w.netz)
	w.gluelessNS = w.net.host("10.0.7.1", w.hosting, w.glueless)
	return w
}

// finalize (re)builds every zone after a change to its data or knobs.
func (w *world) finalize() {
	for _, z := range []*tzone{w.root, w.com, w.example, w.insecure, w.org, w.unsigned, w.nsec3, w.netz, w.hosting, w.glueless} {
		z.finalize(w.t)
	}
}

func (w *world) anchors() []string {
	return []string{w.root.ksk.ToDS(dns.SHA256).String()}
}

func (w *world) resolver(mod ...func(*Config)) *Resolver {
	w.t.Helper()
	cfg := DefaultConfig()
	cfg.IPv6 = IPv6Off
	cfg.RootHints = []string{"10.0.0.1"}
	cfg.TrustAnchors = w.anchors()
	for _, f := range mod {
		f(&cfg)
	}
	r, err := New(cfg, Options{Exchanger: w.net})
	if err != nil {
		w.t.Fatal(err)
	}
	return r
}
