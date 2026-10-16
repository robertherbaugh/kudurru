// Package dhcp implements the node-local DHCPv4 server and durable leases.
package dhcp

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"
)

// Config describes one DHCPv4 scope and its immutable network topology.
type Config struct {
	Name                 string
	Management           string
	RelayOnly            bool
	TrustedRelays        []TrustedRelay
	RegisterDNS          bool
	DNSTTL               time.Duration
	Interface            string
	ServerIP             netip.Addr
	Subnet               netip.Prefix
	RangeStart, RangeEnd netip.Addr
	Router               netip.Addr
	DNSServers           []netip.Addr
	DomainName           string
	LeaseDuration        time.Duration
	Reservations         []Reservation
	ConflictDetection    string
	ProbeTimeout         time.Duration
	NTPServers           []netip.Addr
	DomainSearch         []string
	ClasslessRoutes      []ClasslessRoute
	BootServer           netip.Addr
	BootServerName       string
	BootFile             string
	BootProfiles         []BootProfile
	FailoverServers      []ScopeServer
}

// ScopeServer reserves a failover member's local server address in this scope.
type ScopeServer struct {
	NodeID  string
	Address netip.Addr
}

// TrustedRelay authorizes an exact source, giaddr, and relay extension tuple.
// SourcePort defaults to 67. Alternate ports require RFC 8357 suboption 19.
// LinkSelection and ServerIdentifier authorize exact RFC 3527/5107 values;
// an unset value rejects that extension. Replies use GatewayIP and SourcePort.
type TrustedRelay struct {
	SourceIP         netip.Addr
	GatewayIP        netip.Addr
	SourcePort       uint16
	LinkSelection    netip.Addr
	ServerIdentifier netip.Addr
}

// normalizedConfig copies caller-owned slices and applies compatible defaults.
func normalizedConfig(c Config) Config {
	if c.Name == "" {
		c.Name = "default"
	}
	if c.Management == "" {
		c.Management = "file"
	}
	if c.DNSTTL == 0 {
		c.DNSTTL = 30 * time.Second
	}
	c.DNSServers = append([]netip.Addr(nil), c.DNSServers...)
	c.Reservations = append([]Reservation(nil), c.Reservations...)
	c.TrustedRelays = append([]TrustedRelay(nil), c.TrustedRelays...)
	for i := range c.TrustedRelays {
		if c.TrustedRelays[i].SourcePort == 0 {
			c.TrustedRelays[i].SourcePort = serverPort
		}
	}
	if c.ConflictDetection == "" {
		c.ConflictDetection = "arp"
		if c.RelayOnly || len(c.TrustedRelays) != 0 {
			c.ConflictDetection = "icmp"
		}
	}
	if c.ProbeTimeout == 0 {
		c.ProbeTimeout = 200 * time.Millisecond
	}
	c.NTPServers = append([]netip.Addr(nil), c.NTPServers...)
	c.DomainSearch = append([]string(nil), c.DomainSearch...)
	c.ClasslessRoutes = append([]ClasslessRoute(nil), c.ClasslessRoutes...)
	c.BootProfiles = append([]BootProfile(nil), c.BootProfiles...)
	c.FailoverServers = append([]ScopeServer(nil), c.FailoverServers...)
	return c
}

// Reservation assigns one usable subnet address to an Ethernet MAC.
type Reservation struct {
	MAC      string
	IP       netip.Addr
	Hostname string
}

// Validate checks configuration without consulting the host's interfaces.
// The socket layer verifies that Interface actually owns ServerIP.
func (c Config) Validate() error {
	c = normalizedConfig(c)
	if len(c.Name) > 63 || c.Name[0] == '-' || c.Name[len(c.Name)-1] == '-' {
		return fmt.Errorf("dhcp: scope name must be 1..63 lowercase letters, digits, or interior hyphens")
	}
	for _, ch := range c.Name {
		valid := ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-'
		if !valid {
			return fmt.Errorf("dhcp: invalid scope name %q", c.Name)
		}
	}
	if c.Management != "file" && c.Management != "api" {
		return fmt.Errorf("dhcp: management must be file or api")
	}
	if c.DNSTTL < time.Second || c.DNSTTL > time.Hour || c.DNSTTL%time.Second != 0 {
		return fmt.Errorf("dhcp: DNS TTL must be integral seconds between 1 second and 1 hour")
	}

	if c.Interface == "" || len(c.Interface) > 15 || strings.ContainsAny(c.Interface, " /:\t\r\n\x00") {
		return fmt.Errorf("dhcp: interface must be a valid Linux interface name")
	}
	if !c.Subnet.IsValid() || !c.Subnet.Addr().Is4() || c.Subnet != c.Subnet.Masked() || c.Subnet.Bits() < 1 || c.Subnet.Bits() > 30 {
		return fmt.Errorf("dhcp: subnet must be a canonical IPv4 prefix with usable host addresses")
	}
	for _, field := range []struct {
		name string
		ip   netip.Addr
	}{{"server IP", c.ServerIP}, {"router", c.Router}, {"range start", c.RangeStart}, {"range end", c.RangeEnd}} {
		relayServer := field.name == "server IP" && c.RelayOnly && usableIPv4(field.ip)
		if !relayServer && !c.usable(field.ip) {
			return fmt.Errorf("dhcp: %s must be a usable IPv4 address in the subnet", field.name)
		}
	}
	if c.RangeStart.Compare(c.RangeEnd) > 0 {
		return fmt.Errorf("dhcp: range start exceeds range end")
	}
	// Do not accept a range whose endpoints surround a non-unicast block.
	// Besides avoiding invalid leases, this bounds allocation work to hosts.
	for _, block := range []string{"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4"} {
		p := netip.MustParsePrefix(block)
		if c.RangeStart.Compare(lastIPv4(p)) <= 0 && c.RangeEnd.Compare(p.Addr()) >= 0 {
			return fmt.Errorf("dhcp: address range intersects unusable network %s", p)
		}
	}
	if len(c.DNSServers) == 0 || len(c.DNSServers) > 63 {
		return fmt.Errorf("dhcp: specify between 1 and 63 DNS servers")
	}
	for _, ip := range c.DNSServers {
		if !usableIPv4(ip) {
			return fmt.Errorf("dhcp: DNS servers must be unicast IPv4 addresses")
		}
	}
	if !validHostname(c.DomainName) {
		return fmt.Errorf("dhcp: invalid domain name")
	}
	if c.LeaseDuration < time.Minute || c.LeaseDuration > 30*24*time.Hour || c.LeaseDuration%time.Second != 0 {
		return fmt.Errorf("dhcp: lease duration must be integral seconds between 1 minute and 30 days")
	}
	if err := c.validateProtocolOptions(); err != nil {
		return err
	}
	relays := map[TrustedRelay]bool{}
	for _, relay := range c.TrustedRelays {
		if !usableIPv4(relay.SourceIP) || !usableIPv4(relay.GatewayIP) {
			return fmt.Errorf("dhcp: relay source and gateway must be unicast IPv4 addresses")
		}
		if relay.SourcePort == clientPort {
			return fmt.Errorf("dhcp: trusted relay source port must not be client port 68")
		}
		if relay.LinkSelection.IsValid() && !c.usable(relay.LinkSelection) {
			return fmt.Errorf("dhcp: relay link selection must be a usable address inside the scope")
		}
		if relay.ServerIdentifier.IsValid() && !usableIPv4(relay.ServerIdentifier) {
			return fmt.Errorf("dhcp: relay server identifier must be a unicast IPv4 address")
		}
		if relays[relay] {
			return fmt.Errorf("dhcp: duplicate trusted relay mapping")
		}
		relays[relay] = true
	}
	if c.RelayOnly && len(c.TrustedRelays) == 0 {
		return fmt.Errorf("dhcp: relay-only scope requires a trusted relay")
	}
	macs, ips := map[string]bool{}, map[netip.Addr]bool{}
	for _, r := range c.Reservations {
		mac, err := canonicalMAC(r.MAC)
		if err != nil {
			return fmt.Errorf("dhcp: reservation: %w", err)
		}
		if macs[mac] || ips[r.IP] {
			return fmt.Errorf("dhcp: duplicate reservation MAC or IP")
		}
		if !c.usable(r.IP) || c.infrastructure(r.IP) {
			return fmt.Errorf("dhcp: reservation IP must be a usable host distinct from server, router, and relay gateways")
		}
		if !validHostname(r.Hostname) {
			return fmt.Errorf("dhcp: invalid reservation hostname")
		}
		macs[mac], ips[r.IP] = true, true
	}
	return nil
}

func usableIPv4(ip netip.Addr) bool {
	return ip.Is4() && ip.IsGlobalUnicast() && ip.As4()[0] != 0
}

func (c Config) usable(ip netip.Addr) bool {
	return usableIPv4(ip) && c.Subnet.Contains(ip) && ip != c.Subnet.Addr() && ip != lastIPv4(c.Subnet)
}

func (c Config) inPool(ip netip.Addr) bool {
	return c.usable(ip) && ip.Compare(c.RangeStart) >= 0 && ip.Compare(c.RangeEnd) <= 0 && !c.infrastructure(ip)
}

// infrastructure addresses must never be allocated, including reservations.
func (c Config) infrastructure(ip netip.Addr) bool {
	if ip == c.ServerIP || ip == c.Router {
		return true
	}
	for _, server := range c.FailoverServers {
		if ip == server.Address {
			return true
		}
	}
	for _, relay := range c.TrustedRelays {
		if ip == relay.GatewayIP || ip == relay.ServerIdentifier {
			return true
		}
	}
	return false
}

func lastIPv4(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	n := binary.BigEndian.Uint32(b[:]) | (^uint32(0) >> p.Bits())
	binary.BigEndian.PutUint32(b[:], n)
	return netip.AddrFrom4(b)
}

func canonicalMAC(s string) (string, error) {
	m, err := net.ParseMAC(s)
	if err != nil || len(m) != 6 || m[0]&1 != 0 || m.String() == "00:00:00:00:00:00" {
		return "", fmt.Errorf("MAC must be a nonzero unicast six-octet Ethernet address")
	}
	return m.String(), nil
}

// normalizeClientHostname accepts advisory names from DHCP clients and dnsmasq
// imports. RFC 2132 permits trailing NULs; other unusable names are omitted
// without preventing address allocation. Operator-supplied names stay strict.
func normalizeClientHostname(s string) string {
	s = strings.TrimRight(s, "\x00")
	if !validHostname(s) {
		return ""
	}
	return s
}

func validHostname(s string) bool {
	if s == "" {
		return true
	}
	s = strings.TrimSuffix(s, ".")
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			allowed := ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-'
			if !allowed {
				return false
			}
		}
	}
	return true
}
