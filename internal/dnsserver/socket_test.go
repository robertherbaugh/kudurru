package dnsserver

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/dnsserver/internal/testcert"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// runSocketRegistry serves specs on 127.0.0.1 ephemeral ports with
// Options.Sockets = sockets and the given stages (localAnswer by default),
// and stops it when the test ends.
func runSocketRegistry(tb testing.TB, sockets int, stages []pipeline.Stage, specs ...Spec) *Registry {
	tb.Helper()
	if stages == nil {
		stages = []pipeline.Stage{localAnswer{}}
	}
	reg, err := NewRegistry(Options{
		Handler: &Handler{Chain: pipeline.MustNew(stages...), Logger: quietLogger},
		Logger:  quietLogger,
		Sockets: sockets,
	})
	if err != nil {
		tb.Fatal(err)
	}
	if err := reg.Add(specs...); err != nil {
		tb.Fatal(err)
	}
	if err := reg.Bind(); err != nil {
		tb.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reg.Run(ctx) }()
	tb.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				tb.Errorf("registry Run: %v", err)
			}
		case <-time.After(10 * time.Second):
			tb.Error("registry did not shut down")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok, _ := reg.Ready(); ok {
			return reg
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, bad := reg.Ready()
	tb.Fatalf("listeners not ready: %v", bad)
	return nil
}

func TestSocketCount(t *testing.T) {
	procs := runtime.GOMAXPROCS(0)
	tests := []struct {
		in, linux int
	}{
		{0, procs},
		{-3, procs},
		{1, 1},
		{4, 4},
		{64, 64},
	}
	for _, tt := range tests {
		want := tt.linux
		if !ReuseportSupported {
			want = 1
		}
		if got := SocketCount(tt.in); got != want {
			t.Errorf("SocketCount(%d) = %d, want %d", tt.in, got, want)
		}
	}
}

func TestRebindAddress(t *testing.T) {
	bound := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5353}
	tests := []struct{ in, want string }{
		{"127.0.0.1:0", "127.0.0.1:5353"},
		{"0.0.0.0:0", "0.0.0.0:5353"},
		{":0", ":5353"},
		{"[::1]:0", "[::1]:5353"},
		{"garbage", "127.0.0.1:5353"},
	}
	for _, tt := range tests {
		if got := rebindAddress(tt.in, bound); got != tt.want {
			t.Errorf("rebindAddress(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestMultiSocketListeners: udp and tcp listeners bind Options.Sockets
// sockets on one port (DoT keeps one), every socket answers, and queries
// from many source ports all get answers.
func TestMultiSocketListeners(t *testing.T) {
	const sockets = 4
	pair, err := testcert.Write(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	reg := runSocketRegistry(t, sockets, nil,
		Spec{Protocol: ProtoUDP, Address: "127.0.0.1:0"},
		Spec{Protocol: ProtoTCP, Address: "127.0.0.1:0"},
		Spec{Protocol: ProtoDoT, Address: "127.0.0.1:0", CertFile: pair.CertFile, KeyFile: pair.KeyFile},
	)
	want := sockets
	if !ReuseportSupported {
		want = 1
	}
	for _, p := range []Protocol{ProtoUDP, ProtoTCP, ProtoDoT} {
		l := reg.Lookup(p)[0].(*dnsListener)
		w := want
		if p == ProtoDoT {
			w = 1
		}
		if got := l.Sockets(); got != w {
			t.Errorf("%s: %d sockets, want %d", p, got, w)
		}
	}
	for _, p := range []Protocol{ProtoUDP, ProtoTCP} {
		addr := reg.Addr(p).String()
		var wg sync.WaitGroup
		var answered atomic.Int64
		const clients = 32
		for i := range clients {
			wg.Go(func() {
				c := &dns.Client{Net: p.Network(), Timeout: 2 * time.Second}
				resp, _, err := c.Exchange(query(fmt.Sprintf("c%d.example.", i)), addr)
				if err != nil {
					t.Errorf("%s client %d: %v", p, i, err)
					return
				}
				checkA(t, resp)
				answered.Add(1)
			})
		}
		wg.Wait()
		if answered.Load() != clients {
			t.Fatalf("%s: %d of %d answered", p, answered.Load(), clients)
		}
	}
}

// TestMultiSocketBindConflict: SO_REUSEPORT must not let a second process
// (or registry) silently share a port that is already serving; the plain
// probe bind fails first, as a single-socket bind would.
func TestMultiSocketBindConflict(t *testing.T) {
	first := runSocketRegistry(t, 4, nil,
		Spec{Protocol: ProtoUDP, Address: "127.0.0.1:0"},
		Spec{Protocol: ProtoTCP, Address: "127.0.0.1:0"},
	)
	for _, p := range []Protocol{ProtoUDP, ProtoTCP} {
		taken := first.Addr(p).String()
		second, err := NewRegistry(Options{Handler: &Handler{Chain: pipeline.MustNew(localAnswer{})}, Logger: quietLogger, Sockets: 4})
		if err != nil {
			t.Fatal(err)
		}
		if err := second.Add(Spec{Protocol: p, Address: taken}); err != nil {
			t.Fatal(err)
		}
		err = second.Bind()
		if err == nil || !strings.Contains(err.Error(), "address already in use") {
			_ = second.Listeners()[0].Close()
			t.Fatalf("%s: second multi-socket bind on %s: err = %v, want address already in use", p, taken, err)
		}
	}
}

// dropStage asks the transport to send nothing for every query.
type dropStage struct{ seen atomic.Int64 }

func (*dropStage) Name() string { return "local" }

func (d *dropStage) Serve(*pipeline.Request, pipeline.Next) (*pipeline.Response, error) {
	d.seen.Add(1)
	return &pipeline.Response{Status: pipeline.StatusBlockedRateLimit}, nil
}

// TestDropResponse: a pipeline drop sends nothing over UDP and SERVFAIL
// over TCP; Resolve never returns nil.
func TestDropResponse(t *testing.T) {
	d := &dropStage{}
	reg := runSocketRegistry(t, 1, []pipeline.Stage{d},
		Spec{Protocol: ProtoUDP, Address: "127.0.0.1:0"},
		Spec{Protocol: ProtoTCP, Address: "127.0.0.1:0"},
	)
	udp := &dns.Client{Net: "udp", Timeout: 300 * time.Millisecond}
	if resp, _, err := udp.Exchange(query("drop.example."), reg.Addr(ProtoUDP).String()); err == nil {
		t.Fatalf("udp: dropped query was answered: %v", resp)
	}
	tcp := &dns.Client{Net: "tcp", Timeout: 2 * time.Second}
	resp, _, err := tcp.Exchange(query("drop.example."), reg.Addr(ProtoTCP).String())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Rcode != dns.RcodeServerFailure {
		t.Fatalf("tcp: rcode %s, want SERVFAIL", dns.RcodeToString[resp.Rcode])
	}
	h := &Handler{Chain: pipeline.MustNew(d)}
	if m := h.Resolve(context.Background(), query("drop.example."), pipeline.Client{}); m == nil || m.Rcode != dns.RcodeServerFailure {
		t.Fatalf("Resolve on a drop = %v, want SERVFAIL", m)
	}
	if d.seen.Load() != 3 {
		t.Fatalf("drop stage saw %d queries, want 3", d.seen.Load())
	}
	for _, tt := range []struct {
		resp *pipeline.Response
		want bool
	}{
		{nil, false},
		{&pipeline.Response{Status: pipeline.StatusDropped}, true},
		{&pipeline.Response{Status: pipeline.StatusBlockedRateLimit}, true},
		{&pipeline.Response{Status: pipeline.StatusForwarded}, false},
		{&pipeline.Response{Status: pipeline.StatusDropped, Msg: new(dns.Msg)}, false},
	} {
		if got := IsDrop(tt.resp); got != tt.want {
			t.Errorf("IsDrop(%+v) = %v", tt.resp, got)
		}
	}
}

// BenchmarkUDPSockets measures Do53/UDP throughput on loopback with the
// local-answer stage, one socket against GOMAXPROCS SO_REUSEPORT sockets.
// Every client goroutine owns a connected UDP socket (its own source
// port, so the kernel's flow hash spreads clients over the sockets) and
// keeps one query in flight; q/s is the answered-query rate.
//
//	go test -run '^$' -bench BenchmarkUDPSockets -benchtime 5s ./internal/dnsserver/
func BenchmarkUDPSockets(b *testing.B) {
	counts := []int{1}
	if n := SocketCount(0); n > 1 {
		counts = append(counts, n)
	}
	for _, n := range counts {
		b.Run(fmt.Sprintf("sockets=%d", n), func(b *testing.B) {
			reg := runSocketRegistry(b, n, nil, Spec{Protocol: ProtoUDP, Address: "127.0.0.1:0"})
			addr := reg.Addr(ProtoUDP).String()
			wire, err := query("bench.example.").Pack()
			if err != nil {
				b.Fatal(err)
			}
			var answered, lost atomic.Int64
			b.SetParallelism(4)
			b.ResetTimer()
			start := time.Now()
			b.RunParallel(func(pb *testing.PB) {
				c, err := net.Dial("udp", addr)
				if err != nil {
					b.Error(err)
					return
				}
				defer func() { _ = c.Close() }()
				buf := make([]byte, 512)
				for pb.Next() {
					if _, err := c.Write(wire); err != nil {
						lost.Add(1)
						continue
					}
					_ = c.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
					if _, err := c.Read(buf); err != nil {
						lost.Add(1)
						continue
					}
					answered.Add(1)
				}
			})
			elapsed := time.Since(start)
			b.ReportMetric(float64(answered.Load())/elapsed.Seconds(), "q/s")
			b.ReportMetric(float64(lost.Load()), "lost")
		})
	}
}
