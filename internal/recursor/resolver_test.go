package recursor

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func clientQuery(name string, qtype uint16, mod ...func(*dns.Msg)) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	m.SetEdns0(1232, true)
	for _, f := range mod {
		f(m)
	}
	return m
}

func noDO(m *dns.Msg) { m.IsEdns0().SetDo(false) }
func cd(m *dns.Msg)   { m.CheckingDisabled = true }

func ask(t *testing.T, r *Resolver, name string, qtype uint16, mod ...func(*dns.Msg)) (*dns.Msg, Security) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, sec, err := r.ExchangeValidated(ctx, clientQuery(name, qtype, mod...))
	if err != nil {
		t.Fatalf("%s %s: %v", name, dns.TypeToString[qtype], err)
	}
	return resp, sec
}

func ede(m *dns.Msg) (uint16, bool) {
	if o := m.IsEdns0(); o != nil {
		for _, x := range o.Option {
			if e, ok := x.(*dns.EDNS0_EDE); ok {
				return e.InfoCode, true
			}
		}
	}
	return 0, false
}

func want(t *testing.T, m *dns.Msg, sec Security, rcode int, wantSec Security, answers ...string) {
	t.Helper()
	if m.Rcode != rcode || sec != wantSec {
		code, _ := ede(m)
		t.Fatalf("rcode %s sec %s (EDE %d), want %s %s\n%v", dns.RcodeToString[m.Rcode], sec, code, dns.RcodeToString[rcode], wantSec, m)
	}
	var got []string
	for _, rr := range m.Answer {
		switch v := rr.(type) {
		case *dns.A:
			got = append(got, v.A.String())
		case *dns.AAAA:
			got = append(got, v.AAAA.String())
		case *dns.CNAME:
			got = append(got, "CNAME "+v.Target)
		}
	}
	if len(answers) > 0 && !slices.Equal(got, answers) {
		t.Fatalf("answers %v, want %v", got, answers)
	}
	if m.AuthenticatedData != (wantSec == SecSecure) {
		t.Fatalf("AD %v for %s", m.AuthenticatedData, wantSec)
	}
	if !m.RecursionAvailable {
		t.Fatal("RA not set")
	}
}

func TestSecureAnswer(t *testing.T) {
	w := newWorld(t)
	r := w.resolver()
	m, sec := ask(t, r, "www.example.com", dns.TypeA)
	want(t, m, sec, dns.RcodeSuccess, SecSecure, "192.0.2.10")
	if len(rrsetOfType(m.Answer, dns.TypeRRSIG)) == 0 {
		t.Fatal("a DO client gets the RRSIGs")
	}
	// Without DO: no RRSIGs and no AD, still validated.
	m, sec = ask(t, r, "www.example.com", dns.TypeAAAA, noDO)
	if sec != SecSecure || m.AuthenticatedData || len(rrsetOfType(m.Answer, dns.TypeRRSIG)) > 0 {
		t.Fatalf("no DO: sec %s AD %v %v", sec, m.AuthenticatedData, m.Answer)
	}
	// AD bit without DO asks for AD (RFC 6840 §5.7).
	m, _ = ask(t, r, "www.example.com", dns.TypeA, noDO, func(m *dns.Msg) { m.AuthenticatedData = true })
	if !m.AuthenticatedData {
		t.Fatal("AD requested but not set")
	}
}

func TestQNAMEMinimization(t *testing.T) {
	w := newWorld(t)
	r := w.resolver(func(c *Config) { c.DNSSEC = false })
	m, sec := ask(t, r, "a.b.deep.example.com", dns.TypeA)
	want(t, m, sec, dns.RcodeSuccess, SecUnknown, "192.0.2.20")
	for _, q := range w.net.queries() {
		f := strings.Fields(q)
		switch f[0] {
		case "10.0.0.1":
			if f[1] != "com." && f[1] != "." {
				t.Errorf("the root saw %q", q)
			}
		case "10.0.1.1":
			if f[1] != "example.com." {
				t.Errorf("com saw %q", q)
			}
		}
	}
	// Off: the root sees the full name.
	w.net.reset()
	r = w.resolver(func(c *Config) { c.DNSSEC, c.QNAMEMinimization = false, false })
	ask(t, r, "a.b.deep.example.com", dns.TypeA)
	if !slices.Contains(w.net.queries(), "10.0.0.1 a.b.deep.example.com. A") {
		t.Fatalf("without minimization the full name goes to the root: %v", w.net.queries())
	}
}

func TestValidationOffSendsNoDO(t *testing.T) {
	w := newWorld(t)
	sawDO := false
	w.rootSrv.tamper = func(q, _ *dns.Msg) { sawDO = sawDO || q.IsEdns0().Do() }
	r := w.resolver(func(c *Config) { c.DNSSEC = false })
	m, sec := ask(t, r, "www.example.com", dns.TypeA)
	want(t, m, sec, dns.RcodeSuccess, SecUnknown, "192.0.2.10")
	if sawDO {
		t.Fatal("DO set with validation off")
	}
}

func TestInsecureDelegations(t *testing.T) {
	w := newWorld(t)
	r := w.resolver()
	// NSEC proves unsigned.org has no DS.
	m, sec := ask(t, r, "www.unsigned.org", dns.TypeA)
	want(t, m, sec, dns.RcodeSuccess, SecInsecure, "192.0.2.40")
	// NSEC3 opt-out covers insecure.com.
	m, sec = ask(t, r, "www.insecure.com", dns.TypeA)
	want(t, m, sec, dns.RcodeSuccess, SecInsecure, "192.0.2.30")
	// A negative answer from an unsigned zone is insecure too.
	m, sec = ask(t, r, "nope.unsigned.org", dns.TypeA)
	want(t, m, sec, dns.RcodeNameError, SecInsecure)
}

func TestCNAMEAcrossZones(t *testing.T) {
	w := newWorld(t)
	r := w.resolver()
	m, sec := ask(t, r, "alias.example.com", dns.TypeA)
	want(t, m, sec, dns.RcodeSuccess, SecSecure, "CNAME www.example.com.", "192.0.2.10")
	// Secure CNAME to an insecure zone: insecure.
	m, sec = ask(t, r, "far.example.com", dns.TypeA)
	want(t, m, sec, dns.RcodeSuccess, SecInsecure, "CNAME www.unsigned.org.", "192.0.2.40")
	// Loops end in SERVFAIL.
	m, _ = ask(t, r, "loop1.example.com", dns.TypeA)
	if m.Rcode != dns.RcodeServerFailure {
		t.Fatalf("CNAME loop: %s", dns.RcodeToString[m.Rcode])
	}
}

func TestDNAME(t *testing.T) {
	w := newWorld(t)
	r := w.resolver()
	m, sec := ask(t, r, "www.old.example.com", dns.TypeA)
	want(t, m, sec, dns.RcodeSuccess, SecSecure, "CNAME www.nsec3.org.", "192.0.2.50")
}

func TestNegativeAnswers(t *testing.T) {
	w := newWorld(t)
	r := w.resolver()
	for _, tc := range []struct {
		name  string
		qtype uint16
		rcode int
	}{
		{"nope.example.com", dns.TypeA, dns.RcodeNameError},     // NSEC NXDOMAIN
		{"www.example.com", dns.TypeMX, dns.RcodeSuccess},       // NSEC NODATA
		{"deep.example.com", dns.TypeA, dns.RcodeSuccess},       // empty non-terminal
		{"nope.nsec3.org", dns.TypeA, dns.RcodeNameError},       // NSEC3 NXDOMAIN
		{"www.nsec3.org", dns.TypeTXT, dns.RcodeSuccess},        // NSEC3 NODATA
		{"nope.org", dns.TypeA, dns.RcodeNameError},             // NSEC at the TLD
		{"nosuchtld", dns.TypeA, dns.RcodeNameError},            // at the root
		{"x.y.nope.example.com", dns.TypeA, dns.RcodeNameError}, // minimized NXDOMAIN
	} {
		m, sec := ask(t, r, tc.name, tc.qtype)
		want(t, m, sec, tc.rcode, SecSecure)
		if len(rrsetOfType(m.Ns, dns.TypeSOA)) != 1 {
			t.Fatalf("%s: negative answer without SOA: %v", tc.name, m)
		}
	}
	// NSEC3 NXDOMAIN under opt-out is insecure.
	m, sec := ask(t, r, "nope.com", dns.TypeA)
	want(t, m, sec, dns.RcodeNameError, SecInsecure)
}

func TestWildcards(t *testing.T) {
	w := newWorld(t)
	r := w.resolver()
	m, sec := ask(t, r, "anything.wild.example.com", dns.TypeA)
	want(t, m, sec, dns.RcodeSuccess, SecSecure, "192.0.2.99")
	m, sec = ask(t, r, "x.wild.nsec3.org", dns.TypeA)
	want(t, m, sec, dns.RcodeSuccess, SecSecure, "192.0.2.51")

	// A wildcard expansion without the proof that the name does not exist
	// could replace real data: bogus.
	w.example.noDenial = true
	w.nsec3.noDenial = true
	r = w.resolver()
	for _, n := range []string{"other.wild.example.com", "y.wild.nsec3.org"} {
		m, sec = ask(t, r, n, dns.TypeA)
		if m.Rcode != dns.RcodeServerFailure || sec != SecBogus {
			t.Fatalf("%s without proof: %s %s", n, dns.RcodeToString[m.Rcode], sec)
		}
		if code, _ := ede(m); code != dns.ExtendedErrorCodeNSECMissing {
			t.Fatalf("%s: EDE %d", n, code)
		}
	}
}

func TestBogus(t *testing.T) {
	cases := []struct {
		name  string
		setup func(w *world)
		qname string
		ede   uint16
	}{
		{"bad signature", func(w *world) { w.example.corrupt[setKey("www.example.com.", dns.TypeA)] = true }, "www.example.com", dns.ExtendedErrorCodeDNSBogus},
		{"bad DNSKEY signature", func(w *world) { w.example.corrupt[setKey("example.com.", dns.TypeDNSKEY)] = true }, "www.example.com", dns.ExtendedErrorCodeDNSBogus},
		{"expired", func(w *world) { w.example.expired = true }, "www.example.com", dns.ExtendedErrorCodeSignatureExpired},
		{"stripped signatures", func(w *world) { w.example.stripSigs[setKey("www.example.com.", dns.TypeA)] = true }, "www.example.com", dns.ExtendedErrorCodeRRSIGsMissing},
		{"missing denial", func(w *world) { w.example.noDenial = true }, "nope.example.com", dns.ExtendedErrorCodeNSECMissing},
		{"wrong DS", func(w *world) {
			other := newZone("example.com.", true)
			other.genKeys(w.t)
			w.example.ksk, w.example.kskKey = other.ksk, other.kskKey
		}, "www.example.com", dns.ExtendedErrorCodeDNSBogus},
		{"forged referral DS", func(w *world) { w.com.corrupt[setKey("example.com.", dns.TypeDS)] = true }, "www.example.com", dns.ExtendedErrorCodeDNSBogus},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			tc.setup(w)
			w.finalize()
			r := w.resolver()
			m, sec := ask(t, r, tc.qname, dns.TypeA)
			if m.Rcode != dns.RcodeServerFailure || sec != SecBogus || len(m.Answer) > 0 {
				t.Fatalf("got %s %s %v", dns.RcodeToString[m.Rcode], sec, m.Answer)
			}
			if code, ok := ede(m); !ok || code != tc.ede {
				t.Fatalf("EDE %d, want %d", code, tc.ede)
			}
			// CD: the client validates itself and gets the data.
			m, sec = ask(t, r, tc.qname, dns.TypeA, cd)
			if m.Rcode == dns.RcodeServerFailure || m.AuthenticatedData || sec != SecBogus {
				t.Fatalf("CD: %s AD %v %s", dns.RcodeToString[m.Rcode], m.AuthenticatedData, sec)
			}
		})
	}
}

func TestWrongTrustAnchor(t *testing.T) {
	w := newWorld(t)
	other := newZone(".", true)
	other.genKeys(t)
	r := w.resolver(func(c *Config) { c.TrustAnchors = []string{other.ksk.ToDS(dns.SHA256).String()} })
	m, sec := ask(t, r, "www.unsigned.org", dns.TypeA)
	if m.Rcode != dns.RcodeServerFailure || sec != SecBogus {
		t.Fatalf("with a foreign anchor even insecure zones are bogus: %s %s", dns.RcodeToString[m.Rcode], sec)
	}
}

func TestUnsupportedAlgorithmIsInsecure(t *testing.T) {
	w := newWorld(t)
	// Pretend example.com's DS uses ED448 (16), which is not supported.
	w.com.rrs = slices.DeleteFunc(w.com.rrs, func(rr dns.RR) bool { return rr.Header().Rrtype == dns.TypeDS && rr.Header().Name == "example.com." })
	ds := w.example.ksk.ToDS(dns.SHA256)
	ds.Algorithm = dns.ED448
	w.com.rrs = append(w.com.rrs, ds)
	w.finalize()
	r := w.resolver()
	m, sec := ask(t, r, "www.example.com", dns.TypeA)
	want(t, m, sec, dns.RcodeSuccess, SecInsecure, "192.0.2.10")
}

func TestHiddenInsecureChild(t *testing.T) {
	// The example.com servers also host an unsigned child zone delegated
	// without DS: its unsigned answers are insecure, not bogus.
	w := newWorld(t)
	child := newZone("kid.example.com.", false)
	child.serveNS("ns1.example.com.", "").add("www.kid.example.com. 300 IN A 192.0.2.80")
	w.example.delegate(child, "ns1.example.com.", "")
	w.finalize()
	child.finalize(t)
	w.exampleSrv.zones = append(w.exampleSrv.zones, child)
	r := w.resolver()
	m, sec := ask(t, r, "www.kid.example.com", dns.TypeA)
	want(t, m, sec, dns.RcodeSuccess, SecInsecure, "192.0.2.80")
}

func TestGluelessDelegation(t *testing.T) {
	w := newWorld(t)
	r := w.resolver()
	m, sec := ask(t, r, "www.glueless.org", dns.TypeA)
	want(t, m, sec, dns.RcodeSuccess, SecSecure, "192.0.2.60")
	if !slices.Contains(w.net.queries(), "10.0.6.1 hosting.net. A") && !slices.Contains(w.net.queries(), "10.0.6.1 ns.hosting.net. A") {
		t.Fatalf("the name server's address was not looked up: %v", w.net.queries())
	}
}

func TestDSAndDNSKEYQueries(t *testing.T) {
	w := newWorld(t)
	r := w.resolver()
	m, sec := ask(t, r, "example.com", dns.TypeDS)
	want(t, m, sec, dns.RcodeSuccess, SecSecure)
	if len(rrsetOfType(m.Answer, dns.TypeDS)) != 1 {
		t.Fatalf("DS: %v", m.Answer)
	}
	m, sec = ask(t, r, "example.com", dns.TypeDNSKEY)
	want(t, m, sec, dns.RcodeSuccess, SecSecure)
	if len(rrsetOfType(m.Answer, dns.TypeDNSKEY)) != 2 {
		t.Fatalf("DNSKEY: %v", m.Answer)
	}
	// No DS for an insecure delegation, proven by the parent.
	m, sec = ask(t, r, "unsigned.org", dns.TypeDS)
	want(t, m, sec, dns.RcodeSuccess, SecSecure)
}

func TestTruncationUsesTCP(t *testing.T) {
	w := newWorld(t)
	w.exampleSrv.truncate = true
	r := w.resolver()
	m, sec := ask(t, r, "txt.example.com", dns.TypeTXT)
	want(t, m, sec, dns.RcodeSuccess, SecSecure)
	if w.net.tcp == 0 {
		t.Fatal("no TCP retry after truncation")
	}
}

func TestFailover(t *testing.T) {
	w := newWorld(t)
	// A second, broken server for example.com: refused, silent or lame.
	for _, mode := range []string{"refused", "silent", "lame"} {
		t.Run(mode, func(t *testing.T) {
			w := newWorld(t)
			w.com.add("example.com. 86400 IN NS ns2.example.com.", "ns2.example.com. 86400 IN A 10.0.2.2")
			w.finalize()
			bad := w.net.host("10.0.2.2", w.example)
			switch mode {
			case "refused":
				bad.refuse = true
			case "silent":
				bad.silent = true
			case "lame":
				bad.zones = []*tzone{w.org} // not authoritative for example.com
			}
			r := w.resolver()
			for range 4 {
				m, sec := ask(t, r, "www.example.com", dns.TypeA)
				want(t, m, sec, dns.RcodeSuccess, SecSecure, "192.0.2.10")
			}
		})
	}
	_ = w
}

func TestAllServersDown(t *testing.T) {
	w := newWorld(t)
	w.exampleSrv.silent = true
	r := w.resolver()
	m, _ := ask(t, r, "www.example.com", dns.TypeA)
	if code, _ := ede(m); m.Rcode != dns.RcodeServerFailure || code != dns.ExtendedErrorCodeNoReachableAuthority {
		t.Fatalf("%s EDE %d", dns.RcodeToString[m.Rcode], code)
	}
}

func TestCaseRandomization(t *testing.T) {
	w := newWorld(t)
	var names []string
	w.rootSrv.tamper = func(q, _ *dns.Msg) { names = append(names, q.Question[0].Name) }
	r := w.resolver(func(c *Config) { c.DNSSEC = false })
	for _, n := range []string{"www.example.com", "alias.example.com", "txt.example.com", "nope.example.com"} {
		ask(t, r, n, dns.TypeA)
		r.Flush()
	}
	mixed := false
	for _, n := range names {
		mixed = mixed || n != strings.ToLower(n)
	}
	if !mixed {
		t.Fatalf("no 0x20 in %v", names)
	}
	// A server that does not echo the case still works, and after a few
	// such answers (not one: a spoofer could send that) is asked without
	// 0x20.
	w.exampleSrv.noCase = true
	for i := range no0x20After {
		if !r.infra.supports0x20(netip.MustParseAddr("10.0.2.1")) {
			t.Fatalf("0x20 off after %d mismatches", i)
		}
		m, sec := ask(t, r, "www.example.com", dns.TypeA)
		want(t, m, sec, dns.RcodeSuccess, SecUnknown, "192.0.2.10")
	}
	if r.infra.supports0x20(netip.MustParseAddr("10.0.2.1")) {
		t.Fatal("the server should be remembered as not echoing case")
	}
}

func TestSpoofedAnswersIgnored(t *testing.T) {
	w := newWorld(t)
	// The example.com server answers a different question, and adds
	// out-of-bailiwick records: neither may be used.
	w.exampleSrv.tamper = func(q, r *dns.Msg) {
		if strings.EqualFold(q.Question[0].Name, "www.example.com.") {
			r.Question[0].Name = "evil.example.com."
		}
		r.Extra = append(r.Extra, &dns.A{Hdr: hdr("www.unsigned.org.", dns.TypeA, 300), A: []byte{6, 6, 6, 6}})
	}
	r := w.resolver(func(c *Config) { c.DNSSEC = false })
	m, _ := ask(t, r, "www.example.com", dns.TypeA)
	if m.Rcode != dns.RcodeServerFailure {
		t.Fatalf("a mismatched answer was accepted: %v", m)
	}
	m, sec := ask(t, r, "www.unsigned.org", dns.TypeA)
	want(t, m, sec, dns.RcodeSuccess, SecUnknown, "192.0.2.40")

	// Poisoned glue: a referral with glue for a name outside the
	// delegating zone is not believed.
	w = newWorld(t)
	w.comSrv.tamper = func(_, r *dns.Msg) {
		r.Extra = append(r.Extra, &dns.A{Hdr: hdr("a0.org.", dns.TypeA, 86400), A: []byte{6, 6, 6, 6}})
	}
	r = w.resolver(func(c *Config) { c.DNSSEC = false })
	ask(t, r, "www.example.com", dns.TypeA)
	m, sec = ask(t, r, "www.unsigned.org", dns.TypeA)
	want(t, m, sec, dns.RcodeSuccess, SecUnknown, "192.0.2.40")
	for _, q := range w.net.queries() {
		if strings.HasPrefix(q, "6.6.6.6") {
			t.Fatalf("asked a poisoned address: %v", w.net.queries())
		}
	}
}

func TestNXNSBounded(t *testing.T) {
	// A delegation to 200 glueless name servers that do not exist must
	// not make the resolver send hundreds of queries (NXNS attack).
	w := newWorld(t)
	evil := newZone("evil.org.", false)
	for i := range 200 {
		w.org.add(fmt.Sprintf("evil.org. 86400 IN NS ns%03d.victim.org.", i))
	}
	w.finalize()
	_ = evil
	r := w.resolver(func(c *Config) { c.DNSSEC = false })
	m, _ := ask(t, r, "www.evil.org", dns.TypeA)
	if m.Rcode != dns.RcodeServerFailure {
		t.Fatalf("rcode %s", dns.RcodeToString[m.Rcode])
	}
	if n := len(w.net.queries()); n > maxQueries {
		t.Fatalf("%d queries for one client query", n)
	}
}

func TestLocalZonesNeverLeave(t *testing.T) {
	w := newWorld(t)
	r := w.resolver()
	for _, tc := range []struct {
		name  string
		qtype uint16
		rcode int
	}{
		{"printer.home.arpa", dns.TypeA, dns.RcodeNameError},
		{"1.1.168.192.in-addr.arpa", dns.TypePTR, dns.RcodeNameError},
		{"5.0.0.10.in-addr.arpa", dns.TypePTR, dns.RcodeNameError},
		{"9.31.172.in-addr.arpa", dns.TypePTR, dns.RcodeNameError},
		{"nas.local", dns.TypeA, dns.RcodeNameError},
		{"svc.internal", dns.TypeAAAA, dns.RcodeNameError},
		{"10.in-addr.arpa", dns.TypeSOA, dns.RcodeSuccess},
		{"localhost", dns.TypeA, dns.RcodeSuccess},
	} {
		m, _ := ask(t, r, tc.name, tc.qtype)
		if m.Rcode != tc.rcode {
			t.Fatalf("%s: %s", tc.name, dns.RcodeToString[m.Rcode])
		}
	}
	if q := w.net.queries(); len(q) > 0 {
		t.Fatalf("local names left the network: %v", q)
	}
	// 172.32/16 is public.
	if _, ok := localZone("1.0.32.172.in-addr.arpa."); ok {
		t.Fatal("172.32.0.0 is not private")
	}
}

func TestCachesDelegations(t *testing.T) {
	w := newWorld(t)
	r := w.resolver()
	ask(t, r, "www.example.com", dns.TypeA)
	w.net.reset()
	m, sec := ask(t, r, "txt.example.com", dns.TypeTXT)
	want(t, m, sec, dns.RcodeSuccess, SecSecure)
	if q := w.net.queries(); len(q) != 1 || !strings.HasPrefix(q[0], "10.0.2.1 ") {
		t.Fatalf("a warm cache should ask example.com's server once: %v", q)
	}
}

func TestReconfigureKeepsOrFlushes(t *testing.T) {
	w := newWorld(t)
	r := w.resolver()
	ask(t, r, "www.example.com", dns.TypeA)
	n := r.CacheLen()
	cfg := r.set.Load().cfg
	cfg.QNAMEMinimization = false
	if err := r.Reconfigure(cfg); err != nil {
		t.Fatal(err)
	}
	if r.CacheLen() != n {
		t.Fatal("an unrelated change flushed the cache")
	}
	cfg.DNSSEC = false
	if err := r.Reconfigure(cfg); err != nil {
		t.Fatal(err)
	}
	if r.CacheLen() != 0 {
		t.Fatal("turning validation off must drop cached keys")
	}
	if err := r.Reconfigure(Config{IPv6: "maybe"}); err == nil {
		t.Fatal("bad ipv6 mode accepted")
	}
}

func TestContextDeadline(t *testing.T) {
	w := newWorld(t)
	w.rootSrv.silent = true
	r := w.resolver()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, err := r.ExchangeValidated(ctx, clientQuery("www.example.com", dns.TypeA))
	if err == nil {
		t.Fatal("expected the deadline error")
	}
}

func TestRealRootAnchorsParse(t *testing.T) {
	ds, err := ParseTrustAnchors(DefaultTrustAnchors)
	if err != nil || len(ds) != 2 || ds[0].KeyTag != 20326 || ds[1].KeyTag != 38696 {
		t.Fatalf("%v %v", ds, err)
	}
	if _, err := ParseTrustAnchors([]string{"20326 8 2 E06D44B80B8F1D39A95C0B0D7C65D08458E880409BBC683457104237C7F8EC8D"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTrustAnchors([]string{"example.com. IN DS 1 8 2 AA"}); err == nil {
		t.Fatal("non-root anchor accepted")
	}
	if _, err := ParseRootHints([]string{"198.41.0.4", "[2001:503:ba3e::2:30]:53", "not-an-ip"}); err == nil {
		t.Fatal("bad hint accepted")
	}
}

func TestNSEC3IterationLimit(t *testing.T) {
	w := newWorld(t)
	w.nsec3.iter = maxNSEC3Iter + 1
	w.finalize()
	r := w.resolver()
	m, sec := ask(t, r, "nope.nsec3.org", dns.TypeA)
	want(t, m, sec, dns.RcodeNameError, SecInsecure)
	if code, _ := ede(m); code != dns.ExtendedErrorCodeUnsupportedNSEC3IterValue {
		t.Fatalf("EDE %d", code)
	}
}

func TestSignatureBudget(t *testing.T) {
	// KeyTrap: many keys with the same tag and many signatures must not
	// make one query check hundreds of signatures.
	w := newWorld(t)
	r := w.resolver()
	ask(t, r, "www.example.com", dns.TypeA) // warm the chain
	zsk := w.example.zsk
	var keys []*dns.DNSKEY
	for range 50 {
		k := *zsk
		keys = append(keys, &k)
	}
	q := r.newQuery(context.Background(), r.set.Load())
	r.cache.set(q.key(kKeys, "example.com."), &keyState{sec: SecSecure, keys: keys}, time.Hour, time.Now())
	set := w.example.sets["www.example.com."][dns.TypeA]
	var sigs []*dns.RRSIG
	for range 50 {
		s := *w.example.sigs["www.example.com."][dns.TypeA][0].(*dns.RRSIG)
		s.Signature = strings.Repeat("A", len(s.Signature))
		sigs = append(sigs, &s)
	}
	v := q.validateRRset(set, sigs, "example.com.")
	if v.sec != SecBogus {
		t.Fatalf("sec %s", v.sec)
	}
	if used := maxSigBudget - q.st.sigBudget; used > maxSigTries+1 {
		t.Fatalf("%d signature checks for one RRset", used)
	}
}

func TestConcurrentQueries(t *testing.T) {
	w := newWorld(t)
	r := w.resolver()
	names := []string{"www.example.com", "alias.example.com", "www.unsigned.org", "nope.nsec3.org", "www.glueless.org", "x.wild.example.com", "www.insecure.com"}
	done := make(chan struct{})
	for i := range 32 {
		go func() {
			defer func() { done <- struct{}{} }()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			n := names[i%len(names)]
			m, _, err := r.ExchangeValidated(ctx, clientQuery(n, dns.TypeA))
			if err != nil || m.Rcode == dns.RcodeServerFailure {
				t.Errorf("%s: %v %v", n, err, m)
			}
		}()
	}
	for range 32 {
		<-done
	}
}

func TestLiveInternet(t *testing.T) {
	if testing.Short() || os.Getenv("KUDURRU_LIVE_DNS") == "" {
		t.Skip("set KUDURRU_LIVE_DNS=1 to resolve against the real root servers")
	}
	r, err := New(DefaultConfig(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		sec  Security
		rc   int
	}{
		{"example.com", SecSecure, dns.RcodeSuccess},
		{"www.isc.org", SecSecure, dns.RcodeSuccess},
		{"github.com", SecInsecure, dns.RcodeSuccess},
		{"dnssec-failed.org", SecBogus, dns.RcodeServerFailure},
		{"no-such-name-kudurru.isc.org", SecSecure, dns.RcodeNameError},
	} {
		m, sec := ask(t, r, tc.name, dns.TypeA)
		if sec != tc.sec || m.Rcode != tc.rc {
			t.Errorf("%s: %s %s, want %s %s", tc.name, dns.RcodeToString[m.Rcode], sec, dns.RcodeToString[tc.rc], tc.sec)
		}
	}
}

func TestLimitFailuresNotCached(t *testing.T) {
	// A query that runs out of budget must not leave cached failures
	// behind for the next one.
	w := newWorld(t)
	r := w.resolver()
	q := r.newQuery(context.Background(), r.set.Load())
	q.st.budget = 3
	a := q.resolve("www.glueless.org", dns.TypeA)
	if a.fail == nil && a.sec != SecBogus {
		t.Fatalf("a starved query should fail: %+v", a)
	}
	m, sec := ask(t, r, "www.glueless.org", dns.TypeA)
	want(t, m, sec, dns.RcodeSuccess, SecSecure, "192.0.2.60")
}
