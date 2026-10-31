package dnsserver

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/internal/testcert"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// Observe successfully decoded DNS on each bound socket, after real TLS and
// before the normal shared handler. Instrumentation is installed before Run.
func runDoTSockets(tb testing.TB, sockets int, limits Limits) (*Registry, *testcert.Pair, []atomic.Int64, func()) {
	tb.Helper()
	pair, err := testcert.Write(tb.TempDir(), "")
	if err != nil {
		tb.Fatal(err)
	}
	reg, err := NewRegistry(Options{Handler: &Handler{Chain: pipeline.MustNew(localAnswer{}), Logger: quietLogger}, Logger: quietLogger, Sockets: sockets, Limits: limits})
	if err != nil {
		tb.Fatal(err)
	}
	if err := reg.Add(Spec{Protocol: ProtoDoT, Address: "127.0.0.1:0", CertFile: pair.CertFile, KeyFile: pair.KeyFile}); err != nil {
		tb.Fatal(err)
	}
	if err := reg.Bind(); err != nil {
		tb.Fatal(err)
	}
	listener := reg.Lookup(ProtoDoT)[0].(*dnsListener)
	counts := make([]atomic.Int64, len(listener.srvs))
	for i, srv := range listener.srvs {
		srv.Handler = dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
			counts[i].Add(1)
			listener.ServeDNS(w, q)
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reg.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					tb.Error("DoT registry shutdown failed", err)
				}
			case <-time.After(5 * time.Second):
				tb.Error("DoT registry shutdown did not join")
			}
		})
	}
	tb.Cleanup(stop)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ready, _ := reg.Ready(); ready {
			break
		}
		if time.Now().After(deadline) {
			tb.Fatal("DoT socket group never ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got, want := listener.Sockets(), SocketCount(sockets); got != want {
		tb.Fatalf("DoT bound %d sockets, want %d", got, want)
	}
	return reg, pair, counts, stop
}

func dotSocketClient(pair *testcert.Pair) *dns.Client {
	return &dns.Client{Net: "tcp-tls", Timeout: 2 * time.Second, TLSConfig: &tls.Config{RootCAs: pair.Pool, ServerName: "localhost", MinVersion: tls.VersionTLS12, NextProtos: []string{"dot"}}}
}

func TestDoTMultiSocketTLSLimitsRotationAndShutdown(t *testing.T) {
	cfg, err := config.Load("", []string{"KUDURRU_LISTENERS__SOCKETS=4", "KUDURRU_LISTENERS__LIMITS__MAXTCPQUERIES=2", "KUDURRU_LISTENERS__LIMITS__IDLETIMEOUT=150ms"})
	if err != nil {
		t.Fatal(err)
	}
	limits := LimitsFromConfig(cfg.Listeners)
	limits.ReadTimeout, limits.ShutdownTimeout = time.Second, time.Second
	reg, pair, counts, stop := runDoTSockets(t, cfg.Listeners.Sockets, limits)
	addr := reg.Addr(ProtoDoT).String()
	exercise := func(cert *testcert.Pair) {
		t.Helper()
		for i := range counts {
			counts[i].Store(0)
		}
		var wg sync.WaitGroup
		for client := range 96 {
			wg.Go(func() {
				c := dotSocketClient(cert)
				co, err := c.Dial(addr)
				if err != nil {
					t.Error("DoT TLS dial failed", err)
					return
				}
				defer func() { _ = co.Close() }()
				state := co.Conn.(*tls.Conn).ConnectionState()
				if state.NegotiatedProtocol != "dot" || len(state.VerifiedChains) == 0 || state.PeerCertificates[0].SerialNumber.Cmp(cert.Cert.SerialNumber) != 0 {
					t.Error("DoT socket did not use verified current TLS identity/ALPN")
					return
				}
				for i := range 2 {
					response, _, err := c.ExchangeWithConn(query(fmt.Sprintf("c%d-q%d.example.", client, i)), co)
					if err != nil {
						t.Error("DoT query before per-connection limit failed", err)
						return
					}
					checkA(t, response)
				}
				if _, _, err := c.ExchangeWithConn(query("over-limit.example."), co); err == nil {
					t.Error("DoT socket exceeded MaxTCPQueries")
				}
			})
		}
		wg.Wait()
		for i := range counts {
			if counts[i].Load() == 0 {
				t.Errorf("DoT socket %d never answered an actual TLS query", i)
			}
		}
	}
	exercise(pair)

	// Existing TLS connections retain their negotiated identity across rotation;
	// every socket's next handshake must use the same reloaded certificate.
	oldClient := dotSocketClient(pair)
	held, err := oldClient.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if _, _, err := oldClient.ExchangeWithConn(query("before-rotation.example."), held); err != nil {
		t.Fatal(err)
	}
	rotated, err := testcert.Write(filepath.Dir(pair.CertFile), "")
	if err != nil {
		t.Fatal(err)
	}
	reloader, err := reg.env.certs.Get(pair.CertFile, pair.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reloader.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := oldClient.ExchangeWithConn(query("after-rotation.example."), held); err != nil {
		t.Fatal("rotation broke a negotiated TLS connection", err)
	}
	exercise(rotated)

	c := dotSocketClient(rotated)
	idle, err := c.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idle.Close() }()
	if _, _, err := c.ExchangeWithConn(query("idle.example."), idle); err != nil {
		t.Fatal(err)
	}
	_ = idle.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := idle.ReadMsg(); err == nil {
		t.Fatal("idle DoT connection remained open")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("idle limit did not close DoT connection before client deadline")
	}
	for _, alpn := range []string{"h2", "doq"} {
		bad := dotSocketClient(rotated)
		bad.TLSConfig.NextProtos = []string{alpn}
		if co, err := bad.Dial(addr); err == nil {
			_ = co.Close()
			t.Error("DoT accepted incompatible ALPN", alpn)
		}
	}
	plain := &dns.Client{Net: "tcp", Timeout: 2 * time.Second}
	if _, _, err := plain.Exchange(query("plain.example."), addr); err == nil {
		t.Fatal("DoT accepted plaintext DNS")
	}
	untrusted := dotSocketClient(pair)
	if co, err := untrusted.Dial(addr); err == nil {
		_ = co.Close()
		t.Fatal("rotated DoT certificate bypassed client trust")
	}
	stop()
	if ready, _ := reg.Ready(); ready {
		t.Fatal("stopped DoT group remains ready")
	}
	// A plain bind must succeed, proving all reuse-port sockets were closed.
	probe, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal("DoT shutdown retained a bound socket", err)
	}
	_ = probe.Close()
}

// BenchmarkDoTSockets measures real verified TLS with stable connections and
// connection churn separately. q/s includes every client-observed DNS reply;
// stable connections amortize the initial handshake. No session resumption.
func BenchmarkDoTSockets(b *testing.B) {
	counts := []int{1}
	if n := SocketCount(0); n > 1 {
		counts = append(counts, n)
	}
	for _, sockets := range counts {
		for _, churn := range []bool{false, true} {
			b.Run(fmt.Sprintf("sockets=%d/churn=%t", sockets, churn), func(b *testing.B) {
				pair, err := testcert.Write(b.TempDir(), "")
				if err != nil {
					b.Fatal(err)
				}
				reg := runSocketRegistry(b, sockets, nil, Spec{Protocol: ProtoDoT, Address: "127.0.0.1:0", CertFile: pair.CertFile, KeyFile: pair.KeyFile})
				addr := reg.Addr(ProtoDoT).String()
				var answered, failed atomic.Int64
				b.SetParallelism(4)
				b.ReportAllocs()
				b.ResetTimer()
				start := time.Now()
				b.RunParallel(func(pb *testing.PB) {
					c := dotSocketClient(pair)
					var co *dns.Conn
					defer func() {
						if co != nil {
							_ = co.Close()
						}
					}()
					q := query("bench.example.")
					for pb.Next() {
						if co == nil {
							var dialErr error
							co, dialErr = c.Dial(addr)
							if dialErr != nil {
								failed.Add(1)
								continue
							}
						}
						if response, _, e := c.ExchangeWithConn(q, co); e != nil || response == nil || response.Rcode != dns.RcodeSuccess {
							failed.Add(1)
						} else {
							answered.Add(1)
						}
						if churn {
							_ = co.Close()
							co = nil
						}
					}
				})
				b.ReportMetric(float64(answered.Load())/time.Since(start).Seconds(), "q/s")
				b.ReportMetric(float64(failed.Load()), "failed")
				if failed.Load() != 0 {
					b.Fatal("DoT benchmark lost replies")
				}
			})
		}
	}
}
