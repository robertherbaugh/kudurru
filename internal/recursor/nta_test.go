package recursor

import (
	"context"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/dnssec"
)

// A private signed tree on actual UDP/TCP sockets. The root is valid;
// the delegated child has forged signatures and missing denial proofs.
func ntaNetwork(t *testing.T) (Config, *atomic.Int64) {
	t.Helper()
	root := newZone(".", true).serveNS("ns.root.", "127.0.0.1").add(
		"good.outside.example. 300 IN A 192.0.2.1",
		"bad.outside.example. 300 IN A 192.0.2.2",
	)
	child := newZone("broken.example.", true).serveNS("ns.broken.example.", "127.0.0.2").add(
		"www.broken.example. 300 IN A 192.0.2.3",
		"sub.www.broken.example. 300 IN A 192.0.2.4",
		"alias.broken.example. 300 IN CNAME www.broken.example.",
		"escape.broken.example. 300 IN CNAME bad.outside.example.",
		"old.broken.example. 300 IN DNAME outside.example.",
	)
	child.noDenial = true
	root.genKeys(t)
	child.genKeys(t)
	root.delegate(child, "ns.broken.example.", "127.0.0.2")
	root.finalize(t)
	child.finalize(t)
	// Keep malformed signatures encodable on real sockets. The in-memory
	// fixture's byte corruption can create invalid base64 and fail packing.
	for _, n := range []string{"www.broken.example.", "sub.www.broken.example.", "alias.broken.example.", "escape.broken.example.", "old.broken.example.", "broken.example."} {
		for _, sigs := range child.sigs[n] {
			for _, rr := range sigs {
				rr.(*dns.RRSIG).Expiration = 1
			}
		}
	}
	for _, rr := range root.sigs["bad.outside.example."][dns.TypeA] {
		rr.(*dns.RRSIG).Expiration = 1
	}
	var wireFailures atomic.Int64
	t.Cleanup(func() {
		if wireFailures.Load() != 0 {
			t.Fatalf("authoritative fixture encoding failures=%d", wireFailures.Load())
		}
	})

	var port string
	tcpQueries := &atomic.Int64{}
	for i, z := range []*tzone{root, child} {
		ip := "127.0.0." + strconv.Itoa(i+1)
		bindPort := port
		if bindPort == "" {
			bindPort = "0"
		}
		pc, err := net.ListenPacket("udp", net.JoinHostPort(ip, bindPort))
		if err != nil {
			t.Fatal("could not bind authoritative UDP fixture")
		}
		_, port, _ = net.SplitHostPort(pc.LocalAddr().String())
		ln, err := net.Listen("tcp", net.JoinHostPort(ip, port))
		if err != nil {
			_ = pc.Close()
			t.Fatal("could not bind authoritative TCP fixture")
		}
		handler := dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
			do := q.IsEdns0() != nil && q.IsEdns0().Do()
			m := z.answer(canon(q.Question[0].Name), q.Question[0].Qtype, do).Copy()
			m.Id, m.Response, m.Question = q.Id, true, append([]dns.Question(nil), q.Question...)
			if w.RemoteAddr().Network() == "tcp" {
				tcpQueries.Add(1)
			} else if i == 1 {
				m.Answer, m.Ns, m.Extra, m.Truncated = nil, nil, nil, true
			}
			if err := w.WriteMsg(m); err != nil {
				wireFailures.Add(1)
			}
		})
		for _, srv := range []*dns.Server{{PacketConn: pc, Handler: handler}, {Listener: ln, Handler: handler}} {
			started := make(chan struct{})
			srv.NotifyStartedFunc = func() { close(started) }
			go func() { _ = srv.ActivateAndServe() }()
			<-started
			t.Cleanup(func() { _ = srv.Shutdown() })
		}
	}
	cfg := DefaultConfig()
	cfg.TrustAnchors = []string{root.ksk.ToDS(dns.SHA256).String()}
	cfg.RootHints = []string{net.JoinHostPort("127.0.0.1", port)}
	cfg.IPv6, cfg.QNAMEMinimization = IPv6Off, false
	return cfg, tcpQueries
}

func TestRecursiveNTANetworkExpiryReloadAndAliases(t *testing.T) {
	cfg, tcpQueries := ntaNetwork(t)
	var clock atomic.Int64
	start := time.Now().UTC().Truncate(time.Second)
	clock.Store(start.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	r, err := New(cfg, Options{Now: now, AllowLoopback: true})
	if err != nil {
		t.Fatal("resolver did not construct")
	}
	check := func(resolver *Resolver, name string, rcode int, sec Security, do, ad, cd bool) {
		t.Helper()
		q := clientQuery(name, dns.TypeA)
		q.IsEdns0().SetDo(do)
		q.AuthenticatedData, q.CheckingDisabled = ad, cd
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		m, got, err := resolver.ExchangeValidated(ctx, q)
		if err != nil || m == nil {
			t.Fatal("recursive fixture exchange failed")
		}
		if m.Rcode != rcode || got != sec || m.AuthenticatedData != (sec == SecSecure && !cd && (do || ad)) || m.CheckingDisabled != cd {
			t.Fatalf("native result mismatch: rcode=%d state=%d ad=%t cd=%t", m.Rcode, got, m.AuthenticatedData, m.CheckingDisabled)
		}
		if name == "escape.broken.example." || name == "bad.old.broken.example." || name == "bad.outside.example." {
			code, ok := ede(m)
			if !ok || code != dns.ExtendedErrorCodeSignatureExpired {
				t.Fatalf("outside-target expired proof was not refused: ede=%d", code)
			}
		}
		for _, rr := range m.Answer {
			if !do && rr.Header().Rrtype == dns.TypeRRSIG {
				t.Fatal("proof returned without DO")
			}
			if sec == SecInsecure && rr.Header().Ttl > 60 {
				t.Fatal("exception TTL exceeded original expiry")
			}
		}
	}
	check(r, "www.broken.example.", dns.RcodeServerFailure, SecBogus, true, false, false)
	expiry := start.Add(time.Minute)
	cfg.NegativeTrustAnchors = []dnssec.NTA{{Domain: "BROKEN.EXAMPLE", Reason: "known signer incident", ExpiresAt: expiry}}
	if err := r.Reconfigure(cfg); err != nil {
		t.Fatal("recursive NTA reload failed")
	}
	active, until := r.TrustContext()
	if !until.Equal(expiry) {
		t.Fatal("reload changed absolute deadline")
	}
	for _, flags := range []struct{ do, ad, cd bool }{{true, false, false}, {false, true, false}, {false, false, false}, {true, true, true}} {
		check(r, "www.broken.example.", dns.RcodeSuccess, SecInsecure, flags.do, flags.ad, flags.cd)
	}
	check(r, "sub.www.broken.example.", dns.RcodeSuccess, SecInsecure, true, false, false)
	check(r, "alias.broken.example.", dns.RcodeSuccess, SecInsecure, true, false, false)
	check(r, "escape.broken.example.", dns.RcodeServerFailure, SecBogus, true, false, false)
	check(r, "bad.old.broken.example.", dns.RcodeServerFailure, SecBogus, true, false, false)
	check(r, "absent.broken.example.", dns.RcodeNameError, SecInsecure, true, false, false)
	check(r, "good.outside.example.", dns.RcodeSuccess, SecSecure, true, false, false)
	check(r, "bad.outside.example.", dns.RcodeServerFailure, SecBogus, true, false, false)
	if tcpQueries.Load() == 0 {
		t.Fatal("real recursive exchanges did not exercise TCP fallback")
	}
	// Identical reload cannot renew a minute that is almost over.
	clock.Store(expiry.Add(-time.Second).UnixNano())
	if err := r.Reconfigure(cfg); err != nil {
		t.Fatal("identical reload failed")
	}
	_, until = r.TrustContext()
	if !until.Equal(expiry) {
		t.Fatal("reload renewed exception")
	}
	clock.Store(expiry.UnixNano())
	expired, until := r.TrustContext()
	if expired == active || !until.IsZero() {
		t.Fatal("native cache context did not retire exactly at expiry")
	}
	check(r, "www.broken.example.", dns.RcodeServerFailure, SecBogus, true, false, false)
	check(r, "www.broken.example.", dns.RcodeSuccess, SecBogus, true, true, true)
	restored, err := New(cfg, Options{Now: now, AllowLoopback: true})
	if err != nil {
		t.Fatal("expired configuration was rejected on restart")
	}
	check(restored, "www.broken.example.", dns.RcodeServerFailure, SecBogus, true, false, false)
}

func TestRecursiveNTAExpiryDuringResolution(t *testing.T) {
	w := newWorld(t)
	start := time.Now()
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	expiry := start.Add(time.Minute)
	w.exampleSrv.tamper = func(q, _ *dns.Msg) {
		if canon(q.Question[0].Name) == "www.example.com." && q.Question[0].Qtype == dns.TypeA {
			clock.Store(expiry.UnixNano())
		}
	}
	cfg := DefaultConfig()
	cfg.RootHints, cfg.TrustAnchors = []string{"10.0.0.1"}, w.anchors()
	cfg.NegativeTrustAnchors = []dnssec.NTA{{Domain: "example.com.", Reason: "incident", ExpiresAt: expiry}}
	r, err := New(cfg, Options{Now: func() time.Time { return time.Unix(0, clock.Load()) }, Exchanger: w.net})
	if err != nil {
		t.Fatal("resolver did not construct")
	}
	m, state := ask(t, r, "www.example.com.", dns.TypeA)
	if m.Rcode != dns.RcodeServerFailure || state != SecBogus || m.AuthenticatedData {
		t.Fatal("in-flight expiry admitted an old exception")
	}
}
