package dnsserver

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/internal/testcert"
)

// Public state/address readers run independently of the connection event loop.
// Retain one actual TLS connection while its sender moves to three new sockets.
func TestQUICConnectionStateDuringMigration(t *testing.T) {
	for _, proto := range []Protocol{ProtoDoQ, ProtoDoH3} {
		for _, sockets := range []int{1, 3} {
			t.Run(fmt.Sprintf("%s/sockets=%d", proto, sockets), func(t *testing.T) {
				pair, err := testcert.Write(t.TempDir(), "")
				if err != nil {
					t.Fatal(err)
				}
				reg := runSocketRegistry(t, sockets, nil, Spec{Protocol: proto, Address: "127.0.0.1:0", CertFile: pair.CertFile, KeyFile: pair.KeyFile})
				address := reg.Addr(proto).String()
				remote, err := net.ResolveUDPAddr("udp", address)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				client := newQUICSocketClient(t)
				conn, err := client.Dial(ctx, remote, (&tlsPool{pair}).client(proto.ALPN()...), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer closeQUIC(conn)
				stop, done, ready := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var snapshots atomic.Int64
				var invalid atomic.Bool
				go func() {
					defer close(done)
					close(ready)
					for {
						select {
						case <-stop:
							return
						case <-ctx.Done():
							return
						default:
						}
						state := conn.ConnectionState()
						if state.TLS.Version != tls.VersionTLS13 || conn.LocalAddr() == nil || conn.RemoteAddr() == nil {
							invalid.Store(true)
						}
						snapshots.Add(1)
						runtime.Gosched()
					}
				}()
				var once sync.Once
				join := func() { once.Do(func() { close(stop); <-done }) }
				defer join()
				<-ready
				exchange := quicSocketExchange(proto, address, conn)
				var previous *quic.Path
				for path := range 4 {
					if path > 0 {
						next, err := conn.AddPath(newQUICSocketClient(t))
						if err != nil {
							t.Fatal(err)
						}
						if err := next.Probe(ctx); err != nil {
							t.Fatal("path validation", err)
						}
						if err := next.Switch(); err != nil {
							t.Fatal(err)
						}
						if previous != nil {
							if err := previous.Close(); err != nil {
								t.Fatal(err)
							}
						}
						previous = next
					}
					for range 4 {
						question := query("migration-state.example.")
						question.Id = 0
						answer, err := exchange(ctx, question)
						if err != nil {
							t.Fatal("original connection query", err)
						}
						checkA(t, answer)
					}
				}
				join()
				if snapshots.Load() == 0 || invalid.Load() {
					t.Fatal("concurrent state/address observations invalid", snapshots.Load())
				}
			})
		}
	}
}
