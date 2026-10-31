package recursor

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/dnssec"
)

// All diagnostics describe counts/booleans, never cookie bytes or messages.
func authoritySocket(t *testing.T, handler func(*dns.Msg, *dns.Msg, bool)) (netip.AddrPort, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("authority UDP fixture bind failed")
	}
	ln, err := net.Listen("tcp", pc.LocalAddr().String())
	if err != nil {
		_ = pc.Close()
		t.Fatal("authority TCP fixture bind failed")
	}
	udp, tcp, wireErrors := &atomic.Int64{}, &atomic.Int64{}, &atomic.Int64{}
	h := dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		isTCP := w.RemoteAddr().Network() == "tcp"
		if isTCP {
			tcp.Add(1)
		} else {
			udp.Add(1)
		}
		m := new(dns.Msg)
		m.SetReply(q)
		m.Authoritative = true
		m.Answer = []dns.RR{&dns.A{Hdr: hdr(q.Question[0].Name, dns.TypeA, 60), A: net.IPv4(192, 0, 2, 1)}}
		handler(q, m, isTCP)
		if !m.Response {
			return
		}
		if err := w.WriteMsg(m); err != nil {
			wireErrors.Add(1)
		}
	})
	t.Cleanup(func() {
		if wireErrors.Load() != 0 {
			t.Fatalf("fixture encoding failures=%d", wireErrors.Load())
		}
	})
	for _, srv := range []*dns.Server{{PacketConn: pc, Handler: h}, {Listener: ln, Handler: h}} {
		started := make(chan struct{})
		srv.NotifyStartedFunc = func() { close(started) }
		go func() { _ = srv.ActivateAndServe() }()
		<-started
		t.Cleanup(func() { _ = srv.Shutdown() })
	}
	return netip.MustParseAddrPort(pc.LocalAddr().String()), udp, tcp
}

func wireCookie(q *dns.Msg) string {
	if opt := q.IsEdns0(); opt != nil {
		for _, v := range opt.Option {
			if c, ok := v.(*dns.EDNS0_COOKIE); ok {
				return c.Cookie
			}
		}
	}
	return ""
}

func cookieReply(m *dns.Msg, value string) {
	if m.IsEdns0() == nil {
		m.SetEdns0(ednsSize, false)
	}
	m.IsEdns0().Option = append(m.IsEdns0().Option, &dns.EDNS0_COOKIE{Code: dns.EDNS0COOKIE, Cookie: value})
}

func cookieQuery(t *testing.T, now func() time.Time) *query {
	t.Helper()
	r, err := New(DefaultConfig(), Options{Now: now, AllowLoopback: true})
	if err != nil {
		t.Fatal("native resolver construction failed")
	}
	return r.newQuery(t.Context(), r.set.Load())
}

func sendCookie(t *testing.T, q *query, server netip.AddrPort) *dns.Msg {
	t.Helper()
	m, err := q.send(server, "www.example.", dns.TypeA)
	if err != nil || m == nil || m.Rcode != dns.RcodeSuccess {
		t.Fatal("native authority exchange failed")
	}
	if wireCookie(m) != "" {
		t.Fatal("transport cookie leaked into recursive answer")
	}
	return m
}

func TestAuthorityCookiesLearnRotateAndExpire(t *testing.T) {
	var received []string
	var mu sync.Mutex
	var version atomic.Int64
	server, _, _ := authoritySocket(t, func(q, m *dns.Msg, _ bool) {
		c := wireCookie(q)
		mu.Lock()
		received = append(received, c)
		mu.Unlock()
		if len(c) < 16 {
			return
		}
		suffix := strings.Repeat("01", 16)
		if version.Load() != 0 {
			suffix = strings.Repeat("02", 16)
		}
		cookieReply(m, c[:16]+suffix)
	})
	start := time.Now()
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	q := cookieQuery(t, func() time.Time { return time.Unix(0, clock.Load()) })
	for range 2 {
		sendCookie(t, q, server)
	}
	version.Store(1)
	for range 2 {
		sendCookie(t, q, server)
	}
	clock.Store(start.Add(serverCookieLife).UnixNano())
	sendCookie(t, q, server) // unchanged server cookies expire at the original deadline
	clock.Store(start.Add(serverCookieLife * 2).UnixNano())
	for range 2 {
		sendCookie(t, q, server)
	} // unchanged server cookie cannot renew itself
	clock.Store(start.Add(clientCookieLife).UnixNano())
	sendCookie(t, q, server)
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 8 || len(received[0]) != 16 || len(received[1]) != 48 || received[2] != received[1] || received[3] == received[2] || len(received[4]) != 16 || len(received[5]) != 16 || len(received[6]) != 16 || len(received[7]) != 16 || received[0] == received[7] {
		t.Fatalf("cookie lifetime/rotation invariant failed: count=%d", len(received))
	}
}

func TestAuthorityCookiesUnsupportedReprobe(t *testing.T) {
	var received []string
	var mu sync.Mutex
	server, udp, tcp := authoritySocket(t, func(q, _ *dns.Msg, _ bool) { mu.Lock(); received = append(received, wireCookie(q)); mu.Unlock() })
	start := time.Now()
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	q := cookieQuery(t, func() time.Time { return time.Unix(0, clock.Load()) })
	for range 2 {
		sendCookie(t, q, server)
	}
	clock.Store(start.Add(cookieProbeDelay).UnixNano())
	sendCookie(t, q, server)
	mu.Lock()
	defer mu.Unlock()
	if udp.Load() != 3 || tcp.Load() != 0 || len(received) != 3 || len(received[0]) != 16 || received[1] != "" || len(received[2]) != 16 || received[0] == received[2] {
		t.Fatal("unsupported authority reused exposed cookie or lost compatibility")
	}
}

func TestAuthorityCookiesBadCookieRecoveryAndWorkBound(t *testing.T) {
	for _, tc := range []struct {
		name             string
		failures         int64
		budget           int
		wantUDP, wantTCP int64
		wantErr          error
	}{
		{"refresh", 1, 10, 2, 0, nil},
		{"tcp fallback", 2, 10, 2, 1, nil},
		{"tcp refuses", 3, 10, 2, 1, errCookie},
		{"budget", 2, 1, 1, 0, errBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var count atomic.Int64
			var mismatch atomic.Bool
			server, udp, tcp := authoritySocket(t, func(q, m *dns.Msg, _ bool) {
				n := count.Add(1)
				c := wireCookie(q)
				if len(c) < 16 {
					mismatch.Store(true)
					return
				}
				if n > 1 && len(c) != 48 {
					mismatch.Store(true)
				}
				cookieReply(m, c[:16]+strings.Repeat(fmt.Sprintf("%02x", n), 16))
				if n <= tc.failures {
					m.Rcode = dns.RcodeBadCookie
					m.Answer = nil
				}
			})
			q := cookieQuery(t, time.Now)
			q.st.budget = tc.budget
			m, err := q.send(server, "www.example.", dns.TypeA)
			if !errors.Is(err, tc.wantErr) || (err == nil && (m == nil || m.Rcode != dns.RcodeSuccess)) || udp.Load() != tc.wantUDP || tcp.Load() != tc.wantTCP || mismatch.Load() || q.st.budget != tc.budget-int(tc.wantUDP+tc.wantTCP) && !errors.Is(err, errBudget) {
				t.Fatalf("BADCOOKIE bound failed: udp=%d tcp=%d failed=%t", udp.Load(), tcp.Load(), err != nil)
			}
			if errors.Is(err, errBudget) {
				q.r.infra.mu.Lock()
				stat := q.r.infra.m[server.Addr()]
				failed := stat != nil && stat.failures != 0
				q.r.infra.mu.Unlock()
				if failed {
					t.Fatal("local cookie retry budget exhaustion poisoned authority health")
				}
			}
		})
	}
}

func TestAuthorityCookiesRejectInvalidResponse(t *testing.T) {
	for _, mode := range []string{"wrong echo", "short", "client only", "oversized", "missing known", "missing expired", "BADCOOKIE missing", "question mismatch", "first invalid", "multiple OPT"} {
		t.Run(mode, func(t *testing.T) {
			var count atomic.Int64
			server, udp, tcp := authoritySocket(t, func(q, m *dns.Msg, _ bool) {
				n := count.Add(1)
				c := wireCookie(q)
				if len(c) < 16 {
					return
				}
				value := c[:16] + strings.Repeat("01", 16)
				if n == 1 && (mode == "missing known" || mode == "missing expired") {
					cookieReply(m, value)
					return
				}
				switch mode {
				case "wrong echo":
					value = strings.Repeat("ff", 24)
				case "short":
					value = "aa"
				case "client only":
					value = c[:16]
				case "oversized":
					value = c[:16] + strings.Repeat("01", 33)
				case "missing known", "missing expired":
					return
				case "BADCOOKIE missing":
					m.SetEdns0(ednsSize, false)
					m.Rcode = dns.RcodeBadCookie
					m.Answer = nil
					return
				case "question mismatch":
					m.Question[0].Qtype = dns.TypeAAAA
				case "first invalid":
					cookieReply(m, strings.Repeat("ff", 24))
				}
				cookieReply(m, value)
				if mode == "multiple OPT" {
					m.Extra = append(m.Extra, dns.Copy(m.IsEdns0()))
				}
			})
			var offset atomic.Int64
			q := cookieQuery(t, func() time.Time { return time.Now().Add(time.Duration(offset.Load())) })
			if mode == "missing known" || mode == "missing expired" {
				sendCookie(t, q, server)
				if mode == "missing expired" {
					offset.Store(int64(serverCookieLife))
				}
			}
			_, err := q.send(server, "www.example.", dns.TypeA)
			want := errCookie
			if mode == "question mismatch" {
				want = errMismatch
			}
			wantUDP := int64(1)
			if mode == "missing known" || mode == "missing expired" {
				wantUDP = 2
			}
			if !errors.Is(err, want) || udp.Load() != wantUDP || tcp.Load() != 0 {
				t.Fatalf("invalid cookie accepted or retried: udp=%d tcp=%d refused=%t", udp.Load(), tcp.Load(), err != nil)
			}
			jar := q.r.ex.(netExchanger).cookies
			jar.mu.Lock()
			defer jar.mu.Unlock()
			for _, p := range jar.peers {
				if mode != "missing known" && mode != "missing expired" && len(p.server) != 0 {
					t.Fatal("invalid response poisoned server cookie state")
				}
			}
		})
	}
}

func TestAuthorityCookiesTruncationAndCancellation(t *testing.T) {
	t.Run("truncation", func(t *testing.T) {
		var learned atomic.Bool
		server, udp, tcp := authoritySocket(t, func(q, m *dns.Msg, isTCP bool) {
			c := wireCookie(q)
			if len(c) < 16 {
				return
			}
			if isTCP {
				learned.Store(len(c) == 48)
			} else {
				m.Truncated = true
				m.Answer = nil
			}
			cookieReply(m, c[:16]+strings.Repeat("01", 16))
		})
		q := cookieQuery(t, time.Now)
		budget := q.st.budget
		sendCookie(t, q, server)
		if udp.Load() != 1 || tcp.Load() != 1 || !learned.Load() || q.st.budget != budget-2 {
			t.Fatal("TCP fallback lost negotiated cookie or budget accounting")
		}
	})
	for _, transport := range []string{"udp", "tcp"} {
		t.Run("cancel "+transport, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			server, _, _ := authoritySocket(t, func(q, m *dns.Msg, tcp bool) {
				c := wireCookie(q)
				if len(c) >= 16 {
					cookieReply(m, c[:16]+strings.Repeat("01", 16))
				}
				if transport == "tcp" && !tcp {
					m.Truncated = true
					m.Answer = nil
					return
				}
				close(entered)
				<-release
				m.Response = false
			})
			defer close(release)
			q := cookieQuery(t, time.Now)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			q.ctx = ctx
			done := make(chan error, 1)
			go func() { _, err := q.send(server, "www.example.", dns.TypeA); done <- err }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("authority fixture not reached")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal("cancellation identity lost")
				}
			case <-time.After(300 * time.Millisecond):
				t.Fatal("cancellation waited for transport timeout")
			}
		})
	}
}

func TestAuthorityCookiesConcurrentAndPeerScoped(t *testing.T) {
	var bad atomic.Bool
	server, _, _ := authoritySocket(t, func(q, m *dns.Msg, _ bool) {
		c := wireCookie(q)
		if len(c) < 16 {
			bad.Store(true)
			return
		}
		cookieReply(m, c[:16]+strings.Repeat("01", 16))
	})
	q := cookieQuery(t, time.Now)
	var wg sync.WaitGroup
	for range 24 {
		wg.Go(func() {
			query := q.r.newQuery(t.Context(), q.r.set.Load())
			m, err := query.send(server, "www.example.", dns.TypeA)
			if err != nil || m == nil || wireCookie(m) != "" {
				bad.Store(true)
			}
		})
	}
	wg.Wait()
	if bad.Load() {
		t.Fatal("concurrent authority cookie exchange failed")
	}
	jar := q.r.ex.(netExchanger).cookies
	var cookies []string
	for _, peer := range []cookiePeer{
		{netip.MustParseAddr("127.0.0.1"), server},
		{netip.MustParseAddr("127.0.0.2"), server},
		{netip.MustParseAddr("127.0.0.1"), netip.AddrPortFrom(netip.MustParseAddr("127.0.0.2"), server.Port())},
		{netip.MustParseAddr("127.0.0.1"), netip.AddrPortFrom(server.Addr(), server.Port()+1)},
	} {
		m := clientQuery("www.example.", dns.TypeA)
		jar.prepare(m, peer)
		cookies = append(cookies, wireCookie(m)[:16])
	}
	seen := map[string]bool{}
	for _, c := range cookies {
		if seen[c] {
			t.Fatal("cookie crossed local/server address or port identity")
		}
		seen[c] = true
	}
	for i := 0; i < maxCookiePeers+2; i++ {
		m := clientQuery("www.example.", dns.TypeA)
		jar.prepare(m, cookiePeer{netip.MustParseAddr("127.0.0.1"), netip.AddrPortFrom(server.Addr(), uint16(i+1))})
	}
	if len(jar.peers) > maxCookiePeers {
		t.Fatal("authority cookie cache exceeded bound")
	}
}

func TestAuthorityCookieFirstOptionAndAddressBinding(t *testing.T) {
	server, _, _ := authoritySocket(t, func(q, m *dns.Msg, _ bool) {
		c := wireCookie(q)
		if len(c) >= 16 {
			cookieReply(m, c[:16]+strings.Repeat("01", 16))
			cookieReply(m, strings.Repeat("ff", 24))
		}
	})
	q := cookieQuery(t, time.Now)
	sendCookie(t, q, server) // RFC7873 ignores options after the first.
	m := clientQuery("www.example.", dns.TypeA)
	cookieReply(m, hex.EncodeToString(make([]byte, 24)))
	original := wireCookie(m)
	_, err := q.r.ex.Exchange(t.Context(), m, server, false)
	if err != nil || wireCookie(m) != original {
		t.Fatal("transport modified caller-owned/incoming cookie")
	}
}

func TestAuthorityCookiesActualPeerAndRetiredInflight(t *testing.T) {
	var seen sync.Map
	var reused atomic.Bool
	q := cookieQuery(t, time.Now)
	for range 2 {
		server, _, _ := authoritySocket(t, func(q, m *dns.Msg, _ bool) {
			c := wireCookie(q)
			if len(c) != 16 {
				reused.Store(true)
				return
			}
			if _, loaded := seen.LoadOrStore(c, true); loaded {
				reused.Store(true)
			}
			cookieReply(m, c+strings.Repeat("01", 16))
		})
		sendCookie(t, q, server)
	}
	if reused.Load() {
		t.Fatal("server cookie reused across actual authority endpoints")
	}
	jar := q.r.ex.(netExchanger).cookies
	jar.mu.Lock()
	for peer := range jar.peers {
		if !peer.local.IsLoopback() || !peer.server.Addr().IsLoopback() {
			t.Error("cookie did not use actual connected addressing")
		}
	}
	jar.mu.Unlock()
	// A late authenticated response may finish for its original request, but
	// must not overwrite a rotated entry with retired transport credentials.
	peer := cookiePeer{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddrPort("127.0.0.1:53")}
	old := clientQuery("www.example.", dns.TypeA)
	ticket := jar.prepare(old, peer)
	jar.mu.Lock()
	jar.peers[peer] = freshCookie(time.Now())
	jar.mu.Unlock()
	reply := new(dns.Msg)
	reply.SetReply(old)
	cookieReply(reply, wireCookie(old)+strings.Repeat("01", 16))
	if jar.accept(reply, ticket) != nil {
		t.Fatal("retired in-flight response was not checked against original cookie")
	}
	jar.mu.Lock()
	defer jar.mu.Unlock()
	if len(jar.peers[peer].server) != 0 {
		t.Fatal("retired response repopulated current cookie authority")
	}
}

func TestAuthorityCookiesPreserveRecursiveValidation(t *testing.T) {
	root := newZone(".", true).serveNS("ns.root.", "127.0.0.1").add(
		"good.example. 60 IN A 192.0.2.1", "www.broken.example. 60 IN A 192.0.2.2")
	root.genKeys(t)
	root.finalize(t)
	for _, rr := range root.sigs["www.broken.example."][dns.TypeA] {
		rr.(*dns.RRSIG).Expiration = 1
	}
	var forwarded atomic.Bool
	server, udp, tcp := authoritySocket(t, func(q, m *dns.Msg, isTCP bool) {
		c := wireCookie(q)
		if len(c) < 16 || c == strings.Repeat("ab", 8) {
			forwarded.Store(true)
		}
		answer := root.answer(canon(q.Question[0].Name), q.Question[0].Qtype, q.IsEdns0() != nil && q.IsEdns0().Do()).Copy()
		answer.Id, answer.Response, answer.Question = q.Id, true, append([]dns.Question(nil), q.Question...)
		*m = *answer
		if q.Question[0].Qtype == dns.TypeDNSKEY && !isTCP {
			m.Truncated = true
			m.Answer = nil
			m.Ns = nil
		}
		if len(c) >= 16 {
			cookieReply(m, c[:16]+strings.Repeat("01", 16))
		}
	})
	cfg := DefaultConfig()
	cfg.IPv6, cfg.QNAMEMinimization = IPv6Off, false
	cfg.RootHints = []string{server.String()}
	cfg.TrustAnchors = []string{root.ksk.ToDS(dns.SHA256).String()}
	expires := time.Now().Add(time.Minute)
	cfg.NegativeTrustAnchors = []dnssec.NTA{{Domain: "broken.example.", Reason: "incident", ExpiresAt: expires}}
	r, err := New(cfg, Options{AllowLoopback: true})
	if err != nil {
		t.Fatal("native cookie resolver construction failed")
	}
	for _, tc := range []struct {
		name  string
		state Security
	}{{"good.example.", SecSecure}, {"www.broken.example.", SecInsecure}} {
		for _, do := range []bool{true, false} {
			q := clientQuery(tc.name, dns.TypeA)
			q.IsEdns0().SetDo(do)
			q.AuthenticatedData = true
			cookieReply(q, strings.Repeat("ab", 8))
			m, v, err := r.ExchangeWithValidation(t.Context(), q)
			if err != nil || m == nil || m.Rcode != dns.RcodeSuccess || v.State != tc.state || m.AuthenticatedData != (tc.state == SecSecure) || wireCookie(m) != "" {
				t.Fatal("outbound cookies changed native validation or client response")
			}
			if tc.state == SecInsecure && do && len(v.Excepted) != 1 {
				t.Fatal("cookie negotiation lost exact native NTA provenance")
			}
			for _, rr := range m.Answer {
				if !do && rr.Header().Rrtype == dns.TypeRRSIG {
					t.Fatal("cookie exchange leaked proof to non-DO client")
				}
			}
		}
	}
	if udp.Load() == 0 || tcp.Load() == 0 || forwarded.Load() {
		t.Fatal("actual recursive cookie transport or incoming-cookie isolation was not exercised")
	}
}

func TestAuthorityCookieRetryStateSurvivesCaseFallback(t *testing.T) {
	var sequence atomic.Int64
	server, udp, tcp := authoritySocket(t, func(q, m *dns.Msg, isTCP bool) {
		n := sequence.Add(1)
		c := wireCookie(q)
		if len(c) < 16 {
			return
		}
		cookieReply(m, c[:16]+strings.Repeat("01", 16))
		if !isTCP {
			m.Rcode = dns.RcodeBadCookie
			m.Answer = nil
		}
		if n == 2 {
			name := []byte(m.Question[0].Name)
			if name[0] >= 'a' && name[0] <= 'z' {
				name[0] -= 'a' - 'A'
			} else {
				name[0] += 'a' - 'A'
			}
			m.Question[0].Name = string(name)
		}
	})
	q := cookieQuery(t, time.Now)
	budget := q.st.budget
	sendCookie(t, q, server)
	if udp.Load() != 3 || tcp.Load() != 1 || q.st.budget != budget-4 {
		t.Fatalf("case fallback reset cookie retry/work allowance: udp=%d tcp=%d", udp.Load(), tcp.Load())
	}
}
