package zone

import (
	"net/netip"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestACLFirstMatchWins(t *testing.T) {
	acl, errs := ParseACL([]string{"!10.0.0.2", "10.0.0.0/8", " ! 2001:db8::/32 ", "2001:db8:1::/48", "192.0.2.7", "::ffff:198.51.100.0/120"}, field.NewPath("acl"))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	for addr, want := range map[string]bool{
		"10.0.0.2":           false, // negated, listed first
		"10.0.0.3":           true,
		"10.255.0.1":         true,
		"11.0.0.1":           false, // no match: default deny
		"2001:db8:1::5":      false, // the /32 negation comes first
		"192.0.2.7":          true,  // bare address = /32
		"192.0.2.8":          false,
		"::ffff:10.0.0.3":    true, // v4-mapped clients match v4 entries
		"198.51.100.9":       true, // v4-mapped CIDR entries match v4 clients
		"fe80::1%eth0":       false,
		"2001:db8:ffff::abc": false,
	} {
		if got := acl.Allowed(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: %v, want %v", addr, got, want)
		}
	}
	if _, matched := acl.Match(netip.MustParseAddr("8.8.8.8")); matched {
		t.Error("8.8.8.8 matched nothing")
	}
}

func TestParseACLErrors(t *testing.T) {
	_, errs := ParseACL([]string{"10.0.0.0/8", "10.0.0.0/33", "!", "host.example"}, field.NewPath("spec", "policy", "queryACL"))
	if len(errs) != 3 || errs[0].Field != "spec.policy.queryACL[1]" || errs[1].Field != "spec.policy.queryACL[2]" || errs[2].Field != "spec.policy.queryACL[3]" {
		t.Fatalf("errors: %v", errs)
	}
}

func TestAccessModes(t *testing.T) {
	private := netip.MustParseAddr("192.168.1.10")
	public := netip.MustParseAddr("203.0.113.5")
	ula := netip.MustParseAddr("fd00::1")
	listed := netip.MustParseAddr("198.51.100.1")
	ns := netip.MustParseAddr("192.0.2.53")
	for _, tc := range []struct {
		mode v1alpha1.AccessMode
		want map[netip.Addr]bool
	}{
		{"", map[netip.Addr]bool{public: true, {}: true}},
		{v1alpha1.AccessAllow, map[netip.Addr]bool{public: true, private: true}},
		{v1alpha1.AccessDeny, map[netip.Addr]bool{public: false, private: false}},
		{v1alpha1.AccessAllowOnlyPrivate, map[netip.Addr]bool{public: false, private: true, ula: true, netip.MustParseAddr("127.0.0.1"): true, {}: false}},
		{v1alpha1.AccessAllowOnlyZoneNameServers, map[netip.Addr]bool{ns: true, listed: false}},
		{v1alpha1.AccessUseNetworkACL, map[netip.Addr]bool{listed: true, ns: false, public: false}},
		{v1alpha1.AccessZoneNameServersAndACL, map[netip.Addr]bool{listed: true, ns: true, public: false}},
	} {
		ap, errs := compileAccess(v1alpha1.ZonePolicy{QueryAccess: tc.mode, QueryACL: []string{"198.51.100.0/24"}}, field.NewPath("p"))
		if len(errs) > 0 {
			t.Fatal(errs)
		}
		ap.nsAddrs = []netip.Addr{ns}
		for addr, want := range tc.want {
			if got := ap.allowed(addr); got != want {
				t.Errorf("mode %q, %v: %v, want %v", tc.mode, addr, got, want)
			}
		}
	}
}
