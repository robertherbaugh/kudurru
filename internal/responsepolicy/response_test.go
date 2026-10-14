package responsepolicy

import (
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func settings(t *testing.T, opts Options) *Settings {
	t.Helper()
	s, err := Compile(opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func query(typ uint16) *pipeline.Request {
	m := new(dns.Msg)
	m.SetQuestion("www.example.", typ)
	m.SetEdns0(1232, false)
	return &pipeline.Request{Msg: m, Client: pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.5:1234")}}
}

func response(t *testing.T, req *pipeline.Request, rcode int, records ...string) *pipeline.Response {
	t.Helper()
	r := pipeline.Reply(req, rcode, pipeline.StatusForwarded)
	r.Origin, r.Cacheable = pipeline.OriginForwarded, true
	r.Msg.AuthenticatedData = true
	for _, text := range records {
		rr, err := dns.NewRR(text)
		if err != nil {
			t.Fatal(err)
		}
		r.Msg.Answer = append(r.Msg.Answer, rr)
	}
	return r
}

func TestDNS64RFC6052Layouts(t *testing.T) {
	for _, tc := range []struct{ prefix, want string }{
		{"2001:db8::/32", "2001:db8:c000:221::"},
		{"2001:db8:100::/40", "2001:db8:1c0:2:21::"},
		{"2001:db8:122::/48", "2001:db8:122:c000:2:2100::"},
		{"2001:db8:122:300::/56", "2001:db8:122:3c0:0:221::"},
		{"2001:db8:122:344::/64", "2001:db8:122:344:c0:2:2100:0"},
		{"64:ff9b::/96", "64:ff9b::c000:221"},
	} {
		t.Run(tc.prefix, func(t *testing.T) {
			p, err := ParseDNS64Prefix(tc.prefix)
			if err != nil {
				t.Fatal(err)
			}
			if got := embed(p, netip.MustParseAddr("192.0.2.33")); got != netip.MustParseAddr(tc.want) {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestDNS64DenialTTLAndIsolation(t *testing.T) {
	s := settings(t, Options{DNS64Prefix: "64:ff9b::/96", TTL: 300})
	req := query(dns.TypeAAAA)
	denial := response(t, req, dns.RcodeSuccess)
	soa, _ := dns.NewRR("example. 50 IN SOA ns.example. hostmaster.example. 1 3600 60 86400 30")
	denial.Msg.Ns = []dns.RR{soa}
	a := response(t, query(dns.TypeA), dns.RcodeSuccess, "www.example. 90 IN A 192.0.2.33")
	beforeDenial, beforeA := denial.Msg.String(), a.Msg.String()
	got := s.DNS64(req, denial, a)
	if got == denial || !got.Modified || got.Cacheable || got.Msg.AuthenticatedData || len(got.Msg.Answer) != 1 || len(got.Msg.Ns) != 0 {
		t.Fatalf("bad synthesis: %+v", got)
	}
	v6 := got.Msg.Answer[0].(*dns.AAAA)
	if v6.AAAA.String() != "64:ff9b::c000:221" || v6.Hdr.Ttl != 30 {
		t.Fatal(v6)
	}
	if denial.Msg.String() != beforeDenial || a.Msg.String() != beforeA {
		t.Fatal("raw dependency changed")
	}
	for _, change := range []string{"DO", "NXDOMAIN", "positive", "domain_excluded", "ip_excluded", "filter", "bad_alias"} {
		t.Run(change, func(t *testing.T) {
			q, d, opts := query(dns.TypeAAAA), response(t, req, dns.RcodeSuccess), Options{DNS64Prefix: "64:ff9b::/96"}
			switch change {
			case "DO":
				q.Msg.IsEdns0().SetDo()
			case "NXDOMAIN":
				d.Msg.Rcode = dns.RcodeNameError
			case "positive":
				d = response(t, req, 0, "www.example. 60 IN AAAA 2001:db8::1")
			case "domain_excluded":
				opts.DNS64ExcludeDomains = []string{"example"}
			case "ip_excluded":
				opts.DNS64ExcludeIPv4 = []string{"192.0.2.0/24"}
			case "filter":
				opts.FilterAAAA = true
			case "bad_alias":
				d = response(t, req, 0, "www.example. 60 IN CNAME other.example.")
			}
			if got := settings(t, opts).DNS64(q, d, a); got != d {
				t.Fatalf("unexpected synthesis: %s", got.Msg)
			}
		})
	}
}

func TestDNS64RebindingCannotHideIPv4(t *testing.T) {
	s := settings(t, Options{DNS64Prefix: "64:ff9b::/96", Rebinding: true})
	req := query(dns.TypeAAAA)
	d := response(t, req, dns.RcodeSuccess)
	a := response(t, query(dns.TypeA), dns.RcodeSuccess, "www.example. 60 IN A 192.168.1.1")
	got := s.DNS64(req, d, a)
	if got.Msg.Rcode != dns.RcodeRefused || got.Rule != "rebinding" {
		t.Fatalf("IPv4 hidden in NAT64: %s", got.Msg)
	}
}

func TestRebindingAllSectionsHintsAndExceptions(t *testing.T) {
	for _, text := range []string{
		"www.example. 60 IN A 10.0.0.1", "www.example. 60 IN A 127.0.0.1", "www.example. 60 IN A 169.254.169.254", "www.example. 60 IN A 0.0.0.0",
		"www.example. 60 IN AAAA fd00::1", "www.example. 60 IN AAAA ::ffff:192.168.1.1", "www.example. 60 IN AAAA ff02::1",
		"www.example. 60 IN HTTPS 1 . alpn=h2 ipv4hint=192.0.2.1,10.0.0.1", "www.example. 60 IN SVCB 1 . ipv6hint=2001:db8::1,fe80::1",
	} {
		for section := range 3 {
			t.Run(text+strings.Repeat("_", section), func(t *testing.T) {
				req := query(dns.TypeA)
				r := response(t, req, 0, text)
				if section == 1 {
					r.Msg.Ns, r.Msg.Answer = r.Msg.Answer, nil
				}
				if section == 2 {
					r.Msg.Extra, r.Msg.Answer = r.Msg.Answer, nil
				}
				before := r.Msg.String()
				if got := settings(t, Options{Rebinding: true}).Guard(req, r); got.Msg.Rcode != dns.RcodeRefused || got.Msg.AuthenticatedData {
					t.Fatal(got.Msg)
				}
				if r.Msg.String() != before {
					t.Fatal("guard mutated source")
				}
				for _, opts := range []Options{{Rebinding: true, PrivateDomains: []string{"example."}}, {Rebinding: true, BypassNetworks: []string{"192.0.2.0/24"}}} {
					if got := settings(t, opts).Guard(req, r); got != r {
						t.Fatal("explicit exception ignored")
					}
				}
			})
		}
	}
	req := query(dns.TypeA)
	r := response(t, req, 0, "www.example. 60 IN CNAME private.other.", "private.other. 60 IN A 10.0.0.1")
	s := settings(t, Options{Rebinding: true, PrivateDomains: []string{"example."}})
	if got := s.Guard(req, r); got == r {
		t.Fatal("alias escaped private-domain scope")
	}
	r.Origin = pipeline.OriginAuthoritative
	if got := s.Guard(req, r); got != r {
		t.Fatal("owned zone filtered")
	}
	r.Origin, r.Status = pipeline.OriginSynthetic, pipeline.StatusBlockedList
	if got := s.Guard(req, r); got != r {
		t.Fatal("intentional block response changed")
	}
	// One service record may carry more hints than a per-record counter sees.
	r = response(t, req, 0)
	hint := &dns.SVCBIPv4Hint{Hint: make([]net.IP, maxAddresses+1)}
	for i := range hint.Hint {
		hint.Hint[i] = net.ParseIP("192.0.2.1")
	}
	r.Msg.Answer = []dns.RR{&dns.SVCB{Hdr: dns.RR_Header{Name: "www.example.", Rrtype: dns.TypeSVCB, Class: dns.ClassINET}, Priority: 1, Target: ".", Value: []dns.SVCBKeyValue{hint}}}
	if s.Guard(req, r) == r {
		t.Fatal("over-budget hints passed unchecked")
	}
}

func TestNXDOMAINOrderingAndAAAAProofRemoval(t *testing.T) {
	s := settings(t, Options{BogusNXDOMAIN: []string{"192.0.2.0/24"}, NXDomainIPv4: []string{"198.51.100.5"}, FilterAAAA: true})
	req := query(dns.TypeA)
	r := response(t, req, 0, "www.example. 60 IN A 192.0.2.1")
	if got := s.Transform(req, r); got.Msg.Rcode != dns.RcodeNameError || len(got.Msg.Answer) != 0 {
		t.Fatal("bogus address NXDOMAIN was overridden")
	}
	r = response(t, req, dns.RcodeNameError, "www.example. 60 IN CNAME missing.example.")
	got := s.Transform(req, r)
	if got.Msg.Rcode != dns.RcodeSuccess || len(got.Msg.Answer) != 2 || got.Msg.Answer[1].Header().Name != "missing.example." || got.Msg.AuthenticatedData {
		t.Fatal(got.Msg)
	}
	r = response(t, query(dns.TypeAAAA), 0, "www.example. 60 IN AAAA 2001:db8::1", "www.example. 60 IN HTTPS 1 . mandatory=ipv6hint ipv4hint=192.0.2.1 ipv6hint=2001:db8::1")
	r.Msg.Answer = append(r.Msg.Answer, &dns.RRSIG{Hdr: dns.RR_Header{Name: "www.example.", Rrtype: dns.TypeRRSIG}})
	got = settings(t, Options{FilterAAAA: true}).Transform(query(dns.TypeAAAA), r)
	if len(got.Msg.Answer) != 1 || strings.Contains(got.Msg.String(), "ipv6hint") || strings.Contains(got.Msg.String(), "mandatory=") || got.Msg.AuthenticatedData || !got.Modified {
		t.Fatal(got.Msg)
	}
	if len(r.Msg.Answer) != 3 {
		t.Fatal("filter mutated source")
	}
}

func TestSettingsValidationAndOverrides(t *testing.T) {
	for _, opts := range []Options{
		{DNS64Prefix: "192.0.2.0/24"}, {DNS64Prefix: "64:ff9b::1/96"}, {DNS64Prefix: "2001:db8::/72"}, {DNS64Prefix: "ff00::/96"},
		{PrivateDomains: []string{"."}}, {PrivateDomains: []string{"*.example"}}, {BogusNXDOMAIN: []string{"192.0.2.1/24"}}, {DNS64ExcludeIPv4: []string{"2001:db8::/32"}},
		{NXDomainIPv4: []string{"2001:db8::1"}}, {NXDomainIPv6: []string{"::ffff:192.0.2.1"}}, {TTL: 86401},
	} {
		if _, err := Compile(opts); err == nil {
			t.Fatalf("accepted %+v", opts)
		}
	}
	s := settings(t, Options{DNS64Prefix: "64:ff9b::/96", FilterAAAA: true})
	empty, off, on := "", false, true
	hi, err := CompileOverrides(&empty, &off)
	if err != nil {
		t.Fatal(err)
	}
	lo, _ := CompileOverrides(nil, &on)
	got := s.With(MergeOverrides([]*Overrides{hi, lo}))
	if got.prefix.IsValid() || got.filterAAAA || !s.prefix.IsValid() || !s.filterAAAA {
		t.Fatal("nullable priority or immutability lost")
	}
}
