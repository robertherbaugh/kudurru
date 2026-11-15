package recursor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/dnssec"
)

// The native exchanger uses actual UDP/TCP sockets and verifies this private
// root's signatures. Peer counts distinguish synthesis from an answer cache.
func aggressivePeer(t *testing.T, nsec3 bool, lines []string, alter func(*tzone, *dns.Msg, *dns.Msg), now func() time.Time) (*Resolver, *atomic.Int64) {
	t.Helper()
	// Keep NSEC3 and wildcards in a normal child zone: the existing signer
	// fixture does not encode root NSEC3 owner names or root wildcards.
	for i, line := range lines {
		if strings.HasPrefix(line, "*. ") {
			lines[i] = "*.example. " + strings.TrimPrefix(line, "*. ")
		}
	}
	z := newZone("example.", true).serveNS("example.", "127.0.0.1").add(lines...)
	z.nsec3 = nsec3
	z.genKeys(t)
	z.finalize(t)
	root := newZone(".", true).serveNS(".", "127.0.0.1")
	root.genKeys(t)
	root.delegate(z, "example.", "127.0.0.1")
	root.finalize(t)
	count := &atomic.Int64{}
	server, _, _ := authoritySocket(t, func(q, m *dns.Msg, _ bool) {
		count.Add(1)
		zone := root
		qn, qt := canon(q.Question[0].Name), q.Question[0].Qtype
		if isSubOrEqual(z.name, qn) && (qt != dns.TypeDS || !equalName(qn, z.name)) {
			zone = z
		}
		a := zone.answer(qn, qt, true).Copy()
		a.Id, a.Response, a.Question = q.Id, true, append([]dns.Question(nil), q.Question...)
		if alter != nil {
			alter(zone, q, a)
		}
		*m = *a
	})
	cfg := DefaultConfig()
	cfg.IPv6, cfg.QNAMEMinimization = IPv6Off, false
	cfg.RootHints = []string{server.String()}
	cfg.TrustAnchors = []string{root.ksk.ToDS(dns.SHA256).String()}
	r, err := New(cfg, Options{AllowLoopback: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return r, count
}

func TestAggressiveRealPeerNegative(t *testing.T) {
	for _, n3 := range []bool{false, true} {
		t.Run(fmt.Sprintf("nsec3=%t", n3), func(t *testing.T) {
			r, count := aggressivePeer(t, n3, nil, nil, nil)
			m, sec := ask(t, r, "one.example", dns.TypeA)
			want(t, m, sec, dns.RcodeNameError, SecSecure)
			before := count.Load()
			if before == 0 {
				t.Fatal("no authority traffic")
			}
			// A different name and type must be answered without any exchange.
			m, sec = ask(t, r, "two.example", dns.TypeAAAA)
			want(t, m, sec, dns.RcodeNameError, SecSecure)
			if count.Load() != before || len(rrsetOfType(m.Ns, dns.TypeSOA)) != 1 {
				t.Fatal("covered name was not synthesized with its SOA")
			}
			for _, rr := range m.Ns {
				rr.Header().Ttl = 0
			}
			m, sec = ask(t, r, "three.example", dns.TypeTXT, noDO)
			if m.Rcode != dns.RcodeNameError || sec != SecSecure || m.AuthenticatedData || len(m.Ns) != 1 || m.Ns[0].Header().Ttl == 0 || count.Load() != before {
				t.Fatal("DO handling or detached cache ownership failed")
			}
			_, _ = ask(t, r, "four.example", dns.TypeA, cd)
			if count.Load() <= before {
				t.Fatal("CD query used aggressive synthesis")
			}
		})
	}
}

func TestAggressiveRealPeerExactNoData(t *testing.T) {
	for _, n3 := range []bool{false, true} {
		t.Run(fmt.Sprintf("nsec3=%t", n3), func(t *testing.T) {
			r, count := aggressivePeer(t, n3, []string{"www.example. 300 IN A 192.0.2.3"}, nil, nil)
			m, sec := ask(t, r, "www.example", dns.TypeMX)
			want(t, m, sec, dns.RcodeSuccess, SecSecure)
			before := count.Load()
			m, sec = ask(t, r, "www.example", dns.TypeTXT)
			want(t, m, sec, dns.RcodeSuccess, SecSecure)
			if len(m.Answer) != 0 || count.Load() != before {
				t.Fatal("exact NODATA did not cover another absent type")
			}
			m, sec = ask(t, r, "www.example", dns.TypeA)
			want(t, m, sec, dns.RcodeSuccess, SecSecure, "192.0.2.3")
			if count.Load() <= before {
				t.Fatal("denial hid an existing type")
			}
		})
	}
}

func TestAggressiveRealPeerWildcard(t *testing.T) {
	for _, n3 := range []bool{false, true} {
		t.Run(fmt.Sprintf("nsec3=%t", n3), func(t *testing.T) {
			r, count := aggressivePeer(t, n3, []string{"*. 300 IN A 192.0.2.4"}, nil, nil)
			m, sec := ask(t, r, "one.branch.example", dns.TypeA)
			want(t, m, sec, dns.RcodeSuccess, SecSecure, "192.0.2.4")
			before := count.Load()
			m, sec = ask(t, r, "two.branch.example", dns.TypeA)
			want(t, m, sec, dns.RcodeSuccess, SecSecure, "192.0.2.4")
			if count.Load() != before {
				t.Fatal("wildcard answer contacted authority")
			}
			for _, rr := range m.Answer {
				if rr.Header().Name != "two.branch.example." {
					t.Fatal("cached expansion retained the first owner")
				}
			}
			if len(m.Ns) == 0 {
				t.Fatal("wildcard exclusion proof omitted")
			}
		})
	}
}

func wildcardNoData(z *tzone, q, m *dns.Msg) {
	if q.Question[0].Qtype != dns.TypeTXT {
		return
	}
	name := canon(q.Question[0].Name)
	if name == z.name {
		return
	}
	m.Rcode = dns.RcodeSuccess
	m.Ns = z.withSigs(nil, z.name, dns.TypeSOA, true)
	m.Ns = z.denial(m.Ns, name, false, true)
	if z.nsec3 {
		budget := maxNSEC3Hashes
		s, _ := newNSEC3Set(z.n3, z.name, &budget)
		if rr := s.match(wildcardOf(z.name)); rr != nil {
			m.Ns = append(m.Ns, withSig(z, rr)...)
		}
	}
}

func TestAggressiveRealPeerWildcardNoData(t *testing.T) {
	for _, n3 := range []bool{false, true} {
		t.Run(fmt.Sprintf("nsec3=%t", n3), func(t *testing.T) {
			r, count := aggressivePeer(t, n3, []string{"*. 300 IN A 192.0.2.4"}, wildcardNoData, nil)
			m, sec := ask(t, r, "one.example", dns.TypeTXT)
			want(t, m, sec, dns.RcodeSuccess, SecSecure)
			before := count.Load()
			m, sec = ask(t, r, "two.example", dns.TypeMX)
			want(t, m, sec, dns.RcodeSuccess, SecSecure)
			if count.Load() != before || len(m.Answer) != 0 {
				t.Fatal("wildcard NODATA was not synthesized")
			}
		})
	}
}

func TestAggressiveWildcardAliasRetainsOriginalDeadline(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(fmt.Sprintf("expire=%t", expire), func(t *testing.T) {
			initial := time.Now().Truncate(time.Second)
			var clock atomic.Int64
			clock.Store(initial.UnixNano())
			r, count := aggressivePeer(t, false, []string{
				"*.wild.example. 2 IN CNAME target.example.",
				"target.example. 300 IN A 192.0.2.8",
			}, func(_ *tzone, q, _ *dns.Msg) {
				if expire && equalName(q.Question[0].Name, "target.example.") && q.Question[0].Qtype == dns.TypeA {
					clock.Store(initial.Add(2 * time.Second).UnixNano())
				}
			}, func() time.Time { return time.Unix(0, clock.Load()) })
			m, sec := ask(t, r, "one.wild.example", dns.TypeCNAME)
			want(t, m, sec, dns.RcodeSuccess, SecSecure, "CNAME target.example.")
			before := count.Load()
			m, sec = ask(t, r, "two.wild.example", dns.TypeA)
			if expire {
				want(t, m, sec, dns.RcodeServerFailure, SecBogus)
				if code, _ := ede(m); code != dns.ExtendedErrorCodeSignatureExpired {
					t.Fatal("lost original wildcard deadline")
				}
			} else {
				want(t, m, sec, dns.RcodeSuccess, SecSecure, "CNAME target.example.", "192.0.2.8")
			}
			if count.Load() != before+1 {
				t.Fatal("cached alias did not retain the normal target walk")
			}
		})
	}
}

func TestAggressiveWildcardCannotReplaceExistingCloserName(t *testing.T) {
	r, count := aggressivePeer(t, false, []string{
		"*.wild.example. 300 IN A 192.0.2.4",
		"real.wild.example. 300 IN A 192.0.2.5",
	}, nil, nil)
	_, _ = ask(t, r, "one.wild.example", dns.TypeA)
	before := count.Load()
	m, sec := ask(t, r, "real.wild.example", dns.TypeA)
	want(t, m, sec, dns.RcodeSuccess, SecSecure, "192.0.2.5")
	if count.Load() <= before {
		t.Fatal("wildcard hid existing closer name")
	}
	m, sec = ask(t, r, "below.real.wild.example", dns.TypeA)
	want(t, m, sec, dns.RcodeNameError, SecSecure)
}

func TestAggressiveOriginalExpiryAndClock(t *testing.T) {
	for _, scenario := range []string{"ttl", "signature", "zero", "rollback"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			initial := now
			r, count := aggressivePeer(t, false, nil, func(z *tzone, _ *dns.Msg, m *dns.Msg) {
				for _, rr := range m.Ns {
					switch scenario {
					case "ttl":
						rr.Header().Ttl = 2
					case "zero":
						rr.Header().Ttl = 0
					case "signature":
						if sig, ok := rr.(*dns.RRSIG); ok {
							sig.Expiration = uint32(initial.Add(2 * time.Second).Unix()) //nolint:gosec // test time
							if err := sig.Sign(z.zskKey, rrsetOf(m.Ns, sig.Hdr.Name, sig.TypeCovered)); err != nil {
								t.Error(err)
							}
						}
					}
				}
			}, func() time.Time { return now })
			m, sec := ask(t, r, "one.example", dns.TypeA)
			want(t, m, sec, dns.RcodeNameError, SecSecure)
			before := count.Load()
			switch scenario {
			case "ttl", "signature":
				now = now.Add(time.Second)
				m, sec = ask(t, r, "two.example", dns.TypeAAAA)
				want(t, m, sec, dns.RcodeNameError, SecSecure)
				if count.Load() != before || m.Ns[0].Header().Ttl != 1 {
					t.Fatal("original short deadline extended")
				}
				now = now.Add(time.Second)
			case "rollback":
				now = now.Add(-time.Second)
			}
			_, _ = ask(t, r, "three.example", dns.TypeA)
			if count.Load() <= before {
				t.Fatal("expired, zero-TTL or backward-time proof reused")
			}
		})
	}
}

func TestAggressiveContextFlushAndCancellation(t *testing.T) {
	r, count := aggressivePeer(t, false, nil, nil, nil)
	_, _ = ask(t, r, "one.example", dns.TypeA)
	q := r.newQuery(t.Context(), r.set.Load())
	proofs := r.denials.candidates(q.key("denial|", "example."), q.st.denialEpoch, time.Now(), 1)
	if len(proofs) != 1 {
		t.Fatal("missing validated proof")
	}
	r.Flush()
	q.rememberDenial(proofs[0])
	if r.denials.entries.Len() != 0 {
		t.Fatal("held query refilled flushed cache")
	}
	before := count.Load()
	_, _ = ask(t, r, "two.example", dns.TypeA)
	if count.Load() <= before {
		t.Fatal("flush retained proof")
	}
	cfg := r.Config()
	cfg.NegativeTrustAnchors = []dnssec.NTA{{Domain: "example.", Reason: "incident", ExpiresAt: time.Now().Add(time.Minute)}}
	if err := r.Reconfigure(cfg); err != nil {
		t.Fatal(err)
	}
	before = count.Load()
	m, sec := ask(t, r, "three.example", dns.TypeA)
	if sec != SecInsecure || m.AuthenticatedData || count.Load() <= before {
		t.Fatal("new exception context reused secure proof")
	}
	cfg.NegativeTrustAnchors = nil
	if err := r.Reconfigure(cfg); err != nil {
		t.Fatal(err)
	}
	_, _ = ask(t, r, "four.example", dns.TypeA)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := r.ExchangeValidated(ctx, clientQuery("five.example", dns.TypeA))
	if !errors.Is(err, context.Canceled) {
		t.Fatal("canceled cache request did not retain cancellation")
	}
}

func TestAggressiveHeldResponseCannotRefillAfterTrustChange(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	r, count := aggressivePeer(t, false, nil, func(_ *tzone, q, _ *dns.Msg) {
		if equalName(q.Question[0].Name, "hold.example.") && held.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	type result struct {
		msg *dns.Msg
		sec Security
		err error
	}
	done := make(chan result, 1)
	go func() {
		m, sec, err := r.ExchangeValidated(ctx, clientQuery("hold.example", dns.TypeA))
		done <- result{m, sec, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		close(release)
		t.Fatal("authority did not receive original query")
	}
	cfg := r.Config()
	cfg.TrustAnchors = append([]string(nil), DefaultTrustAnchors...)
	err := r.Reconfigure(cfg)
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	first := <-done
	if first.err != nil {
		t.Fatal(first.err)
	}
	want(t, first.msg, first.sec, dns.RcodeNameError, SecSecure)
	if r.denials.entries.Len() != 0 {
		t.Fatal("old publication refilled current proof cache")
	}
	before := count.Load()
	m, sec := ask(t, r, "new.example", dns.TypeA)
	want(t, m, sec, dns.RcodeServerFailure, SecBogus)
	if count.Load() <= before {
		t.Fatal("new trust context reused old proof")
	}
}

func TestAggressiveRetainsOnlyVerifiedRecords(t *testing.T) {
	r, count := aggressivePeer(t, false, nil, func(z *tzone, _ *dns.Msg, m *dns.Msg) {
		if len(m.Ns) == 0 {
			return
		}
		// An unverified sibling signature and an unsigned foreign SOA are
		// not cache evidence, even when the complete response validates.
		junk := dns.Copy(z.sigs[z.name][dns.TypeNSEC][0]).(*dns.RRSIG)
		junk.Expiration = 1
		m.Ns = append([]dns.RR{junk}, m.Ns...)
		foreign, _ := dns.NewRR("unrelated. 0 IN SOA ns.unrelated. h.unrelated. 1 2 3 4 0")
		m.Ns = append(m.Ns, foreign)
	}, nil)
	_, _ = ask(t, r, "one.example", dns.TypeA)
	before := count.Load()
	m, sec := ask(t, r, "two.example", dns.TypeA)
	want(t, m, sec, dns.RcodeNameError, SecSecure)
	if count.Load() != before {
		t.Fatal("unverified sibling controlled cache lifetime")
	}
	for _, rr := range m.Ns {
		if sig, ok := rr.(*dns.RRSIG); ok && sig.Expiration == 1 {
			t.Fatal("unverified signature retained")
		}
		if equalName(rr.Header().Name, "unrelated.") {
			t.Fatal("unverified SOA retained")
		}
	}
}

func TestAggressiveDelegationDNAMEAndEmptyNonterminal(t *testing.T) {
	w := newWorld(t)
	r := w.resolver(noMinimize)
	_, _ = ask(t, r, "deep.example.com", dns.TypeAAAA)
	w.net.reset()
	m, sec := ask(t, r, "deep.example.com", dns.TypeTXT)
	want(t, m, sec, dns.RcodeSuccess, SecSecure)
	if len(w.net.queries()) != 0 {
		t.Fatal("empty nonterminal was not reused as NODATA")
	}
	_, _ = ask(t, r, "old.example.com", dns.TypeTXT)
	m, sec = ask(t, r, "www.old.example.com", dns.TypeA)
	want(t, m, sec, dns.RcodeSuccess, SecSecure, "CNAME www.nsec3.org.", "192.0.2.50")
	_, _ = ask(t, r, "unsigned.org", dns.TypeDS)
	m, sec = ask(t, r, "www.unsigned.org", dns.TypeA)
	want(t, m, sec, dns.RcodeSuccess, SecInsecure, "192.0.2.40")
	_, _ = ask(t, r, "nope.com", dns.TypeA) // opt-out
	w.net.reset()
	_, _ = ask(t, r, "another.com", dns.TypeA)
	if len(w.net.queries()) == 0 {
		t.Fatal("opt-out range synthesized absence")
	}
}

func TestAggressiveBoundedStorageAndWork(t *testing.T) {
	r, _ := aggressivePeer(t, true, nil, nil, nil)
	_, _ = ask(t, r, "one.example", dns.TypeA)
	q := r.newQuery(t.Context(), r.set.Load())
	p := r.denials.candidates(q.key("denial|", "example."), q.st.denialEpoch, time.Now(), 1)[0]
	for i := range maxDenialEntries + 1 {
		r.denials.put(fmt.Sprint(i), q.st.denialEpoch, p, maxDenialProofBytes)
	}
	if r.denials.entries.Len() > maxDenialEntries || r.denials.bytes > maxDenialBytes {
		t.Fatal("proof storage exceeded bounds")
	}
	for range maxDenialPerZone + 1 {
		r.denials.put("zone", q.st.denialEpoch, p, 1)
	}
	if len(r.denials.zones["zone"]) != maxDenialPerZone {
		t.Fatal("zone candidate bound failed")
	}
	// A long unmatched closest-encloser walk cannot consume the allowance
	// reserved for ordinary recursive validation.
	r.denials.flush()
	q = r.newQuery(t.Context(), r.set.Load())
	for range maxDenialPerZone {
		r.denials.put(q.key("denial|", "example."), q.st.denialEpoch, p, 1)
	}
	for range 12 {
		q.cachedDenial(strings.Repeat("label.", 40)+"example.", dns.TypeA)
	}
	if q.st.hashBudget < maxNSEC3Hashes-16 {
		t.Fatal("synthesis consumed ordinary validation budget")
	}
}

func TestAggressiveAliasMissesReserveNormalValidation(t *testing.T) {
	suffix := strings.Repeat("deep.", 25) + "example."
	var records []string
	for i := range 8 {
		target := fmt.Sprintf("alias%d.%s", i+1, suffix)
		if i == 7 {
			target = "missing." + suffix
		}
		records = append(records, fmt.Sprintf("alias%d.%s 300 IN CNAME %s", i, suffix, target))
	}
	r, count := aggressivePeer(t, true, records, nil, nil)
	m, sec := ask(t, r, "seed.example", dns.TypeA)
	want(t, m, sec, dns.RcodeNameError, SecSecure)
	before := count.Load()
	m, sec = ask(t, r, "alias0."+suffix, dns.TypeA)
	want(t, m, sec, dns.RcodeNameError, SecSecure)
	if len(rrsetOfType(m.Answer, dns.TypeCNAME)) != 8 || count.Load() <= before {
		t.Fatal("signed multi-response alias walk was not exercised")
	}
}

func TestAggressiveConcurrentCopiesAndFlush(t *testing.T) {
	r, _ := aggressivePeer(t, false, nil, nil, nil)
	_, _ = ask(t, r, "one.example", dns.TypeA)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errors := make(chan string, 8)
	for i := range 8 {
		wg.Go(func() {
			for j := range 10 {
				if i == 0 {
					r.Flush()
				}
				m, sec, err := r.ExchangeValidated(ctx, clientQuery(fmt.Sprintf("n%d-%d.example", i, j), dns.TypeA))
				if err != nil || m == nil || sec != SecSecure || m.Rcode != dns.RcodeNameError {
					errors <- "concurrent resolution failed"
					return
				}
				for _, rr := range m.Ns {
					rr.Header().Name = "changed.invalid."
				}
			}
		})
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	m, sec := ask(t, r, "last.example", dns.TypeA)
	want(t, m, sec, dns.RcodeNameError, SecSecure)
	for _, rr := range m.Ns {
		if equalName(rr.Header().Name, "changed.invalid.") {
			t.Fatal("consumer mutated shared proof")
		}
	}
}

func BenchmarkAggressiveDenial(b *testing.B) {
	for _, zone := range []string{"example.com.", "nsec3.org."} {
		b.Run(zone, func(b *testing.B) {
			w := newWorld(b)
			r := w.resolver(noMinimize)
			if _, _, err := r.ExchangeValidated(b.Context(), clientQuery("nope."+zone, dns.TypeA)); err != nil {
				b.Fatal(err)
			}
			name := "child.nope." + zone
			if r.newQuery(b.Context(), r.set.Load()).cachedDenial(name, dns.TypeA) == nil {
				b.Fatal("benchmark lacks reusable proof")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if r.newQuery(b.Context(), r.set.Load()).cachedDenial(name, dns.TypeA) == nil {
					b.Fatal("proof expired")
				}
			}
		})
	}
}
