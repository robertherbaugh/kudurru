package e2e

import (
	"context"
	"crypto"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// Root modes of anchorRoot. Each mode answers the root DNSKEY set with
// signatures made when the root starts, valid for four hours, which covers
// the one-hour minimum refresh of the soak.
const (
	// anchorRootNormal: key A, signed by A.
	anchorRootNormal int32 = iota
	// anchorRootRevoked: key A with the REVOKE bit, self-signed (RFC 5011
	// section 2.1), and no other key: every trusted key is lost.
	anchorRootRevoked
	// anchorRootRecovered: key B only, signed by B (a new root key that an
	// operator installs with `kudurru anchors recover --offline`).
	anchorRootRecovered
	anchorRootModes
)

// anchorRoot is a signed stand-in for the root zone, on a local non-loopback
// IPv4 address: the native resolver refuses loopback authorities, also with
// custom root hints, and this test does not relax that. It answers the root
// DNSKEY set over TCP (managed anchor acquisitions use TCP only, and are
// counted per mode) and UDP (the resolver's own validation path, counted
// apart), and the root NS set for priming. It is not an RFC interoperability
// fixture: that evidence uses real authorities (BIND, Knot) elsewhere.
type anchorRoot struct {
	Addr     string
	SeedA    string // DS of key A, the cluster's seed
	DSB      string // DS of key B, the operator's recovery input
	mode     atomic.Int32
	tcpReads [anchorRootModes]atomic.Int32
	udpReads atomic.Int32
	answers  [anchorRootModes][]dns.RR
}

func anchorRootAddress(t *testing.T) netip.Addr {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var fallback netip.Addr
	for _, address := range addresses {
		prefix, err := netip.ParsePrefix(address.String())
		if err != nil {
			continue
		}
		ip := prefix.Addr().Unmap()
		if !ip.Is4() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
			continue
		}
		if ip.IsGlobalUnicast() {
			return ip
		}
		if !fallback.IsValid() {
			// Custom root hints also admit link-local and other internal
			// addresses (a private tree); a sandbox may have only those.
			fallback = ip
		}
	}
	if !fallback.IsValid() {
		t.Fatal("managed anchor process acceptance requires a local non-loopback IPv4 interface")
	}
	return fallback
}

func newAnchorRoot(t *testing.T) *anchorRoot {
	t.Helper()
	generate := func() (*dns.DNSKEY, crypto.Signer) {
		key := &dns.DNSKEY{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 60}, Flags: dns.ZONE | dns.SEP, Protocol: 3, Algorithm: dns.ED25519}
		private, err := key.Generate(256)
		if err != nil {
			t.Fatal(err)
		}
		return key, private.(crypto.Signer)
	}
	keyA, signerA := generate()
	keyB, signerB := generate()
	r := &anchorRoot{SeedA: keyA.ToDS(dns.SHA256).String(), DSB: keyB.ToDS(dns.SHA256).String()}
	sign := func(key *dns.DNSKEY, signer crypto.Signer) []dns.RR {
		sig := &dns.RRSIG{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: 60}, TypeCovered: dns.TypeDNSKEY, Algorithm: key.Algorithm,
			Labels: 0, OrigTtl: 60, KeyTag: key.KeyTag(), SignerName: ".",
			Inception: uint32(time.Now().Add(-time.Hour).Unix() & 0xffffffff), Expiration: uint32(time.Now().Add(4*time.Hour).Unix() & 0xffffffff)}
		if err := sig.Sign(signer, []dns.RR{key}); err != nil {
			t.Fatal(err)
		}
		return []dns.RR{key, sig}
	}
	revoked := dns.Copy(keyA).(*dns.DNSKEY)
	revoked.Flags |= dns.REVOKE
	r.answers[anchorRootNormal] = sign(keyA, signerA)
	r.answers[anchorRootRevoked] = sign(revoked, signerA)
	r.answers[anchorRootRecovered] = sign(keyB, signerB)

	ip := anchorRootAddress(t)
	listener, err := net.Listen("tcp4", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	packet, err := net.ListenPacket("udp4", listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	r.Addr = listener.Addr().String()
	var wg sync.WaitGroup
	// Cleanups run last-registered first: join the servers after the
	// shutdowns registered below.
	t.Cleanup(wg.Wait)
	for _, network := range []string{"tcp", "udp"} {
		started := make(chan struct{})
		server := &dns.Server{NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
			response := new(dns.Msg)
			response.SetReply(q)
			response.Authoritative = true
			if len(q.Question) != 1 || q.Question[0].Name != "." {
				response.Rcode = dns.RcodeRefused
				_ = w.WriteMsg(response)
				return
			}
			switch q.Question[0].Qtype {
			case dns.TypeDNSKEY:
				mode := r.mode.Load()
				if network == "tcp" {
					r.tcpReads[mode].Add(1)
				} else {
					r.udpReads.Add(1)
				}
				for _, rr := range r.answers[mode] {
					response.Answer = append(response.Answer, dns.Copy(rr))
				}
				if o := q.IsEdns0(); o != nil {
					response.SetEdns0(1232, o.Do())
				}
			case dns.TypeNS:
				for _, name := range []string{"a.root.", "b.root."} {
					response.Answer = append(response.Answer, &dns.NS{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: 60}, Ns: name})
					response.Extra = append(response.Extra, &dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IP(ip.AsSlice())})
				}
			default:
				response.Rcode = dns.RcodeRefused
			}
			_ = w.WriteMsg(response)
		})}
		if network == "tcp" {
			server.Listener = listener
		} else {
			server.PacketConn = packet
		}
		wg.Add(1)
		go func() { defer wg.Done(); _ = server.ActivateAndServe() }()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.ShutdownContext(ctx)
		})
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("stand-in root failed to start")
		}
	}
	return r
}

// TCP returns the number of managed anchor acquisitions answered in mode.
func (r *anchorRoot) TCP(mode int32) int32 { return r.tcpReads[mode].Load() }

// TotalTCP returns every managed anchor acquisition in all modes.
func (r *anchorRoot) TotalTCP() int32 {
	var n int32
	for mode := range anchorRootModes {
		n += r.tcpReads[mode].Load()
	}
	return n
}

// Set switches the DNSKEY set every later query receives.
func (r *anchorRoot) Set(mode int32) { r.mode.Store(mode) }

// anchorNodeConfig adds managed root anchors with direct topology to one
// node's configuration. anchorMode, the seeds and the hints are cluster
// settings: the primary's file seeds them, members take them from its image,
// and setting them on every node changes nothing. managedTopology is host
// only, so every member declares it.
func anchorNodeConfig(cfg map[string]any, r *anchorRoot) {
	cfg["upstreams"] = map[string]any{
		"groups": map[string]any{"default": map[string]any{"mode": "recursive", "strategy": "parallel_best"}},
		"recursion": map[string]any{"anchorMode": "managed", "managedTopology": "direct", "dnssec": true,
			"trustAnchors": []any{r.SeedA}, "rootHints": []any{r.Addr}, "ipv6": "off"},
	}
	// Every validated answer goes to the stand-in root, never to a cache.
	cfg["cache"] = map[string]any{"maxEntries": 0}
}

// anchorValidated asks one node, over UDP and TCP, for the root DNSKEY set
// through its recursive group, and reports whether every answer is NOERROR
// with the AD bit and the key with its signature: validated from the
// node's accepted managed trust.
func anchorValidated(node string) bool {
	for _, network := range []string{"udp", "tcp"} {
		q := new(dns.Msg)
		q.SetQuestion(".", dns.TypeDNSKEY)
		q.RecursionDesired = true
		q.SetEdns0(1232, true)
		c := &dns.Client{Net: network, Timeout: 3 * time.Second}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		answer, _, err := c.ExchangeContext(ctx, q, node)
		cancel()
		if err != nil || answer.Rcode != dns.RcodeSuccess || !answer.AuthenticatedData || len(answer.Answer) != 2 {
			return false
		}
	}
	return true
}

// anchorServfail reports that one node answers SERVFAIL over UDP and TCP to
// both a validated recursive question and an authoritative one, and emits no
// data or proof: its output is retired or it holds no trusted key.
func anchorServfail(node, authoritative string) bool {
	for _, network := range []string{"udp", "tcp"} {
		for _, question := range []dns.Question{{Name: ".", Qtype: dns.TypeDNSKEY, Qclass: dns.ClassINET}, {Name: authoritative, Qtype: dns.TypeA, Qclass: dns.ClassINET}} {
			q := new(dns.Msg)
			q.SetQuestion(question.Name, question.Qtype)
			q.RecursionDesired = true
			q.SetEdns0(1232, true)
			c := &dns.Client{Net: network, Timeout: 3 * time.Second}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			answer, _, err := c.ExchangeContext(ctx, q, node)
			cancel()
			if err != nil || answer.Rcode != dns.RcodeServerFailure || len(answer.Answer) != 0 || answer.AuthenticatedData {
				return false
			}
		}
	}
	return true
}
