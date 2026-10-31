package dnsserver

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/proxyproto"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
)

// TestRateLimitThroughPROXYv2 runs the ingress limiter end to end on
// multi-socket listeners behind an authenticated PROXYv2 peer: the
// PROXYv2-attributed client (not the loopback dnsdist) is charged, UDP
// beyond the limit alternates drop and TC=1 (slip 50), and TCP from the
// same client is never limited.
func TestRateLimitThroughPROXYv2(t *testing.T) {
	lim, err := ingress.NewRateLimiter(ingress.RateLimitConfig{IPv4: map[int]int{32: 2}, Slip: 50})
	if err != nil {
		t.Fatal(err)
	}
	var drops, slips atomic.Int64
	if got := testutil.ToFloat64(lim.SlipCollector()); got != 0 {
		t.Fatalf("initial slip counter = %v, want 0", got)
	}
	lim.SetHook(func(v ingress.Verdict) {
		if v == ingress.VerdictDrop {
			drops.Add(1)
		} else {
			slips.Add(1)
		}
	})
	reg, err := NewRegistry(Options{
		Handler: &Handler{Chain: pipeline.MustNew(&ingress.Stage{NodeID: "n1", Limiter: lim}, localAnswer{}), Logger: quietLogger},
		Logger:  quietLogger,
		Proxy:   mustPolicy(t, []string{"127.0.0.1/32"}, proxySecret),
		Sockets: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	f := runRegistry(t, reg, Spec{Protocol: ProtoUDP, Address: "127.0.0.1:0"}, Spec{Protocol: ProtoTCP, Address: "127.0.0.1:0"})
	udpAddr, tcpAddr := f.Addr(ProtoUDP).String(), f.Addr(ProtoTCP).String()

	var got []string
	for i := range 6 {
		wire, err := query(fmt.Sprintf("q%d.example.", i)).Pack()
		if err != nil {
			t.Fatal(err)
		}
		payload := append(ppHeader(t, proxyproto.TransportDatagram, udpAddr, secretTLV(proxySecret)), wire...)
		resp, err := udpFrom(t, "127.0.0.1", udpAddr, payload, 300*time.Millisecond)
		switch {
		case err != nil:
			got = append(got, "none")
		case resp.Truncated && len(resp.Answer) == 0:
			got = append(got, "tc")
		case len(resp.Answer) == 1:
			got = append(got, "answer")
		default:
			got = append(got, "unexpected:"+resp.String())
		}
	}
	if want := "answer answer none tc none tc"; strings.Join(got, " ") != want {
		t.Fatalf("UDP verdicts %q, want %q", strings.Join(got, " "), want)
	}
	if drops.Load() != 2 || slips.Load() != 2 {
		t.Fatalf("hook saw %d drops, %d slips; want 2 and 2", drops.Load(), slips.Load())
	}

	// The same client over TCP (it proved its address) is answered.
	wire, _ := query("tcp.example.").Pack()
	resp, err := tcpFrom(t, "127.0.0.1", tcpAddr, ppHeader(t, proxyproto.TransportStream, tcpAddr, secretTLV(proxySecret)), wire, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkA(t, resp)
	if got := testutil.ToFloat64(lim.SlipCollector()); got != 2 {
		t.Fatalf("slip counter = %v, want 2 UDP TC responses; allow/drop/TCP must not count", got)
	}
	if err := lim.Reconfigure(nil); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(lim.SlipCollector()); got != 2 {
		t.Fatalf("disabling the limiter reset the cumulative slip counter: %v", got)
	}
}

// runRegistry binds and runs reg with specs until the test ends.
func runRegistry(t *testing.T, reg *Registry, specs ...Spec) *Registry {
	t.Helper()
	if err := reg.Add(specs...); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	done := make(chan error, 1)
	go func() { done <- reg.Run(ctx) }()
	t.Cleanup(func() {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("registry Run: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("registry did not shut down")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok, _ := reg.Ready(); ok {
			return reg
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("listeners not ready")
	return nil
}
