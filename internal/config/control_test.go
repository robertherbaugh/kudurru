package config

import (
	"fmt"
	"net/netip"
	"slices"
	"testing"

	"github.com/miekg/dns"
)

func TestControlConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		control Control
		valid   bool
	}{
		{"empty", Control{}, true},
		{"canonical", Control{AllowedNetworks: []string{"192.0.2.123/24", "2001:db8::123/64", "::ffff:192.0.2.123/120"}, BlockedQTypes: []string{"a", "AAAA", "hinfo", "TYPE65280", "TYPE65535"}}, true},
		{"mapped all IPv4", Control{AllowedNetworks: []string{"::ffff:0.0.0.0/96"}}, true},
		{"invalid CIDR", Control{AllowedNetworks: []string{"192.0.2.1"}}, false},
		{"ambiguous mapped", Control{AllowedNetworks: []string{"::ffff:192.0.2.1/80"}}, false},
		{"invalid zone", Control{AllowedNetworks: []string{"fe80::1%eth0/64"}}, false},
		{"unknown mnemonic", Control{BlockedQTypes: []string{"NOTATYPE"}}, false},
		{"zero", Control{BlockedQTypes: []string{"TYPE0"}}, false},
		{"large", Control{BlockedQTypes: []string{"TYPE65536"}}, false},
		{"negative", Control{BlockedQTypes: []string{"TYPE-1"}}, false},
		{"signed", Control{BlockedQTypes: []string{"TYPE+1"}}, false},
		{"bare number", Control{BlockedQTypes: []string{"1"}}, false},
		{"empty type", Control{BlockedQTypes: []string{""}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flat := Defaults()
			flat["control.allowedNetworks"] = tc.control.AllowedNetworks
			flat["control.blockedQTypes"] = tc.control.BlockedQTypes
			cfg, err := FromFlat(flat)
			if (err == nil) != tc.valid {
				t.Fatalf("control config admission mismatch: accepted=%t", err == nil)
			}
			if err != nil {
				return
			}
			rules, err := cfg.Control.Rules()
			if err != nil {
				t.Fatal("accepted control did not compile")
			}
			if tc.name == "canonical" && (!slices.Equal(rules.AllowedNetworks, []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("2001:db8::/64"), netip.MustParsePrefix("192.0.2.0/24")}) || !slices.Equal(rules.BlockedQTypes, []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeHINFO, 65280, 65535})) {
				t.Fatal("control canonicalization changed network/type meaning")
			}
			restored, err := FromFlat(Flatten(cfg))
			if err != nil || !slices.Equal(restored.Control.AllowedNetworks, cfg.Control.AllowedNetworks) || !slices.Equal(restored.Control.BlockedQTypes, cfg.Control.BlockedQTypes) {
				t.Fatal("control settings did not round trip")
			}
		})
	}
	for _, count := range []int{1024, 1025} {
		c := Control{AllowedNetworks: make([]string, count)}
		for i := range c.AllowedNetworks {
			c.AllowedNetworks[i] = "192.0.2.0/24"
		}
		if _, err := c.Rules(); (err == nil) != (count == 1024) {
			t.Fatal("network count bound incorrect")
		}
	}
	for _, count := range []int{256, 257} {
		c := Control{BlockedQTypes: make([]string, count)}
		for i := range c.BlockedQTypes {
			c.BlockedQTypes[i] = fmt.Sprintf("TYPE%d", i+1)
		}
		if _, err := c.Rules(); (err == nil) != (count == 256) {
			t.Fatal("query type count bound incorrect")
		}
	}
	for _, path := range []string{"control.allowedNetworks", "control.blockedQTypes"} {
		key, ok := Lookup(path)
		if !ok || key.Scope != ScopeCluster || key.HostOnly || key.RestartRequired {
			t.Fatal("control key missing cluster hot-reload metadata")
		}
	}
}
