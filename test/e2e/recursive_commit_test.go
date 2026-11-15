package e2e

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// standInRoot is an authoritative server that stands for the root servers
// of a recursive group: every A question is answered with standInAddress,
// NS questions for the root name its own address, everything else is an
// empty NOERROR. It records every question it receives.
type standInRoot struct {
	addr string
	mu   sync.Mutex
	seen map[string]int
}

const standInAddress = "192.0.2.77"

// recursiveRootAddress is a local non-loopback IPv4 address: the native
// resolver refuses loopback authorities, and the test does not relax that.
func recursiveRootAddress(t *testing.T) netip.Addr {
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
		switch {
		case !ip.Is4() || ip.IsLoopback():
		case ip.IsGlobalUnicast():
			return ip
		case ip.IsLinkLocalUnicast() && !fallback.IsValid():
			fallback = ip
		}
	}
	if fallback.IsValid() {
		return fallback
	}
	t.Fatal("a recursive node test needs a local non-loopback IPv4 interface")
	return netip.Addr{}
}

func startStandInRoot(t *testing.T) *standInRoot {
	t.Helper()
	ip := recursiveRootAddress(t)
	packet, err := net.ListenPacket("udp4", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", packet.LocalAddr().String())
	if err != nil {
		_ = packet.Close()
		t.Fatal(err)
	}
	root := &standInRoot{addr: packet.LocalAddr().String(), seen: map[string]int{}}
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(q)
		m.Authoritative = true
		if len(q.Question) == 1 {
			question := q.Question[0]
			name := strings.ToLower(question.Name)
			root.mu.Lock()
			root.seen[name]++
			root.mu.Unlock()
			switch {
			case question.Qtype == dns.TypeA:
				m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30}, A: net.ParseIP(standInAddress)})
			case question.Qtype == dns.TypeNS && name == ".":
				m.Answer = append(m.Answer, &dns.NS{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: 3600}, Ns: "a.root."})
				m.Extra = append(m.Extra, &dns.A{Hdr: dns.RR_Header{Name: "a.root.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 3600}, A: net.IP(ip.AsSlice())})
			}
		}
		_ = w.WriteMsg(m)
	})
	for _, server := range []*dns.Server{{PacketConn: packet, Handler: handler}, {Listener: listener, Handler: handler}} {
		started := make(chan struct{})
		server.NotifyStartedFunc = func() { close(started) }
		go func() { _ = server.ActivateAndServe() }()
		t.Cleanup(func() { _ = server.Shutdown() })
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("stand-in root did not start")
		}
	}
	return root
}

func (r *standInRoot) asked(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen[strings.ToLower(name)] > 0
}

// TestRecursiveAnsweredAcrossCommits: a node whose default group is
// mode: recursive keeps answering cache-miss lookups while configuration
// commits land (each API token created through the admin socket is one).
// A lookup that arrived in the tens of milliseconds after a commit used to
// be refused before recursion started, because the retired data generation
// no longer admitted native recursion: first it got no answer at all, then
// (with the retained upstream-failure release) an immediate SERVFAIL with
// extended error 23 although no upstream had been asked. Every lookup must
// now be answered NOERROR with the stand-in's address on its first try
// (D377).
func TestRecursiveAnsweredAcrossCommits(t *testing.T) {
	kudurrutest.RequireE2E(t)
	root := startStandInRoot(t)
	spec, cfg := kudurrutest.NewNodeSpec(t, t.TempDir(), "recursive-commit")
	cfg["upstreams"] = map[string]any{
		"groups": map[string]any{
			"default": map[string]any{"mode": "recursive", "strategy": "parallel_best", "timeout": "2s"},
		},
		"recursion": map[string]any{
			"rootHints":         []any{root.addr},
			"dnssec":            false,
			"qnameMinimization": false,
			"ipv6":              "off",
		},
	}
	kudurrutest.WriteConfig(t, spec.Config, cfg)
	kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{
		Env: []string{"KUDURRU_ADMIN_PASSWORD=recursive-commit-e2e-password-1"}, // synthetic fixture; avoid generated credentials in failure logs
	})
	socket := filepath.Join(spec.DataDir, "admin.sock")

	const rounds = 5
	// One try per lookup, like a stub resolver's first attempt: a lookup
	// that the node drops is a failure here, not a retry.
	c := &dns.Client{Timeout: 3 * time.Second}
	sent := 0
	ask := func(round int) {
		t.Helper()
		name := fmt.Sprintf("q%d.recursive-commit.example.", sent)
		m := new(dns.Msg)
		m.SetQuestion(name, dns.TypeA)
		m.SetEdns0(1232, false)
		sent++
		r, _, err := c.Exchange(m, spec.Ports.DNS)
		if err != nil {
			t.Fatalf("commit %d of %d, query %d: no answer: %v (stand-in root asked: %v)", round, rounds, sent, err, root.asked(name))
		}
		if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 {
			ede := ""
			if opt := r.IsEdns0(); opt != nil {
				for _, o := range opt.Option {
					if e, ok := o.(*dns.EDNS0_EDE); ok {
						ede = fmt.Sprintf(" EDE %d", e.InfoCode)
					}
				}
			}
			t.Fatalf("commit %d of %d, query %d: %s%s with %d answers, want NOERROR with %s (stand-in root asked: %v)",
				round, rounds, sent, dns.RcodeToString[r.Rcode], ede, len(r.Answer), standInAddress, root.asked(name))
		}
		a, ok := r.Answer[0].(*dns.A)
		if !ok || a.A.String() != standInAddress {
			t.Fatalf("commit %d of %d, query %d: answer %v, want %s", round, rounds, sent, r.Answer[0], standInAddress)
		}
	}
	ask(0)
	for round := 1; round <= rounds; round++ {
		created := make(chan error, 1)
		go func() {
			out, err := exec.Command(kudurruBin, "admin", "create-token", "--socket", socket).Output() //nolint:gosec // the test's binary
			if err == nil && strings.TrimSpace(string(out)) == "" {
				err = errors.New("no token printed")
			}
			created <- err
		}()
		// Lookups go out one after another from before the commit until
		// well after the command returned, so that some are in flight when
		// the commit lands and some arrive before its successor is served.
		var until time.Time
		for until.IsZero() || time.Now().Before(until) {
			ask(round)
			if until.IsZero() {
				select {
				case err := <-created:
					if err != nil {
						t.Fatalf("create-token %d: %v", round, err)
					}
					until = time.Now().Add(250 * time.Millisecond)
				default:
				}
			}
			time.Sleep(time.Millisecond)
		}
	}
	t.Logf("%d cache-miss lookups answered NOERROR across %d commits", sent, rounds)
}
