package dhcp

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func topologyConfigs() []Config {
	direct := protocolConfig()
	remote := relayConfig()
	remote.Interface, remote.ServerIP = direct.Interface, direct.ServerIP
	remote.Subnet = netip.MustParsePrefix("203.0.113.0/24")
	remote.RangeStart, remote.RangeEnd = netip.MustParseAddr("203.0.113.10"), netip.MustParseAddr("203.0.113.20")
	remote.Router = netip.MustParseAddr("203.0.113.1")
	remote.TrustedRelays[0].GatewayIP = remote.Router
	return []Config{direct, remote}
}

func TestValidateTopology(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func([]Config) []Config
		want string
	}{
		{name: "direct and remote share transit"},
		{name: "disabled empty", edit: func([]Config) []Config { return nil }},
		{name: "invalid scope", edit: func(c []Config) []Config { c[1].Interface = ""; return c }, want: "interface"},
		{name: "normalized duplicate name", edit: func(c []Config) []Config { c[1].Name = "default"; return c }, want: "duplicate scope"},
		{name: "overlap", edit: func(c []Config) []Config {
			c[1] = relayConfig()
			return c
		}, want: "overlapping"},
		{name: "shared interface different server", edit: func(c []Config) []Config {
			c[1].ServerIP = netip.MustParseAddr("198.51.100.2")
			return c
		}, want: "share server IP"},
		{name: "two direct scopes", edit: func(c []Config) []Config {
			c[1].RelayOnly = false
			c[1].ServerIP = netip.MustParseAddr("203.0.113.2")
			return c
		}, want: "share server IP"},
		{name: "ambiguous relay", edit: func(c []Config) []Config {
			c[0].TrustedRelays = append([]TrustedRelay(nil), c[1].TrustedRelays...)
			return c
		}, want: "ambiguous trusted relay"},
		{name: "same relay mapping distinct ingress", edit: func(c []Config) []Config {
			c[0].TrustedRelays = append([]TrustedRelay(nil), c[1].TrustedRelays...)
			c[1].Interface = "transit1"
			return c
		}},
		{name: "foreign server in current pool", edit: func(c []Config) []Config {
			c[1].Interface, c[1].ServerIP = "transit1", c[0].RangeStart
			return c
		}, want: "another server address"},
		{name: "foreign server outside current pool", edit: func(c []Config) []Config {
			c[1].Interface, c[1].ServerIP = "transit1", netip.MustParseAddr("192.0.2.200")
			return c
		}, want: "another server address"},
		{name: "foreign server at mutable router", edit: func(c []Config) []Config {
			c[1].Interface, c[1].ServerIP = "transit1", c[0].Router
			return c
		}, want: "another server address"},
		{name: "foreign gateway in current pool", edit: func(c []Config) []Config {
			c[1].TrustedRelays[0].GatewayIP = c[0].RangeStart
			return c
		}, want: "could allocate relay gateway"},
		{name: "foreign gateway outside current pool", edit: func(c []Config) []Config {
			c[1].TrustedRelays[0].GatewayIP = netip.MustParseAddr("192.0.2.200")
			return c
		}, want: "could allocate relay gateway"},
		{name: "foreign gateway at mutable router", edit: func(c []Config) []Config {
			c[1].TrustedRelays[0].GatewayIP = c[0].Router
			return c
		}, want: "could allocate relay gateway"},
		{name: "foreign gateway at reservation", edit: func(c []Config) []Config {
			ip := netip.MustParseAddr("192.0.2.200")
			c[0].Reservations = []Reservation{{MAC: "02:00:00:00:00:01", IP: ip}}
			c[1].TrustedRelays[0].GatewayIP = ip
			return c
		}, want: "could allocate relay gateway"},
		{name: "foreign gateway is immutable server", edit: func(c []Config) []Config {
			c[1].TrustedRelays[0].GatewayIP = c[0].ServerIP
			return c
		}},
		{name: "foreign gateway is own trusted gateway", edit: func(c []Config) []Config {
			c[1].TrustedRelays[0].GatewayIP = c[0].RangeStart
			c[0].TrustedRelays = []TrustedRelay{{SourceIP: netip.MustParseAddr("198.51.100.3"), GatewayIP: c[0].RangeStart}}
			return c
		}},
		{name: "foreign gateway is subnet network", edit: func(c []Config) []Config {
			c[1].TrustedRelays[0].GatewayIP = c[0].Subnet.Addr()
			return c
		}},
		{name: "foreign gateway is subnet broadcast", edit: func(c []Config) []Config {
			c[1].TrustedRelays[0].GatewayIP = netip.MustParseAddr("192.0.2.255")
			return c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configs := topologyConfigs()
			if tc.edit != nil {
				configs = tc.edit(configs)
			}
			before := make([]Config, len(configs))
			for i, cfg := range configs {
				before[i] = cfg
				before[i].TrustedRelays = append([]TrustedRelay(nil), cfg.TrustedRelays...)
				before[i].Reservations = append([]Reservation(nil), cfg.Reservations...)
				before[i].DNSServers = append([]netip.Addr(nil), cfg.DNSServers...)
			}
			err := ValidateTopology(configs)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("ValidateTopology() = %v, want %q", err, tc.want)
			}
			for i := range configs {
				if !reflect.DeepEqual(configs[i], before[i]) {
					t.Fatal("validation changed caller configuration")
				}
			}
			// Cross-scope validity must not depend on scope declaration order.
			if len(configs) == 2 {
				reversedErr := ValidateTopology([]Config{configs[1], configs[0]})
				if (err == nil) != (reversedErr == nil) {
					t.Fatalf("order changed validity: %v vs %v", err, reversedErr)
				}
			}
		})
	}
}
