package main

import (
	"context"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

// The native resolver deliberately refuses loopback authorities. Use a real
// local non-loopback interface and explicit private-tree hints, without relaxing
// that production boundary or substituting a fake resolver/exchange function.
func anchorRecursiveAddress(t *testing.T) netip.Addr {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		prefix, err := netip.ParsePrefix(address.String())
		if err == nil {
			ip := prefix.Addr().Unmap()
			if ip.Is4() && ip.IsGlobalUnicast() && !ip.IsLoopback() {
				return ip
			}
		}
	}
	t.Fatal("managed recursive acceptance requires a local non-loopback IPv4 interface")
	return netip.Addr{}
}

func anchorRecursiveServer(t *testing.T) (string, []string, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	ip := anchorRecursiveAddress(t)
	listener, err := net.Listen("tcp4", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	packet, err := net.ListenPacket("udp4", listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	seed, signed := anchorWireSignedReply(t)
	var tcpKeys, udpKeys atomic.Int32
	for _, network := range []string{"tcp", "udp"} {
		started := make(chan struct{})
		server := &dns.Server{NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
			if len(q.Question) != 1 {
				t.Error("private authority received malformed question")
				return
			}
			response := new(dns.Msg)
			response.SetReply(q)
			response.Authoritative = true
			switch q.Question[0].Qtype {
			case dns.TypeDNSKEY:
				if network == "tcp" {
					tcpKeys.Add(1)
				} else {
					udpKeys.Add(1)
				}
				raw, err := q.Pack()
				if err != nil {
					t.Error(err)
					return
				}
				if err := response.Unpack(signed(raw)); err != nil {
					t.Error(err)
					return
				}
			case dns.TypeNS:
				for _, name := range []string{"a.root.", "b.root."} {
					response.Answer = append(response.Answer, &dns.NS{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: 60}, Ns: name})
					response.Extra = append(response.Extra, &dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IP(ip.AsSlice())})
				}
			default:
				t.Error("unexpected private-root acquisition type", q.Question[0].Qtype)
				response.Rcode = dns.RcodeServerFailure
			}
			if err := w.WriteMsg(response); err != nil {
				t.Error("private authority write", err)
			}
		})}
		if network == "tcp" {
			server.Listener = listener
		} else {
			server.PacketConn = packet
		}
		done := make(chan error, 1)
		go func() { done <- server.ActivateAndServe() }()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := server.ShutdownContext(ctx); err != nil {
				t.Error(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-ctx.Done():
				t.Error("private root failed to join")
			}
		})
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("private root failed to start")
		}
	}
	return listener.Addr().String(), seed.DS, &tcpKeys, &udpKeys
}

func TestRunServeDirectAnchorsActualRecursiveDNSKEYAndRestart(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0 // require the native resolver path, not outer raw hits
	endpoint, seeds, tcpKeys, udpKeys := anchorRecursiveServer(t)
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Mode: "recursive", Strategy: "parallel_best"}}
	cfg.Upstreams.Recursion.ManagedTopology = "direct"
	cfg.Upstreams.Recursion.AnchorMode = config.RootAnchorsManaged
	cfg.Upstreams.Recursion.DNSSEC = true
	cfg.Upstreams.Recursion.TrustAnchors = seeds
	cfg.Upstreams.Recursion.RootHints = []string{endpoint}
	cfg.Upstreams.Recursion.IPv6 = "off"
	files, roots := tsigMountedCertificate(t)
	tsigMountedTLSListeners(t, cfg, files)
	var original accepted.Version
	var priorUDP int32
	for phase := range 2 {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- runServe(ctx, cfg, "") }()
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error("managed recursive shutdown", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("managed recursive runtime failed to join")
			}
		}
		t.Cleanup(stop)
		deadline := time.NewTimer(10 * time.Second)
		for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != 200 {
			select {
			case err := <-done:
				stopped = true
				cancel()
				t.Fatal("managed recursive startup", err)
			case <-deadline.C:
				t.Fatal("managed recursive runtime did not become ready")
			case <-time.After(20 * time.Millisecond):
			}
		}
		deadline.Stop()
		for _, protocol := range []string{"udp", "tcp", "dot", "doh", "doq", "doh3"} {
			q := new(dns.Msg)
			q.SetQuestion(".", dns.TypeDNSKEY)
			q.SetEdns0(1232, true)
			q.Id = 0
			raw, err := q.Pack()
			if err != nil {
				t.Fatal(err)
			}
			answer := new(dns.Msg)
			if err := answer.Unpack(tsigMountedExchange(t, cfg, roots, protocol, raw)); err != nil {
				t.Fatal(err)
			}
			if answer.Rcode != dns.RcodeSuccess || !answer.AuthenticatedData || len(answer.Answer) != 2 {
				t.Fatal("actual managed recursive proof/output failed", phase, protocol, answer.Rcode, answer.AuthenticatedData, len(answer.Answer))
			}
			var key *dns.DNSKEY
			var sig *dns.RRSIG
			for _, rr := range answer.Answer {
				switch value := rr.(type) {
				case *dns.DNSKEY:
					key = value
				case *dns.RRSIG:
					sig = value
				}
			}
			if key == nil || sig == nil || sig.Verify(key, []dns.RR{key}) != nil {
				t.Fatal("native response lost actual signed DNSKEY proof")
			}
			ds := key.ToDS(dns.SHA256)
			ds.Hdr.Ttl = 60
			if ds.String() != seeds[0] {
				t.Fatal("native resolver used a different root key")
			}
		}
		stop()
		if tcpKeys.Load() != 1 || udpKeys.Load() <= priorUDP {
			t.Fatal("bootstrap replayed or restart avoided fresh native authority", phase, tcpKeys.Load(), udpKeys.Load())
		}
		priorUDP = udpKeys.Load()
		st, closeStore := tsigMountedStore(t, cfg)
		image, err := st.ReadGeneration(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		owner, err := accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
		if err != nil {
			t.Fatal(err)
		}
		if owner.Inspect().Phase != accepted.Managed {
			t.Fatal("recursive output omitted accepted managed history")
		}
		if phase == 0 {
			original = owner.Version()
		} else if owner.Version() != original {
			t.Fatal("restart or native queries invented accepted progress")
		}
		closeStore()
	}
}
