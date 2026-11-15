package upstream

import (
	"bytes"
	"crypto"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/recursor"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

// Use real signed UDP answers, then delay only the consumer's observation.
// A still-positive relative TTL must not renew the original absolute proof.
func TestRecursiveAggressiveDeadlineSurvivesDelayedCacheAndRestore(t *testing.T) {
	start := time.Now()
	addr, anchor, requests := denialDeadlineRoot(t, start)
	cfg := recursor.DefaultConfig()
	cfg.IPv6, cfg.QNAMEMinimization = recursor.IPv6Off, false
	cfg.RootHints, cfg.TrustAnchors = []string{addr}, []string{anchor}
	rec, err := recursor.New(cfg, recursor.Options{AllowLoopback: true, Now: func() time.Time { return start }})
	if err != nil {
		t.Fatal(err)
	}
	set, err := upstream.New(upstream.Config{Groups: map[string]upstream.GroupConfig{"default": {Recursive: true}}, Recursion: cfg}, upstream.Options{Recursor: rec})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close() })
	q := new(dns.Msg)
	q.SetQuestion("one.example.", dns.TypeA)
	q.SetEdns0(1232, true)
	plan, err := set.Plan(&pipeline.Request{Ctx: t.Context(), Msg: q}, nil, false, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Exchange(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	before := requests.Load()
	q.SetQuestion("two.example.", dns.TypeA)
	res, err := plan.Exchange(t.Context(), q)
	if err != nil || res.DNSSEC != recursor.SecSecure || res.Msg.Rcode != dns.RcodeNameError || requests.Load() != before {
		t.Fatal("actual signed proof did not synthesize a distinct name")
	}
	deadline := start.Add(2 * time.Second)
	if !res.NativeValidation.ValidUntil.Equal(deadline) {
		t.Fatal("native result lost original absolute deadline")
	}
	// The consumer runs a second later; now+TTL would be after deadline.
	now := start.Add(time.Second)
	security := recursiveSecurity(res, plan, false, now)
	if !security.ValidUntil.Equal(deadline) {
		t.Fatal("delayed consumer renewed proof")
	}
	raw := cache.New(cache.Options{MaxEntries: 32, StaleTTL: time.Hour, Now: func() time.Time { return now }})
	key := cache.MakeScopedKey(plan.Scope(), "two.example.", dns.TypeA, dns.ClassINET, true, false, netip.Prefix{})
	wire, err := res.Msg.Pack()
	if err != nil {
		t.Fatal(err)
	}
	metadata := cache.Metadata{Scope: plan.Scope(), Security: security}
	// Deliberately request much longer freshness and stale windows.
	raw.SetWithMetadata(key, wire, cache.Negative, time.Hour, metadata)
	if _, ok := raw.Get(key); !ok {
		t.Fatal("live proof was not cached")
	}
	var dump bytes.Buffer
	if err := raw.Dump(&dump); err != nil {
		t.Fatal(err)
	}
	now = deadline
	if _, ok := raw.Get(key); ok {
		t.Fatal("outer cache extended proof with minimum TTL or stale policy")
	}
	restored := cache.New(cache.Options{MaxEntries: 32, StaleTTL: time.Hour, Now: func() time.Time { return now }})
	if n, err := restored.Load(&dump); err != nil || n != 0 {
		t.Fatal("cache restore renewed expired proof")
	}
	late := recursiveSecurity(res, plan, false, now)
	if late.AvailableAt(now) {
		t.Fatal("late handoff was granted a fresh deadline")
	}
	restored.SetWithMetadata(key, wire, cache.Negative, time.Hour, cache.Metadata{Scope: plan.Scope(), Security: late})
	if restored.Len() != 0 {
		t.Fatal("late proof filled cache")
	}
}

func denialDeadlineRoot(t *testing.T, start time.Time) (string, string, *atomic.Int64) {
	t.Helper()
	key := &dns.DNSKEY{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 300}, Flags: 257, Protocol: 3, Algorithm: dns.ECDSAP256SHA256}
	var signer crypto.Signer
	for signer == nil || key.KeyTag() == 0 {
		private, err := key.Generate(256)
		if err != nil {
			t.Fatal(err)
		}
		signer = private.(crypto.Signer)
	}
	sign := func(rr dns.RR) []dns.RR {
		sig := &dns.RRSIG{Hdr: dns.RR_Header{Ttl: rr.Header().Ttl}, Algorithm: key.Algorithm, KeyTag: key.KeyTag(), SignerName: ".",
			Inception: uint32(start.Add(-time.Hour).Unix()), Expiration: uint32(start.Add(time.Hour).Unix())} //nolint:gosec // present-day test signature window
		if err := sig.Sign(signer, []dns.RR{rr}); err != nil {
			t.Fatal(err)
		}
		return []dns.RR{rr, sig}
	}
	soa, _ := dns.NewRR(". 2 IN SOA ns. hostmaster. 1 60 10 3600 2")
	nsec, _ := dns.NewRR(". 2 IN NSEC . NS SOA RRSIG NSEC DNSKEY")
	ns, _ := dns.NewRR(". 300 IN NS ns.")
	proof := append(sign(soa), sign(nsec)...)
	keys, servers := sign(key), sign(ns)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	requests := &atomic.Int64{}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		requests.Add(1)
		m := new(dns.Msg)
		m.SetReply(q)
		m.Authoritative = true
		switch q.Question[0].Qtype {
		case dns.TypeDNSKEY:
			m.Answer = keys
		case dns.TypeNS:
			m.Answer = servers
		default:
			m.Rcode, m.Ns = dns.RcodeNameError, proof
		}
		_ = w.WriteMsg(m)
	})}
	started := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(started) }
	go func() { _ = srv.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = srv.Shutdown() })
	return pc.LocalAddr().String(), key.ToDS(dns.SHA256).String(), requests
}
