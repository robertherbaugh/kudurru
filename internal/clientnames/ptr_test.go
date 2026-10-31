package clientnames

import (
	"context"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func ptrServer(t *testing.T, handler dns.HandlerFunc) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	server := &dns.Server{PacketConn: pc, Handler: handler, NotifyStartedFunc: func() { close(started) }}
	done := make(chan error, 1)
	go func() { done <- server.ActivateAndServe() }()
	<-started
	t.Cleanup(func() {
		if err := server.Shutdown(); err != nil {
			t.Error(err)
		}
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return pc.LocalAddr().String()
}

func options(server string) Options {
	return Options{Servers: []string{server}, Networks: []string{"192.0.2.0/24"}, MaxEntries: 16, Workers: 2, Timeout: 100 * time.Millisecond, MaxAge: time.Second, NegativeTTL: time.Second}
}

func startResolver(t *testing.T, o Options) *Resolver {
	t.Helper()
	r, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	eventually(t, func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.running })
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("worker shutdown exceeded deadline")
		}
		if r.Hostnames(netip.MustParseAddr("192.0.2.3")) != nil {
			t.Error("names retained after shutdown")
		}
	})
	return r
}

func eventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func response(q *dns.Msg, name string) *dns.Msg {
	a := new(dns.Msg)
	a.SetReply(q)
	a.Answer = []dns.RR{&dns.PTR{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 60}, Ptr: name}}
	return a
}

func TestPTRActualUDPAsyncExpiryAndNegative(t *testing.T) {
	var requests atomic.Int32
	var negative atomic.Bool
	server := ptrServer(t, func(w dns.ResponseWriter, q *dns.Msg) {
		requests.Add(1)
		a := response(q, "Phone.LAN.")
		if negative.Load() {
			a.Answer = nil
			a.Rcode = dns.RcodeNameError
		}
		_ = w.WriteMsg(a)
	})
	r := startResolver(t, options(server))
	addr := netip.MustParseAddr("192.0.2.3")
	if got := r.Hostnames(addr); got != nil {
		t.Fatal("cold lookup blocked for a result")
	}
	eventually(t, func() bool { names := r.Hostnames(addr); return len(names) == 1 && names[0] == "phone.lan" })
	if requests.Load() != 1 {
		t.Fatal("lookup was not deduplicated")
	}
	names := r.Hostnames(addr)
	names[0] = "mutated"
	if r.Hostnames(addr)[0] != "phone.lan" {
		t.Fatal("caller mutated cache")
	}
	negative.Store(true)
	time.Sleep(time.Second + 10*time.Millisecond)
	if r.Hostnames(addr) != nil {
		t.Fatal("expired name retained")
	}
	eventually(t, func() bool { r.mu.Lock(); defer r.mu.Unlock(); return !r.entries[addr].pending })
	for range 100 {
		if r.Hostnames(addr) != nil {
			t.Fatal("negative acquired a name")
		}
	}
	if requests.Load() != 2 {
		t.Fatal("negative backoff ignored")
	}
	if r.Hostnames(netip.MustParseAddr("198.51.100.1")) != nil {
		t.Fatal("out-of-scope address")
	}
	if err := r.Run(t.Context()); err == nil {
		t.Fatal("resolver allowed a second owner")
	}
}

func TestPTRRejectsInvalidAnswersOnWire(t *testing.T) {
	cases := map[string]func(*dns.Msg){
		"ambiguous": func(a *dns.Msg) { p := *a.Answer[0].(*dns.PTR); p.Ptr = "other.lan."; a.Answer = append(a.Answer, &p) },
		"cname": func(a *dns.Msg) {
			a.Answer = []dns.RR{&dns.CNAME{Hdr: dns.RR_Header{Name: a.Question[0].Name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 60}, Target: "other.lan."}}
		},
		"wrong-owner":    func(a *dns.Msg) { a.Answer[0].Header().Name = "other.arpa." },
		"wrong-class":    func(a *dns.Msg) { a.Answer[0].Header().Class = dns.ClassCHAOS },
		"zero-ttl":       func(a *dns.Msg) { a.Answer[0].Header().Ttl = 0 },
		"truncated":      func(a *dns.Msg) { a.Truncated = true },
		"wrong-question": func(a *dns.Msg) { a.Question[0].Qtype = dns.TypeA },
		"wrong-id":       func(a *dns.Msg) { a.Id++ },
		"invalid-name":   func(a *dns.Msg) { a.Answer[0].(*dns.PTR).Ptr = "*.lan." },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			server := ptrServer(t, func(w dns.ResponseWriter, q *dns.Msg) { a := response(q, "phone.lan."); mutate(a); _ = w.WriteMsg(a) })
			r := startResolver(t, options(server))
			addr := netip.MustParseAddr("192.0.2.3")
			_ = r.Hostnames(addr)
			eventually(t, func() bool { r.mu.Lock(); defer r.mu.Unlock(); return !r.entries[addr].pending })
			if r.Hostnames(addr) != nil {
				t.Fatal("invalid DNS evidence admitted")
			}
		})
	}
}

func TestPTRBoundsAndLateResponse(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 16)
	server := ptrServer(t, func(w dns.ResponseWriter, q *dns.Msg) {
		entered <- struct{}{}
		<-release
		_ = w.WriteMsg(response(q, "late.lan."))
	})
	t.Cleanup(func() { close(release) })
	o := options(server)
	o.MaxEntries = 4
	r := startResolver(t, o)
	addr := netip.MustParseAddr("192.0.2.3")
	_ = r.Hostnames(addr)
	<-entered
	for range 1000 {
		_ = r.Hostnames(addr)
	}
	r.mu.Lock()
	queued := len(r.queue)
	r.mu.Unlock()
	if queued != 0 {
		t.Fatal("duplicate in-flight address queued")
	}
	for n := 1; n < 255; n++ {
		_ = r.Hostnames(netip.AddrFrom4([4]byte{192, 0, 2, byte(n)}))
	}
	r.mu.Lock()
	entries, queue := len(r.entries), len(r.queue)
	r.mu.Unlock()
	if entries > 4 || queue > 4 {
		t.Fatal("unbounded acquisition")
	}
	// The original entry was evicted while its real UDP request was held. Its
	// eventual result cannot bind to a replacement for the same address.
	_ = r.Hostnames(addr)
	time.Sleep(150 * time.Millisecond)
	if r.Hostnames(addr) != nil {
		t.Fatal("late/evicted acquisition revived identity")
	}
}

func TestPTRValidation(t *testing.T) {
	for _, change := range []func(*Options){
		func(o *Options) { o.Servers = nil }, func(o *Options) { o.Servers = []string{"resolver.local:53"} },
		func(o *Options) { o.Servers = []string{"0.0.0.0:53"} }, func(o *Options) { o.Servers = []string{"[fe80::1%eth0]:53"} },
		func(o *Options) { o.Networks = nil }, func(o *Options) { o.Networks = []string{"192.0.2.1"} },
		func(o *Options) { o.MaxEntries = 65537 }, func(o *Options) { o.Workers = 9 }, func(o *Options) { o.Timeout = 10 * time.Second },
	} {
		o := options("127.0.0.1:53")
		change(&o)
		if _, err := New(o); err == nil {
			t.Fatal("invalid bounds accepted")
		}
	}
}
