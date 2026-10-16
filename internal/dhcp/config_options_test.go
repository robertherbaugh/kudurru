package dhcp

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestProtocolConfigValidationAndCopy(t *testing.T) {
	for name, edit := range map[string]func(*Config){
		"unknown probe": func(c *Config) { c.ConflictDetection = "command" },
		"ARP routed": func(c *Config) {
			c.ConflictDetection = "arp"
			c.TrustedRelays = []TrustedRelay{{SourceIP: c.Router, GatewayIP: c.Router}}
		},
		"short probe":    func(c *Config) { c.ProbeTimeout = 49 * time.Millisecond },
		"long probe":     func(c *Config) { c.ProbeTimeout = 501 * time.Millisecond },
		"fraction probe": func(c *Config) { c.ProbeTimeout = 100*time.Millisecond + time.Nanosecond },
		"NTP IPv6":       func(c *Config) { c.NTPServers = []netip.Addr{netip.IPv6Loopback()} },
		"NTP many":       func(c *Config) { c.NTPServers = make([]netip.Addr, 64) },
		"search empty":   func(c *Config) { c.DomainSearch = []string{""} },
		"search invalid": func(c *Config) { c.DomainSearch = []string{"bad..example"} },
		"search long":    func(c *Config) { c.DomainSearch = []string{strings.Repeat("a.", 125) + "example"} },
		"route hostbits": func(c *Config) {
			c.ClasslessRoutes = []ClasslessRoute{{Destination: netip.MustParsePrefix("10.0.0.1/8"), Router: c.Router}}
		},
		"route missing": func(c *Config) { c.ClasslessRoutes = []ClasslessRoute{{Router: c.Router}} },
		"route outside": func(c *Config) {
			c.ClasslessRoutes = []ClasslessRoute{{Destination: netip.MustParsePrefix("0.0.0.0/0"), Router: netip.MustParseAddr("198.51.100.1")}}
		},
		"boot IPv6":      func(c *Config) { c.BootServer = netip.IPv6Loopback() },
		"boot long":      func(c *Config) { c.BootFile = strings.Repeat("a", 128) },
		"boot control":   func(c *Config) { c.BootServerName = "host\nexample" },
		"boot duplicate": func(c *Config) { c.BootProfiles = []BootProfile{{Architecture: 7}, {Architecture: 7}} },
		"relay port68": func(c *Config) {
			c.TrustedRelays = []TrustedRelay{{SourceIP: c.Router, GatewayIP: c.Router, SourcePort: 68}}
		},
		"relay wrong link": func(c *Config) {
			c.TrustedRelays = []TrustedRelay{{SourceIP: c.Router, GatewayIP: c.Router, LinkSelection: netip.MustParseAddr("198.51.100.1")}}
		},
		"relay invalid override": func(c *Config) {
			c.TrustedRelays = []TrustedRelay{{SourceIP: c.Router, GatewayIP: c.Router, ServerIdentifier: netip.IPv6Loopback()}}
		},
		"failover duplicate": func(c *Config) {
			c.FailoverServers = []ScopeServer{{NodeID: "a", Address: c.ServerIP}, {NodeID: "a", Address: c.Router}}
		},
		"failover address duplicate": func(c *Config) {
			c.FailoverServers = []ScopeServer{{NodeID: "a", Address: c.ServerIP}, {NodeID: "b", Address: c.ServerIP}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := protocolConfig()
			edit(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("invalid protocol options accepted")
			}
		})
	}
	c := protocolConfig()
	c.ConflictDetection = ""
	if n := normalizedConfig(c); n.ConflictDetection != "arp" || n.ProbeTimeout != 200*time.Millisecond {
		t.Fatal("direct probe defaults")
	}
	c.RelayOnly = true
	c.TrustedRelays = []TrustedRelay{{SourceIP: c.Router, GatewayIP: c.Router}}
	if n := normalizedConfig(c); n.ConflictDetection != "icmp" || n.TrustedRelays[0].SourcePort != 67 {
		t.Fatal("routed probe/relay port defaults")
	}
	c.NTPServers = []netip.Addr{c.ServerIP}
	c.DomainSearch = []string{"home.example"}
	c.ClasslessRoutes = []ClasslessRoute{{Destination: netip.MustParsePrefix("0.0.0.0/0"), Router: c.Router}}
	c.BootProfiles = []BootProfile{{Architecture: 7, BootFile: "boot.efi"}}
	c.FailoverServers = []ScopeServer{{NodeID: "other", Address: c.RangeStart}}
	n := normalizedConfig(c)
	n.NTPServers[0] = c.Router
	n.DomainSearch[0] = "changed.example"
	n.ClasslessRoutes[0].Router = c.ServerIP
	n.BootProfiles[0].BootFile = "changed"
	n.FailoverServers[0].NodeID = "changed"
	if c.NTPServers[0] != c.ServerIP || c.DomainSearch[0] != "home.example" || c.ClasslessRoutes[0].Router != c.Router || c.BootProfiles[0].BootFile != "boot.efi" || c.FailoverServers[0].NodeID != "other" {
		t.Fatal("normalized protocol config aliases caller")
	}
	if c.inPool(c.RangeStart) {
		t.Fatal("failover server address allocatable")
	}
}

func TestFailoverAndOverrideTopologyExclusions(t *testing.T) {
	for _, kind := range []string{"failover", "override"} {
		scopes := topologyConfigs()
		if kind == "failover" {
			scopes[1].FailoverServers = []ScopeServer{{NodeID: "other", Address: scopes[0].RangeStart}}
		} else {
			scopes[1].TrustedRelays[0].ServerIdentifier = scopes[0].RangeStart
		}
		if err := ValidateTopology(scopes); err == nil {
			t.Fatal("foreign infrastructure not excluded", kind)
		}
	}
	// Distinct link selectors authorize one relay source/giaddr for two subnets.
	scopes := topologyConfigs()
	scopes[0].TrustedRelays = []TrustedRelay{{SourceIP: scopes[1].TrustedRelays[0].SourceIP, GatewayIP: scopes[1].TrustedRelays[0].GatewayIP, LinkSelection: scopes[0].Router}}
	scopes[1].TrustedRelays[0].LinkSelection = scopes[1].Router
	if err := ValidateTopology(scopes); err != nil {
		t.Fatal("distinct link selectors rejected", err)
	}
}
