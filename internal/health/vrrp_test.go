package health

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// dnsServer answers health TXT on one loopback port over UDP and TCP. A
// free UDP port is not necessarily free for TCP (parallel test binaries
// hold ephemeral TCP ports), so it retries until both bind.
func dnsServer(t *testing.T, rcode *atomic.Int32) string {
	t.Helper()
	var (
		pc net.PacketConn
		ln net.Listener
	)
	for range 20 {
		var err error
		if pc, err = net.ListenPacket("udp", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
		if ln, err = net.Listen("tcp", pc.LocalAddr().String()); err == nil {
			break
		}
		_ = pc.Close()
		pc = nil
	}
	if pc == nil {
		t.Fatal("no loopback port free for both UDP and TCP")
	}
	addr := pc.LocalAddr().String()
	h := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetRcode(r, int(rcode.Load()))
		if m.Rcode == dns.RcodeSuccess {
			m.Answer = []dns.RR{&dns.TXT{Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET}, Txt: []string{"node-a 1"}}}
		}
		_ = w.WriteMsg(m)
	})
	for _, s := range []*dns.Server{{PacketConn: pc, Handler: h}, {Listener: ln, Handler: h}} {
		go func() { _ = s.ActivateAndServe() }()
		t.Cleanup(func() { _ = s.Shutdown() })
	}
	return addr
}

func dnsdistAPI(t *testing.T, up *atomic.Int32) (string, string) {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "dnsdist-api.key")
	if err := os.WriteFile(keyFile, []byte("k3y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "k3y" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		state := "down"
		if up.Load() > 0 {
			state = "up"
		}
		_, _ = w.Write([]byte(`{"daemon_type":"dnsdist","servers":[{"name":"a","state":"` + state + `"},{"name":"b","state":"down"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, keyFile
}

func TestVRRPProbe(t *testing.T) {
	var rcode, up atomic.Int32
	up.Store(1)
	addr := dnsServer(t, &rcode)
	api, keyFile := dnsdistAPI(t, &up)
	_, port, _ := net.SplitHostPort(addr)
	vip := "192.0.2.53:" + port
	notHeld := func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("192.0.2.11")}, nil }
	p := VRRPProbe{DNS: vip, DNSDistAPI: api, DNSDistAPIKeyFile: keyFile, LocalAddrs: notHeld, Timeout: 2 * time.Second}

	// BACKUP: the VIP is not local, loopback is probed.
	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("%v (%s)", err, res)
	}
	if res.HoldsVIP || res.Target != addr || res.UDP != "NOERROR" || res.TCP != "NOERROR" || res.BackendsUp != 1 {
		t.Errorf("result %s", res)
	}

	// MASTER: the VIP itself is probed (here it is unreachable).
	held := p
	held.LocalAddrs = func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("192.0.2.53")}, nil }
	held.Timeout = 300 * time.Millisecond
	res, err = held.Run(context.Background())
	if err == nil || !res.HoldsVIP || res.Target != vip {
		t.Errorf("held VIP: %v %s", err, res)
	}

	// No backend up → FAULT.
	up.Store(0)
	if res, err := p.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "0 of 2 backends up") {
		t.Errorf("backends down: %v %s", err, res)
	}
	up.Store(1)

	// REFUSED from dnsdist → FAULT.
	rcode.Store(dns.RcodeRefused)
	if res, err := p.Run(context.Background()); err == nil || res.UDP != "REFUSED" {
		t.Errorf("refused: %v %s", err, res)
	}
	rcode.Store(dns.RcodeSuccess)

	// --require-backends 2 with one up → FAULT.
	two := p
	two.RequireBackends = 2
	if _, err := two.Run(context.Background()); err == nil {
		t.Error("require-backends 2 passed with one backend up")
	}

	// A wrong key file → FAULT.
	bad := p
	bad.DNSDistAPIKeyFile = filepath.Join(t.TempDir(), "missing")
	if _, err := bad.Run(context.Background()); err == nil {
		t.Error("missing key file passed")
	}
	if _, err := (VRRPProbe{DNS: "not-an-addr"}).Run(context.Background()); err == nil {
		t.Error("bad --dns accepted")
	}
}
