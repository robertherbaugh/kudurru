package dnsserver

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/robertherbaugh/kudurru/internal/dnsserver/internal/testcert"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

type socketIndexKey struct{}

func TestQUICSocketHandshakeEveryShardAndRetry(t *testing.T) {
	pair, err := testcert.Write(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, err := tls.LoadX509KeyPair(pair.CertFile, pair.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []quic.Version{quic.Version1, quic.Version2} {
		for _, retry := range []bool{false, true} {
			t.Run(fmt.Sprintf("version=%d/retry=%t", version, retry), func(t *testing.T) {
				count, next := SocketCount(3), 0
				g, err := listenQUICSockets("127.0.0.1:0", count, func(tr *quic.Transport) (quicAcceptor, error) {
					index := next
					next++
					tr.ConnContext = func(ctx context.Context, _ *quic.ClientInfo) (context.Context, error) {
						return context.WithValue(ctx, socketIndexKey{}, index), nil
					}
					tr.VerifySourceAddress = func(net.Addr) bool { return retry }
					return tr.Listen(&tls.Config{Certificates: []tls.Certificate{serverTLS}, NextProtos: []string{"doq"}, MinVersion: tls.VersionTLS13}, &quic.Config{Versions: []quic.Version{version}})
				})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = g.closeSockets() }()
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				seen := map[int]bool{}
				// Each is a distinct client's fresh random Initial CID, never a
				// failed handshake retry. Bound attempts to visit all shards.
				for attempt := 0; attempt < 128 && len(seen) < count; attempt++ {
					c, err := quic.DialAddr(ctx, g.Addr().String(), (&tlsPool{pair}).client("doq"), &quic.Config{Versions: []quic.Version{version}})
					if err != nil {
						t.Fatal(err)
					}
					s, err := g.Accept(ctx)
					if err != nil {
						closeQUIC(c)
						t.Fatal(err)
					}
					index := s.Context().Value(socketIndexKey{}).(int)
					seen[index] = true
					closeQUIC(c)
					closeQUIC(s)
				}
				if len(seen) != count {
					t.Fatal("not every socket completed a verified handshake", seen)
				}
			})
		}
	}
}

// This uses one original QUIC connection for all requests, moving it through
// actual UDP transports (new source ports), with no redial or HTTP retry path.
func TestQUICSocketDNSAcrossMigration(t *testing.T) {
	for _, proto := range []Protocol{ProtoDoQ, ProtoDoH3} {
		for _, count := range []int{1, 3} {
			t.Run(fmt.Sprintf("%s/sockets=%d", proto, count), func(t *testing.T) {
				pair, err := testcert.Write(t.TempDir(), "")
				if err != nil {
					t.Fatal(err)
				}
				reg := runSocketRegistry(t, count, nil, Spec{Protocol: proto, Address: "127.0.0.1:0", CertFile: pair.CertFile, KeyFile: pair.KeyFile})
				if got := reg.Lookup(proto)[0].(interface{ Sockets() int }).Sockets(); got != SocketCount(count) {
					t.Fatal("wrong socket count", got)
				}
				address := reg.Addr(proto).String()
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				tr := newQUICSocketClient(t)
				remote, err := net.ResolveUDPAddr("udp", address)
				if err != nil {
					t.Fatal(err)
				}
				c, err := tr.Dial(ctx, remote, (&tlsPool{pair}).client(proto.ALPN()...), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer closeQUIC(c)
				exchange := quicSocketExchange(proto, address, c)
				var previous *quic.Path
				for path := range 4 {
					if path > 0 {
						next := newQUICSocketClient(t)
						p, err := c.AddPath(next)
						if err != nil {
							t.Fatal(err)
						}
						if err := p.Probe(ctx); err != nil {
							t.Fatal("original connection path validation failed", path, err)
						}
						if err := p.Switch(); err != nil {
							t.Fatal(err)
						}
						// Retire the old path's CID after switching. Keeping every
						// inactive path open exhausts quic-go's four active CIDs,
						// including on an ordinary single-socket listener.
						if previous != nil {
							if err := previous.Close(); err != nil {
								t.Fatal(err)
							}
						}
						previous = p
					}
					for range 4 {
						q := query("migration.example.")
						q.Id = 0
						answer, err := exchange(ctx, q)
						if err != nil {
							t.Fatal("original connection DNS failed", path, err)
						}
						checkA(t, answer)
					}
				}
			})
		}
	}
}

func newQUICSocketClient(tb testing.TB) *quic.Transport {
	tb.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	tr := &quic.Transport{Conn: pc}
	tb.Cleanup(func() { _ = tr.Close(); _ = pc.Close() })
	return tr
}

func quicSocketExchange(proto Protocol, address string, c *quic.Conn) func(context.Context, *dns.Msg) (*dns.Msg, error) {
	if proto == ProtoDoQ {
		return func(_ context.Context, q *dns.Msg) (*dns.Msg, error) { return doqExchange(c, q) }
	}
	h := (&http3.Transport{}).NewClientConn(c)
	return func(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
		wire, err := q.Pack()
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+address+DefaultDoHPath+"?dns="+base64.RawURLEncoding.EncodeToString(wire), nil)
		if err != nil {
			return nil, err
		}
		res, err := h.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = res.Body.Close() }()
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("HTTP/3 status %d", res.StatusCode)
		}
		wire, err = io.ReadAll(io.LimitReader(res.Body, 65536))
		if err != nil {
			return nil, err
		}
		answer := new(dns.Msg)
		return answer, answer.Unpack(wire)
	}
}

func TestQUICSocketFailureClosesWholeGroup(t *testing.T) {
	for _, proto := range []Protocol{ProtoDoQ, ProtoDoH3} {
		t.Run(string(proto), func(t *testing.T) {
			pair, err := testcert.Write(t.TempDir(), "")
			if err != nil {
				t.Fatal(err)
			}
			reg, err := NewRegistry(Options{Handler: &Handler{Chain: pipeline.MustNew(localAnswer{}), Logger: quietLogger}, Logger: quietLogger, Sockets: 3})
			if err != nil {
				t.Fatal(err)
			}
			if err := reg.Add(Spec{Protocol: proto, Address: "127.0.0.1:0", CertFile: pair.CertFile, KeyFile: pair.KeyFile}); err != nil {
				t.Fatal(err)
			}
			if err := reg.Bind(); err != nil {
				t.Fatal(err)
			}
			listener := reg.Lookup(proto)[0]
			defer func() { _ = listener.Close() }()
			address := reg.Addr(proto).String()
			var group *quicSockets
			if proto == ProtoDoQ {
				group = listener.(*doqListener).ln
			} else {
				group = listener.(*doh3Listener).ln
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- reg.Run(ctx) }()
			if err := group.packets[0].Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("failed receive socket did not report an error")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("one failed receive socket did not join the group")
			}
			if ready, _ := reg.Ready(); ready {
				t.Fatal("failed group still ready")
			}
			pc, err := net.ListenPacket("udp", address)
			if err != nil {
				t.Fatal("group retained a socket after failure", err)
			}
			_ = pc.Close()
		})
	}
}

func TestQUICSocketPartialBindAndBounds(t *testing.T) {
	for _, n := range []int{0, -1, 257} {
		if g, err := listenQUICSockets("127.0.0.1:0", n, nil); err == nil || g != nil {
			t.Fatal("invalid count accepted", n)
		}
	}
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.LocalAddr().String()
	_ = probe.Close()
	refusal := errors.New("test: transport initialization refused")
	if _, err := listenQUICSockets(address, SocketCount(3), func(*quic.Transport) (quicAcceptor, error) { return nil, refusal }); !errors.Is(err, refusal) {
		t.Fatal(err)
	}
	rebound, err := net.ListenPacket("udp", address)
	if err != nil {
		t.Fatal("partial bind leaked sockets", err)
	}
	_ = rebound.Close()
}

func TestQUICSocketBindConflict(t *testing.T) {
	pair, err := testcert.Write(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, proto := range []Protocol{ProtoDoQ, ProtoDoH3} {
		t.Run(string(proto), func(t *testing.T) {
			first := runSocketRegistry(t, 3, nil, Spec{Protocol: proto, Address: "127.0.0.1:0", CertFile: pair.CertFile, KeyFile: pair.KeyFile})
			second, err := NewRegistry(Options{Handler: &Handler{Chain: pipeline.MustNew(localAnswer{})}, Logger: quietLogger, Sockets: 3})
			if err != nil {
				t.Fatal(err)
			}
			if err := second.Add(Spec{Protocol: proto, Address: first.Addr(proto).String(), CertFile: pair.CertFile, KeyFile: pair.KeyFile}); err != nil {
				t.Fatal(err)
			}
			if err := second.Bind(); err == nil {
				_ = second.Listeners()[0].Close()
				t.Fatal("second registry joined an existing QUIC socket group")
			}
		})
	}
}

func TestQUICSocketConcurrentBind(t *testing.T) {
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.LocalAddr().String()
	_ = probe.Close()
	start := make(chan struct{})
	groups := make(chan []net.PacketConn, 12)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			<-start
			pcs, err := listenQUICPackets(address, SocketCount(3))
			if err == nil {
				groups <- pcs
			}
		})
	}
	close(start)
	wg.Wait()
	close(groups)
	bound := 0
	for pcs := range groups {
		bound++
		_ = closeAllFunc(pcs)()
	}
	if bound != 1 {
		t.Fatal("simultaneous startup shared a routing group", bound)
	}
}

// BenchmarkQUICSockets measures real TLS connections and DNS responses on
// loopback. It is a local comparison, not a network/fixed-class release gate.
func BenchmarkQUICSockets(b *testing.B) {
	for _, proto := range []Protocol{ProtoDoQ, ProtoDoH3} {
		for _, count := range []int{1, 4} {
			b.Run(fmt.Sprintf("%s/sockets=%d", proto, count), func(b *testing.B) {
				pair, err := testcert.Write(b.TempDir(), "")
				if err != nil {
					b.Fatal(err)
				}
				reg := runSocketRegistry(b, count, nil, Spec{Protocol: proto, Address: "127.0.0.1:0", CertFile: pair.CertFile, KeyFile: pair.KeyFile})
				address := reg.Addr(proto).String()
				const clients = 16
				exchanges := make(chan func(context.Context, *dns.Msg) (*dns.Msg, error), clients)
				for range clients {
					ctx, cancel := context.WithTimeout(b.Context(), 5*time.Second)
					c, err := quic.DialAddr(ctx, address, (&tlsPool{pair}).client(proto.ALPN()...), nil)
					cancel()
					if err != nil {
						b.Fatal(err)
					}
					b.Cleanup(func() { closeQUIC(c) })
					exchanges <- quicSocketExchange(proto, address, c)
				}
				b.ReportAllocs()
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					q := query("benchmark.example.")
					q.Id = 0
					for pb.Next() {
						exchange := <-exchanges
						_, err := exchange(b.Context(), q)
						exchanges <- exchange
						if err != nil {
							b.Error(err)
							return
						}
					}
				})
			})
		}
	}
}
