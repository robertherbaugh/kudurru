package upstream

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func withECS(m *dns.Msg, prefix string) *dns.Msg {
	p := netip.MustParsePrefix(prefix)
	m.SetEdns0(4096, false)
	m.IsEdns0().Option = append(m.IsEdns0().Option, ecsOption(p, 0))
	return m
}

func TestECSSubnet(t *testing.T) {
	public4 := netip.MustParseAddr("203.0.113.77")
	public6 := netip.MustParseAddr("2001:db8:1234:5678::1")
	for _, tc := range []struct {
		name   string
		mode   ECSMode
		msg    *dns.Msg
		client netip.Addr
		want   string // "" = none
	}{
		{"strip ignores client option", ECSStrip, withECS(query("a.", dns.TypeA), "198.51.100.0/24"), public4, ""},
		{"strip adds nothing", ECSStrip, query("a.", dns.TypeA), public4, ""},
		{"default mode is strip", "", query("a.", dns.TypeA), public4, ""},
		{"forward without option", ECSForward, query("a.", dns.TypeA), public4, ""},
		{"forward clamps v4", ECSForward, withECS(query("a.", dns.TypeA), "198.51.100.9/32"), public4, "198.51.100.0/24"},
		{"forward keeps shorter", ECSForward, withECS(query("a.", dns.TypeA), "198.51.0.0/16"), public4, "198.51.0.0/16"},
		{"forward clamps v6", ECSForward, withECS(query("a.", dns.TypeA), "2001:db8:aaaa:bbbb::/64"), public4, "2001:db8:aaaa:bb00::/56"},
		{"forward opt-out /0", ECSForward, withECS(query("a.", dns.TypeA), "0.0.0.0/0"), public4, "0.0.0.0/0"},
		{"add derives v4", ECSAdd, query("a.", dns.TypeA), public4, "203.0.113.0/24"},
		{"add derives v6", ECSAdd, query("a.", dns.TypeA), public6, "2001:db8:1234:5600::/56"},
		{"add unmaps v4-in-v6", ECSAdd, query("a.", dns.TypeA), netip.AddrFrom16(public4.As16()), "203.0.113.0/24"},
		{"add prefers client option", ECSAdd, withECS(query("a.", dns.TypeA), "198.51.100.0/24"), public4, "198.51.100.0/24"},
		{"add skips rfc1918", ECSAdd, query("a.", dns.TypeA), netip.MustParseAddr("192.168.1.5"), ""},
		{"add skips cgnat", ECSAdd, query("a.", dns.TypeA), netip.MustParseAddr("100.64.3.4"), ""},
		{"add skips loopback", ECSAdd, query("a.", dns.TypeA), netip.MustParseAddr("127.0.0.1"), ""},
		{"add skips ula", ECSAdd, query("a.", dns.TypeA), netip.MustParseAddr("fd00::1"), ""},
		{"add skips unknown client", ECSAdd, query("a.", dns.TypeA), netip.Addr{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := Config{ECS: ECSConfig{Mode: tc.mode}}.withDefaults().ECS
			got := e.subnet(tc.msg, tc.client)
			if tc.want == "" {
				if got.IsValid() {
					t.Fatalf("got %v, want none", got)
				}
				return
			}
			if got != netip.MustParsePrefix(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPrepareQuery(t *testing.T) {
	s := newSet(t, Config{
		Groups: map[string]GroupConfig{DefaultGroup: {Servers: []string{"127.0.0.1:1"}}},
		ECS:    ECSConfig{Mode: ECSAdd},
	}, Options{})
	in := query("Example.COM", dns.TypeAAAA)
	in.Id = 0
	in.RecursionDesired = false
	in.CheckingDisabled = true
	in.SetEdns0(4096, true)
	in.IsEdns0().Option = append(in.IsEdns0().Option, &dns.EDNS0_COOKIE{Code: dns.EDNS0COOKIE, Cookie: "0102030405060708"}, &dns.EDNS0_PADDING{Padding: make([]byte, 64)})
	in.Question = append(in.Question, dns.Question{Name: "second.", Qtype: dns.TypeA, Qclass: dns.ClassINET})

	q := s.PrepareQuery(in, netip.MustParseAddr("203.0.113.9"))
	if q.Id == 0 || !q.RecursionDesired || !q.CheckingDisabled || len(q.Question) != 1 || q.Question[0].Name != "Example.COM." {
		t.Fatalf("header/question: %+v", q.MsgHdr)
	}
	opt := q.IsEdns0()
	if opt == nil || opt.UDPSize() != EDNSBufferSize || !opt.Do() || len(opt.Option) != 1 {
		t.Fatalf("OPT: %v", opt)
	}
	ecs, ok := opt.Option[0].(*dns.EDNS0_SUBNET)
	if !ok || ecs.SourceNetmask != 24 || !ecs.Address.Equal(net.IPv4(203, 0, 113, 0)) {
		t.Fatalf("ECS: %v", opt.Option[0])
	}
	if in.Id != 0 || in.IsEdns0().UDPSize() != 4096 {
		t.Fatal("PrepareQuery must not modify the client message")
	}
	if _, err := q.Pack(); err != nil {
		t.Fatal(err)
	}
	if got := s.ECSSubnet(in, netip.MustParseAddr("203.0.113.9")); got != netip.MustParsePrefix("203.0.113.0/24") {
		t.Fatalf("ECSSubnet = %v", got)
	}
}

// TestECSEndToEnd checks what a real upstream receives in each mode.
func TestECSEndToEnd(t *testing.T) {
	f := startFake(t)
	client := netip.MustParseAddr("203.0.113.9")
	for _, tc := range []struct {
		mode ECSMode
		msg  *dns.Msg
		want string
	}{
		{ECSStrip, withECS(query("a.test", dns.TypeA), "198.51.100.0/24"), ""},
		{ECSForward, withECS(query("a.test", dns.TypeA), "198.51.100.7/32"), "198.51.100.0/24"},
		{ECSForward, query("a.test", dns.TypeA), ""},
		{ECSAdd, query("a.test", dns.TypeA), "203.0.113.0/24"},
	} {
		s := newSet(t, Config{Groups: map[string]GroupConfig{DefaultGroup: {Servers: []string{f.addr}}}, ECS: ECSConfig{Mode: tc.mode}}, Options{})
		r, err := s.Exchange(t.Context(), DefaultGroup, s.PrepareQuery(tc.msg, client))
		if err != nil {
			t.Fatal(err)
		}
		got := clientECS(f.lastQuery())
		switch {
		case tc.want == "" && got != nil:
			t.Errorf("%s: upstream received ECS %v", tc.mode, got)
		case tc.want != "":
			p := netip.MustParsePrefix(tc.want)
			if got == nil || int(got.SourceNetmask) != p.Bits() || !got.Address.Equal(net.IP(p.Addr().AsSlice())) {
				t.Errorf("%s: upstream received %v, want %v", tc.mode, got, tc.want)
			}
		}

		// The client gets its own option echoed with the upstream's scope.
		resp := r.Msg.Copy()
		FinishReply(tc.msg, resp, -1)
		if s := clientECS(tc.msg); s != nil {
			echo := clientECS(resp)
			if echo == nil || echo.SourceNetmask != s.SourceNetmask {
				t.Errorf("%s: echo %v", tc.mode, echo)
			}
		}
	}
}

func TestFinishReply(t *testing.T) {
	upstreamResp := func() *dns.Msg {
		m := new(dns.Msg)
		m.SetReply(query("x.test", dns.TypeA))
		m.Id = 999
		m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "x.test.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(192, 0, 2, 1)}}
		m.SetEdns0(1232, true)
		m.IsEdns0().Option = []dns.EDNS0{
			&dns.EDNS0_EDE{InfoCode: dns.ExtendedErrorCodeFiltered, ExtraText: "upstream says"},
			&dns.EDNS0_SUBNET{Code: dns.EDNS0SUBNET, Family: 1, SourceNetmask: 24, SourceScope: 20, Address: net.IPv4(198, 51, 100, 0)},
			&dns.EDNS0_COOKIE{Code: dns.EDNS0COOKIE, Cookie: "0102030405060708"},
		}
		return m
	}

	t.Run("no EDNS", func(t *testing.T) {
		req := query("X.test", dns.TypeA)
		req.Id = 7
		resp := upstreamResp()
		FinishReply(req, resp, -1)
		if resp.Id != 7 || resp.IsEdns0() != nil || resp.Question[0].Name != "X.test." || len(resp.Answer) != 1 {
			t.Fatalf("got %v", resp)
		}
	})
	t.Run("EDNS with ECS", func(t *testing.T) {
		req := withECS(query("x.test", dns.TypeA), "198.51.100.0/24")
		req.IsEdns0().SetDo(false)
		resp := upstreamResp()
		FinishReply(req, resp, -1)
		opt := resp.IsEdns0()
		if opt == nil || opt.Do() || opt.UDPSize() != EDNSBufferSize || len(opt.Option) != 2 {
			t.Fatalf("OPT: %v", opt)
		}
		if ede, ok := opt.Option[0].(*dns.EDNS0_EDE); !ok || ede.InfoCode != dns.ExtendedErrorCodeFiltered {
			t.Fatalf("EDE lost: %v", opt.Option)
		}
		if ecs := clientECS(resp); ecs == nil || ecs.SourceScope != 20 || ecs.SourceNetmask != 24 {
			t.Fatalf("ECS echo: %v", ecs)
		}
		if _, err := resp.Pack(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("forced scope", func(t *testing.T) {
		req := withECS(query("x.test", dns.TypeA), "198.51.100.0/24")
		resp := upstreamResp()
		FinishReply(req, resp, 24)
		if ecs := clientECS(resp); ecs == nil || ecs.SourceScope != 24 {
			t.Fatalf("ECS echo: %v", ecs)
		}
		AddEDE(resp, dns.ExtendedErrorCodeStaleAnswer, "")
		if n := len(resp.IsEdns0().Option); n != 3 {
			t.Fatalf("AddEDE: %d options", n)
		}
	})
	t.Run("extended rcode without EDNS", func(t *testing.T) {
		resp := upstreamResp()
		resp.Rcode = dns.RcodeBadCookie
		FinishReply(query("x.test", dns.TypeA), resp, -1)
		if resp.Rcode != dns.RcodeServerFailure {
			t.Fatalf("rcode %d", resp.Rcode)
		}
		if _, err := resp.Pack(); err != nil {
			t.Fatal(err)
		}
	})
	_ = time.Second
}
