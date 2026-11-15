package zone

import (
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func stubFixture(t *testing.T) (*v1alpha1.DNSZone, *StubData) {
	t.Helper()
	dz := dnsZone("example.org.", func(dz *v1alpha1.DNSZone) {
		dz.Spec.Type = v1alpha1.ZoneTypeStub
		dz.Spec.Stub = &v1alpha1.StubSpec{Nameservers: []string{"192.0.2.53"}}
	})
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	var records []dns.RR
	for _, line := range []string{"example.org. 300 IN SOA ns.example.org. admin.example.org. 4294967295 10 3 30 10", "example.org. 20 IN NS ns.example.org.", "ns.example.org. 15 IN A 192.0.2.53", "example.org. 20 IN RRSIG NS 13 2 20 20270101000000 20260101000000 1234 example.org. AQID"} {
		rr, err := dns.NewRR(line)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, rr)
	}
	return dz, &StubData{Records: records, Servers: []StubServer{{Name: "ns.example.org.", Address: netip.MustParseAddrPort("192.0.2.53:5300"), Protocol: "tcp"}}, ConfirmedAt: now, ExpiresAt: now.Add(15 * time.Second)}
}

func TestStubImmutableReferralExpiryAndACL(t *testing.T) {
	dz, data := stubFixture(t)
	dz.Spec.Policy.QueryAccess = v1alpha1.AccessAllowOnlyZoneNameServers
	z, err := BuildStub(dz, data)
	if err != nil {
		t.Fatal(err)
	}
	data.Records[1].Header().Ttl = 999
	data.Servers[0].Name = "bad."
	servers := z.StubServers()
	servers[0].Protocol = "udp"
	if z.Serial() != 4294967295 || z.StubServers()[0].Protocol != "tcp" || z.SecondaryIdentity() != "" || z.StubIdentity() != StubIdentity(dz) {
		t.Fatal("mutable/incorrect stub model")
	}
	if !z.QueryAllowed(netip.MustParseAddr("192.0.2.53")) || z.QueryAllowed(netip.MustParseAddr("192.0.2.2")) {
		t.Fatal("discovered nameserver ACL")
	}
	ns, extra := z.StubReferral(data.ConfirmedAt.Add(5*time.Second), false)
	if len(ns) != 1 || len(extra) != 1 || ns[0].Header().Ttl != 10 || extra[0].Header().Ttl != 10 {
		t.Fatalf("aged referral: %v %v", ns, extra)
	}
	ns[0].Header().Ttl = 77
	signed, _ := z.StubReferral(data.ConfirmedAt, true)
	if len(signed) != 2 || signed[0].Header().Ttl != 15 {
		t.Fatalf("private signed referral: %v", signed)
	}
	if z.RoutingAvailableAt(data.ExpiresAt) || z.RoutingAvailableAt(data.ConfirmedAt.Add(-time.Second)) || z.TransferAllowed(netip.MustParseAddr("192.0.2.53"), "") {
		t.Fatal("expired/future/transferable stub")
	}
}

func TestStubRejectsInvalidDelegation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*StubData)
	}{
		{"expiry", func(d *StubData) { d.ExpiresAt = d.ConfirmedAt.Add(16 * time.Second) }},
		{"unadvertised-server", func(d *StubData) { d.Servers[0].Name = "other.example.org." }},
		{"tls", func(d *StubData) { d.Servers[0].Protocol = "tls" }},
		{"no-servers", func(d *StubData) { d.Servers = nil }},
		{"out-of-zone", func(d *StubData) { d.Records[2].Header().Name = "elsewhere." }},
		{"non-glue", func(d *StubData) { d.Records[2].Header().Name = "www.example.org." }},
		{"record-bound", func(d *StubData) {
			for len(d.Records) <= MaxStubRecords {
				d.Records = append(d.Records, d.Records[2])
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dz, data := stubFixture(t)
			test.change(data)
			if _, err := BuildStub(dz, data); err == nil {
				t.Fatal("accepted invalid stub")
			}
		})
	}
	dz, _ := stubFixture(t)
	dz.Spec.Policy.QueryAccess = v1alpha1.AccessUseNetworkACL
	dz.Spec.Policy.QueryACL = []string{"garbage"}
	z, err := BuildStubPlaceholder(dz)
	if err != nil || z.RoutingAvailableAt(time.Now()) || z.QueryAllowed(netip.MustParseAddr("192.0.2.53")) {
		t.Fatalf("placeholder must retain deny-all coverage: %v", err)
	}
}

func TestForwardDNSSECOverrideIsNullableAndCopied(t *testing.T) {
	for _, value := range []*bool{nil, new(true), new(false)} {
		dz := dnsZone("example.org.", func(dz *v1alpha1.DNSZone) {
			dz.Spec.Type = v1alpha1.ZoneTypeForward
			dz.Spec.Forward = &v1alpha1.ForwardSpec{DNSSECValidation: value}
		})
		z := mustBuild(t, dz)
		got := z.DNSSECValidation()
		if value == nil {
			if got != nil {
				t.Fatal("nil override lost")
			}
			continue
		}
		want := *value
		*value = !*value
		if got == nil || *got != want {
			t.Fatal("override not copied at build")
		}
		*got = !*got
		if *z.DNSSECValidation() != want {
			t.Fatal("accessor leaked mutable override")
		}
	}
}
