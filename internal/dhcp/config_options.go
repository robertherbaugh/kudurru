package dhcp

import (
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// ClasslessRoute is an RFC 3442 destination and next-hop router. When routes
// are configured, include an explicit 0.0.0.0/0 route to advertise a default.
type ClasslessRoute struct {
	Destination netip.Prefix
	Router      netip.Addr
}

// BootProfile overrides the scope's boot settings for one RFC 4578 option 93
// architecture. The first matching architecture in the client's list wins.
type BootProfile struct {
	Architecture   uint16
	BootServer     netip.Addr
	BootServerName string
	BootFile       string
}

func (c Config) validateProtocolOptions() error {
	nodes, addresses := map[string]bool{}, map[netip.Addr]bool{}
	for _, server := range c.FailoverServers {
		if server.NodeID == "" || len(server.NodeID) > 63 || strings.TrimSpace(server.NodeID) != server.NodeID || !usableIPv4(server.Address) || nodes[server.NodeID] || addresses[server.Address] {
			return fmt.Errorf("dhcp: failover servers require unique node IDs and unicast IPv4 addresses")
		}
		nodes[server.NodeID], addresses[server.Address] = true, true
	}
	switch c.ConflictDetection {
	case "arp":
		if c.RelayOnly || len(c.TrustedRelays) != 0 {
			return fmt.Errorf("dhcp: ARP conflict detection requires a direct scope without relays; use icmp or disabled for routed clients")
		}
	case "icmp", "disabled":
	default:
		return fmt.Errorf("dhcp: conflict detection must be arp, icmp, or disabled")
	}
	if c.ProbeTimeout < 50*time.Millisecond || c.ProbeTimeout > 500*time.Millisecond || c.ProbeTimeout%time.Millisecond != 0 {
		return fmt.Errorf("dhcp: probe timeout must be integral milliseconds between 50ms and 500ms")
	}
	if len(c.NTPServers) > 63 {
		return fmt.Errorf("dhcp: at most 63 NTP servers are allowed")
	}
	for _, ip := range c.NTPServers {
		if !usableIPv4(ip) {
			return fmt.Errorf("dhcp: NTP servers must be unicast IPv4 addresses")
		}
	}
	for _, domain := range c.DomainSearch {
		if domain == "" || !validHostname(domain) {
			return fmt.Errorf("dhcp: invalid search domain")
		}
	}
	if len(encodeSearchDomains(c.DomainSearch)) > 255 {
		return fmt.Errorf("dhcp: encoded search domains exceed 255 bytes")
	}
	seen := map[netip.Prefix]bool{}
	for _, route := range c.ClasslessRoutes {
		if !route.Destination.IsValid() || !route.Destination.Addr().Is4() || route.Destination != route.Destination.Masked() || !c.usable(route.Router) {
			return fmt.Errorf("dhcp: classless routes require canonical IPv4 destinations and a usable router inside the scope")
		}
		if seen[route.Destination] {
			return fmt.Errorf("dhcp: duplicate classless route destination")
		}
		seen[route.Destination] = true
	}
	if len(encodeClasslessRoutes(c.ClasslessRoutes)) > 255 {
		return fmt.Errorf("dhcp: encoded classless routes exceed 255 bytes")
	}
	if err := validateBoot(c.BootServer, c.BootServerName, c.BootFile); err != nil {
		return err
	}
	architectures := map[uint16]bool{}
	for _, profile := range c.BootProfiles {
		if architectures[profile.Architecture] {
			return fmt.Errorf("dhcp: duplicate boot architecture")
		}
		architectures[profile.Architecture] = true
		if err := validateBoot(profile.BootServer, profile.BootServerName, profile.BootFile); err != nil {
			return err
		}
	}
	if len(c.BootProfiles) > 128 {
		return fmt.Errorf("dhcp: at most 128 boot profiles are allowed")
	}
	return nil
}

func validateBoot(server netip.Addr, name, file string) error {
	if server.IsValid() && !usableIPv4(server) {
		return fmt.Errorf("dhcp: boot server must be a unicast IPv4 address")
	}
	for _, field := range []struct {
		value string
		limit int
	}{{name, 253}, {file, 127}} {
		if len(field.value) > field.limit {
			return fmt.Errorf("dhcp: boot server name or file is too long")
		}
		for _, ch := range field.value {
			if ch < 33 || ch > 126 {
				return fmt.Errorf("dhcp: boot values must contain printable non-space ASCII")
			}
		}
	}
	return nil
}

func encodeSearchDomains(domains []string) []byte {
	var wire []byte
	for _, domain := range domains {
		for _, label := range strings.Split(strings.TrimSuffix(domain, "."), ".") {
			length := len(label)
			if length < 1 || length > 63 {
				return nil // Invalid names are rejected by configuration validation.
			}
			wire = append(wire, byte(length))
			wire = append(wire, label...)
		}
		wire = append(wire, 0)
	}
	return wire
}

func encodeClasslessRoutes(routes []ClasslessRoute) []byte {
	var wire []byte
	for _, route := range routes {
		bits := route.Destination.Bits()
		if bits < 0 || bits > 32 {
			return nil // Only validated IPv4 prefixes can be encoded.
		}
		wire = append(wire, byte(bits))
		if route.Destination.Addr().Is4() {
			ip := route.Destination.Addr().As4()
			wire = append(wire, ip[:(bits+7)/8]...)
		}
		if route.Router.Is4() {
			ip := route.Router.As4()
			wire = append(wire, ip[:]...)
		}
	}
	return wire
}
