package recursor

import (
	"strings"
	"testing"

	"github.com/miekg/dns"
)

// Forged answers built from real, replayable signed records of other
// names or zones must never validate (review findings, D170).

func n3set(z *tzone) *nsec3Set {
	budget := 1000
	return &nsec3Set{recs: z.n3, alg: dns.SHA1, iter: z.iter, hashes: map[string]string{}, budget: &budget}
}

func withSig(z *tzone, rr dns.RR) []dns.RR {
	h := rr.Header()
	return append([]dns.RR{rr}, z.sigs[h.Name][h.Rrtype]...)
}

func spoofFor(name string, f func(q, r *dns.Msg)) func(q, r *dns.Msg) {
	return func(q, r *dns.Msg) {
		if strings.EqualFold(q.Question[0].Name, name) {
			f(q, r)
		}
	}
}

func expectBogus(t *testing.T, r *Resolver, name string, qtype uint16) {
	t.Helper()
	m, sec := ask(t, r, name, qtype)
	if m.Rcode != dns.RcodeServerFailure || sec != SecBogus {
		t.Fatalf("forgery accepted: %s %s %v", dns.RcodeToString[m.Rcode], sec, m.Answer)
	}
}

func noMinimize(c *Config) { c.QNAMEMinimization = false }

func TestSpoofedDenialBorrowsInsecureSigner(t *testing.T) {
	// com's server (spoofed) denies www.example.com with an SOA "signed"
	// by the insecure zone insecure.com.
	w := newWorld(t)
	w.comSrv.tamper = spoofFor("www.example.com.", func(_, r *dns.Msg) {
		r.Rcode, r.Authoritative = dns.RcodeNameError, true
		r.Answer, r.Extra = nil, nil
		soa, _ := dns.NewRR("insecure.com. 300 IN SOA ns.insecure.com. h.insecure.com. 1 2 3 4 300")
		sig := *w.com.sigs["com."][dns.TypeSOA][0].(*dns.RRSIG)
		sig.Hdr.Name, sig.SignerName = "insecure.com.", "insecure.com."
		r.Ns = []dns.RR{soa, &sig}
	})
	expectBogus(t, w.resolver(noMinimize), "www.example.com", dns.TypeA)
}

func TestSpoofedNSEC3DSProofAtDelegation(t *testing.T) {
	// An unsigned answer for a name below the signed example.com, "made
	// insecure" by com's NSEC3 for the delegation example.com plus an
	// opt-out span: the delegation cannot be a closest encloser.
	w := newWorld(t)
	set := n3set(w.com)
	w.comSrv.tamper = func(q, r *dns.Msg) {
		switch qn := strings.ToLower(q.Question[0].Name); {
		case qn == "www.example.com." && q.Question[0].Qtype == dns.TypeA:
			a, _ := dns.NewRR("www.example.com. 300 IN A 6.6.6.6")
			r.Rcode, r.Authoritative, r.Answer, r.Ns, r.Extra = dns.RcodeSuccess, true, []dns.RR{a}, nil, nil
		case qn == "www.example.com." && q.Question[0].Qtype == dns.TypeDS:
			r.Rcode, r.Authoritative, r.Answer, r.Extra = dns.RcodeSuccess, true, nil, nil
			r.Ns = withSig(w.com, w.com.sets["com."][dns.TypeSOA][0])
			r.Ns = append(r.Ns, withSig(w.com, set.match("example.com."))...)
			r.Ns = append(r.Ns, withSig(w.com, set.cover("www.example.com."))...)
		}
	}
	expectBogus(t, w.resolver(noMinimize), "www.example.com", dns.TypeA)
}

func TestSpoofedNSEC3MixesZones(t *testing.T) {
	// Parent and child both use NSEC3 with the same parameters: the
	// child's apex NSEC3 and the parent's spans must not combine into an
	// NXDOMAIN for a name that exists.
	w := newWorld(t)
	w.example.nsec3 = true
	w.finalize()
	child, parent := n3set(w.example), n3set(w.com)
	w.comSrv.tamper = spoofFor("www.example.com.", func(_, r *dns.Msg) {
		r.Rcode, r.Authoritative, r.Answer, r.Extra = dns.RcodeNameError, true, nil, nil
		r.Ns = withSig(w.com, w.com.sets["com."][dns.TypeSOA][0])
		r.Ns = append(r.Ns, withSig(w.example, child.match("example.com."))...)
		r.Ns = append(r.Ns, withSig(w.com, parent.cover("www.example.com."))...)
		r.Ns = append(r.Ns, withSig(w.com, parent.cover("*.example.com."))...)
	})
	expectBogus(t, w.resolver(noMinimize), "www.example.com", dns.TypeA)
}

func TestSpoofedWildcardBelowExistingName(t *testing.T) {
	// real.wild.example.com exists, so x.real.wild.example.com is
	// NXDOMAIN; a replayed *.wild.example.com signature with the NSEC of
	// real.wild.example.com must not pass as a wildcard answer.
	w := newWorld(t)
	w.example.add("real.wild.example.com. 300 IN A 192.0.2.98")
	w.finalize()
	w.exampleSrv.tamper = spoofFor("x.real.wild.example.com.", func(_, r *dns.Msg) {
		a := dns.Copy(w.example.sets["*.wild.example.com."][dns.TypeA][0])
		a.Header().Name = "x.real.wild.example.com."
		sig := dns.Copy(w.example.sigs["*.wild.example.com."][dns.TypeA][0])
		sig.Header().Name = "x.real.wild.example.com."
		r.Rcode, r.Authoritative, r.Answer, r.Extra = dns.RcodeSuccess, true, []dns.RR{a, sig}, nil
		r.Ns = nil
		for _, n := range w.example.nsecs {
			if n.Hdr.Name == "real.wild.example.com." {
				r.Ns = withSig(w.example, n)
			}
		}
	})
	r := w.resolver()
	expectBogus(t, r, "x.real.wild.example.com", dns.TypeA)
	// The real wildcard still works.
	m, sec := ask(t, r, "y.wild.example.com", dns.TypeA)
	want(t, m, sec, dns.RcodeSuccess, SecSecure, "192.0.2.99")
}

func TestJunkSignatureFirstDoesNotDecide(t *testing.T) {
	// A junk RRSIG from a non-zone signer listed before the real one must
	// not turn good data bogus.
	w := newWorld(t)
	w.exampleSrv.tamper = spoofFor("www.example.com.", func(_, r *dns.Msg) {
		good := *w.example.sigs["www.example.com."][dns.TypeA][0].(*dns.RRSIG)
		junk := good
		junk.SignerName = "www.example.com."
		var out []dns.RR
		for _, rr := range r.Answer {
			if rr.Header().Rrtype != dns.TypeRRSIG {
				out = append(out, rr)
			}
		}
		r.Answer = append(out, &junk, &good)
	})
	m, sec := ask(t, w.resolver(), "www.example.com", dns.TypeA)
	want(t, m, sec, dns.RcodeSuccess, SecSecure, "192.0.2.10")
}

func TestCNAMEIntoLocalZoneStaysLocal(t *testing.T) {
	w := newWorld(t)
	w.example.add("printer.example.com. 300 IN CNAME printer.home.arpa.")
	w.finalize()
	r := w.resolver()
	m, _ := ask(t, r, "printer.example.com", dns.TypeA)
	if m.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode %s", dns.RcodeToString[m.Rcode])
	}
	for _, q := range w.net.queries() {
		if strings.Contains(q, "home.arpa") {
			t.Fatalf("a local name left the network: %v", w.net.queries())
		}
	}
}
