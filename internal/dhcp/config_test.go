package dhcp

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

func leaseTestConfig() Config {
	return Config{
		Interface: "eth0", ServerIP: netip.MustParseAddr("192.0.2.1"),
		Subnet:     netip.MustParsePrefix("192.0.2.0/24"),
		RangeStart: netip.MustParseAddr("192.0.2.10"), RangeEnd: netip.MustParseAddr("192.0.2.12"),
		Router: netip.MustParseAddr("192.0.2.254"), DNSServers: []netip.Addr{netip.MustParseAddr("192.0.2.1")},
		DomainName: "home.example", LeaseDuration: 12 * time.Hour,
	}
}

func TestLeaseConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Config)
	}{
		{"interface missing", func(c *Config) { c.Interface = "" }},
		{"interface invalid", func(c *Config) { c.Interface = "bad\x00if" }},
		{"IPv6 server", func(c *Config) { c.ServerIP = netip.MustParseAddr("2001:db8::1") }},
		{"mapped server", func(c *Config) { c.ServerIP = netip.MustParseAddr("::ffff:192.0.2.1") }},
		{"server outside subnet", func(c *Config) { c.ServerIP = netip.MustParseAddr("198.51.100.1") }},
		{"server network", func(c *Config) { c.ServerIP = c.Subnet.Addr() }},
		{"server broadcast", func(c *Config) { c.ServerIP = netip.MustParseAddr("192.0.2.255") }},
		{"router missing", func(c *Config) { c.Router = netip.Addr{} }},
		{"router multicast", func(c *Config) { c.Router = netip.MustParseAddr("224.0.0.1") }},
		{"subnet host bits", func(c *Config) { c.Subnet = netip.MustParsePrefix("192.0.2.1/24") }},
		{"subnet IPv6", func(c *Config) { c.Subnet = netip.MustParsePrefix("2001:db8::/64") }},
		{"subnet point to point", func(c *Config) { c.Subnet = netip.MustParsePrefix("192.0.2.0/31") }},
		{"range backwards", func(c *Config) { c.RangeStart, c.RangeEnd = c.RangeEnd, c.RangeStart }},
		{"range network", func(c *Config) { c.RangeStart = c.Subnet.Addr() }},
		{"range broadcast", func(c *Config) { c.RangeEnd = netip.MustParseAddr("192.0.2.255") }},
		{"range outside subnet", func(c *Config) { c.RangeEnd = netip.MustParseAddr("198.51.100.5") }},
		{"DNS missing", func(c *Config) { c.DNSServers = nil }},
		{"DNS IPv6", func(c *Config) { c.DNSServers = []netip.Addr{netip.MustParseAddr("::1")} }},
		{"DNS option too large", func(c *Config) { c.DNSServers = make([]netip.Addr, 64) }},
		{"domain label too long", func(c *Config) { c.DomainName = strings.Repeat("a", 64) }},
		{"domain malformed", func(c *Config) { c.DomainName = "bad..example" }},
		{"domain control", func(c *Config) { c.DomainName = "bad\x00.example" }},
		{"lease short", func(c *Config) { c.LeaseDuration = 59 * time.Second }},
		{"lease long", func(c *Config) { c.LeaseDuration = 30*24*time.Hour + time.Second }},
		{"lease fractional", func(c *Config) { c.LeaseDuration += time.Nanosecond }},
		{"reservation bad MAC", func(c *Config) { c.Reservations = []Reservation{{MAC: "00:11:22:33:44:55:66:77", IP: c.RangeStart}} }},
		{"reservation multicast MAC", func(c *Config) { c.Reservations = []Reservation{{MAC: "01:00:00:00:00:01", IP: c.RangeStart}} }},
		{"reservation zero MAC", func(c *Config) { c.Reservations = []Reservation{{MAC: "00:00:00:00:00:00", IP: c.RangeStart}} }},
		{"reservation duplicate MAC", func(c *Config) {
			c.Reservations = []Reservation{{MAC: "02:00:00:AA:00:01", IP: c.RangeStart}, {MAC: "02-00-00-aa-00-01", IP: c.RangeEnd}}
		}},
		{"reservation duplicate IP", func(c *Config) {
			c.Reservations = []Reservation{{MAC: "02:00:00:00:00:01", IP: c.RangeStart}, {MAC: "02:00:00:00:00:02", IP: c.RangeStart}}
		}},
		{"reservation server", func(c *Config) { c.Reservations = []Reservation{{MAC: "02:00:00:00:00:01", IP: c.ServerIP}} }},
		{"reservation router", func(c *Config) { c.Reservations = []Reservation{{MAC: "02:00:00:00:00:01", IP: c.Router}} }},
		{"reservation outside subnet", func(c *Config) {
			c.Reservations = []Reservation{{MAC: "02:00:00:00:00:01", IP: netip.MustParseAddr("198.51.100.4")}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := leaseTestConfig()
			tc.edit(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	for _, duration := range []time.Duration{time.Minute, 12 * time.Hour, 30 * 24 * time.Hour} {
		cfg := leaseTestConfig()
		cfg.LeaseDuration = duration
		cfg.Reservations = []Reservation{{MAC: "02-00-00-AA-00-01", IP: netip.MustParseAddr("192.0.2.100"), Hostname: "printer"}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("valid configuration rejected: %v", err)
		}
	}
}

func TestExpandedConfigValidation(t *testing.T) {
	cfg := relayConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal("relay server outside client subnet rejected", err)
	}
	cfg.RangeStart, cfg.RangeEnd = cfg.TrustedRelays[0].GatewayIP, cfg.TrustedRelays[0].GatewayIP
	if cfg.inPool(cfg.TrustedRelays[0].GatewayIP) {
		t.Fatal("relay gateway allocatable")
	}
	for _, edit := range []func(*Config){
		func(c *Config) { c.Name = "../escape" },
		func(c *Config) { c.Name = "Upper" },
		func(c *Config) { c.Name = strings.Repeat("a", 64) },
		func(c *Config) { c.Name = "-scope" },
		func(c *Config) { c.Management = "unknown" },
		func(c *Config) { c.DNSTTL = time.Millisecond },
		func(c *Config) { c.DNSTTL = time.Hour + time.Second },
		func(c *Config) { c.TrustedRelays = nil },
		func(c *Config) { c.TrustedRelays[0].SourceIP = netip.IPv6Loopback() },
		func(c *Config) { c.TrustedRelays = append(c.TrustedRelays, c.TrustedRelays[0]) },
		func(c *Config) {
			c.Reservations = []Reservation{{MAC: "02:00:00:00:00:01", IP: c.TrustedRelays[0].GatewayIP}}
		},
	} {
		c := normalizedConfig(relayConfig())
		edit(&c)
		if err := c.Validate(); err == nil {
			t.Fatal("invalid expanded config accepted", c)
		}
	}
	original := relayConfig()
	copied := normalizedConfig(original)
	copied.TrustedRelays[0].GatewayIP = netip.MustParseAddr("192.0.2.98")
	if original.TrustedRelays[0].GatewayIP == copied.TrustedRelays[0].GatewayIP {
		t.Fatal("relay mappings aliased")
	}
}
