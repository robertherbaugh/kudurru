package control

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func msg(name string, qtype uint16, mutate func(*dns.Msg)) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(name, qtype)
	if mutate != nil {
		mutate(m)
	}
	return m
}

func TestDecide(t *testing.T) {
	st := New(Options{
		AllowedNetworks: []netip.Prefix{
			netip.MustParsePrefix("10.0.0.0/8"),
			netip.MustParsePrefix("::ffff:192.168.0.0/112"), // IPv4-mapped form of 192.168.0.0/16
			netip.MustParsePrefix("2001:db8::/32"),
			{}, // invalid entries are ignored
		},
		BlockedQTypes: []uint16{dns.TypeHINFO, dns.TypeNULL},
	})
	cases := []struct {
		name string
		from string
		m    *dns.Msg
		want Action
	}{
		{"allowed IPv4", "10.1.2.3", msg("example.com.", dns.TypeA, nil), Allow},
		{"allowed via mapped prefix", "192.168.7.7", msg("example.com.", dns.TypeA, nil), Allow},
		{"allowed IPv4-mapped client", "::ffff:10.1.2.3", msg("example.com.", dns.TypeA, nil), Allow},
		{"allowed IPv6", "2001:db8::1", msg("example.com.", dns.TypeAAAA, nil), Allow},
		{"loopback always allowed", "127.0.0.1", msg("example.com.", dns.TypeA, nil), Allow},
		{"IPv6 loopback always allowed", "::1", msg("example.com.", dns.TypeA, nil), Allow},
		{"outside the allowed networks", "203.0.113.9", msg("example.com.", dns.TypeA, nil), Refuse},
		{"no client address", "", msg("example.com.", dns.TypeA, nil), Refuse},
		{"blocked qtype", "10.1.2.3", msg("example.com.", dns.TypeHINFO, nil), Refuse},
		{"QR set (reflected response)", "10.1.2.3", msg("example.com.", dns.TypeA, func(m *dns.Msg) { m.Response = true }), DropSilently},
		{"qtype 0", "10.1.2.3", msg("example.com.", 0, nil), DropSilently},
		{"qtype OPT", "10.1.2.3", msg("example.com.", dns.TypeOPT, nil), DropSilently},
		{"qclass 0", "10.1.2.3", msg("example.com.", dns.TypeA, func(m *dns.Msg) { m.Question[0].Qclass = 0 }), DropSilently},
		{"not fully qualified", "10.1.2.3", msg("example.com.", dns.TypeA, func(m *dns.Msg) { m.Question[0].Name = "example.com" }), DropSilently},
		{"bad name", "10.1.2.3", msg("example.com.", dns.TypeA, func(m *dns.Msg) { m.Question[0].Name = "a..b." }), DropSilently},
		{"no question", "10.1.2.3", msg("example.com.", dns.TypeA, func(m *dns.Msg) { m.Question = nil }), DropSilently},
		{"malformed beats refused", "203.0.113.9", msg("example.com.", 0, nil), DropSilently},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &pipeline.Request{Msg: tc.m}
			if tc.from != "" {
				req.Client.Addr = netip.AddrPortFrom(netip.MustParseAddr(tc.from), 1)
			}
			if got := st.Decide(req); got != tc.want {
				t.Errorf("Decide = %s, want %s", got, tc.want)
			}
		})
	}
	if st.Decide(&pipeline.Request{}) != DropSilently {
		t.Error("nil message must be dropped")
	}
	if New(Options{}).Decide(&pipeline.Request{Msg: msg("example.com.", dns.TypeA, nil)}) != Allow {
		t.Error("no rules: everything is allowed, even without a client address")
	}
	keep := New(Options{KeepMalformed: true})
	if keep.Decide(&pipeline.Request{Msg: msg("example.com.", 0, nil)}) != Allow {
		t.Error("KeepMalformed")
	}
}

func TestServe(t *testing.T) {
	st := New(Options{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}})
	if st.Name() != "control" {
		t.Fatal(st.Name())
	}
	reached := 0
	next := func(r *pipeline.Request) (*pipeline.Response, error) {
		reached++
		return pipeline.Reply(r, dns.RcodeSuccess, pipeline.StatusForwarded), nil
	}
	serve := func(from string, m *dns.Msg) (*pipeline.Response, error) {
		return st.Serve(&pipeline.Request{Msg: m, Client: pipeline.Client{Addr: netip.AddrPortFrom(netip.MustParseAddr(from), 1)}}, next)
	}

	resp, err := serve("10.0.0.1", msg("example.com.", dns.TypeA, nil))
	if err != nil || resp.Status != pipeline.StatusForwarded || reached != 1 {
		t.Fatalf("allow: %v %v", resp, err)
	}

	q := msg("example.com.", dns.TypeA, func(m *dns.Msg) { m.SetEdns0(4096, true) })
	resp, err = serve("192.0.2.1", q)
	if err != nil || resp.Msg.Rcode != dns.RcodeRefused || resp.Status != pipeline.StatusRefusedACL {
		t.Fatalf("refuse: %v %v", resp, err)
	}
	opt := resp.Msg.IsEdns0()
	if opt == nil || !opt.Do() || len(opt.Option) != 1 {
		t.Fatalf("refusal must echo EDNS with one EDE option: %v", opt)
	}
	if e, ok := opt.Option[0].(*dns.EDNS0_EDE); !ok || e.InfoCode != dns.ExtendedErrorCodeProhibited {
		t.Errorf("EDE 18 expected, got %v", opt.Option[0])
	}
	resp, _ = serve("192.0.2.1", msg("example.com.", dns.TypeA, nil))
	if resp.Msg.IsEdns0() != nil {
		t.Error("no OPT in the refusal of a query without EDNS")
	}

	resp, err = serve("10.0.0.1", msg("example.com.", dns.TypeOPT, nil))
	if resp != nil || !errors.Is(err, pipeline.ErrDrop) {
		t.Fatalf("drop: %v %v", resp, err)
	}
	if reached != 1 {
		t.Errorf("refused and dropped queries must not continue (%d)", reached)
	}
	for a, want := range map[Action]string{Allow: "allow", DropSilently: "drop", Refuse: "refuse", Action(9): "unknown"} {
		if a.String() != want {
			t.Errorf("%d: %s", a, a)
		}
	}
}
