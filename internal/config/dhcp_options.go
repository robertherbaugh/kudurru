package config

import (
	"fmt"
	"net/netip"
	"slices"

	"github.com/robertherbaugh/kudurru/internal/dhcp"
)

// DHCPClasslessRoute advertises an IPv4 destination and its next hop.
type DHCPClasslessRoute struct {
	Destination string `koanf:"destination" json:"destination"`
	Router      string `koanf:"router" json:"router"`
}

// DHCPBootProfile selects boot information by client architecture (option 93).
type DHCPBootProfile struct {
	Architecture   uint16 `koanf:"architecture" json:"architecture"`
	BootServer     string `koanf:"bootServer" json:"bootServer,omitempty"`
	BootServerName string `koanf:"bootServerName" json:"bootServerName,omitempty"`
	BootFile       string `koanf:"bootFile" json:"bootFile,omitempty"`
}

// DHCPScopeServer reserves every failover member's address in a client subnet.
type DHCPScopeServer struct {
	NodeID  string `koanf:"nodeID" json:"nodeID"`
	Address string `koanf:"address" json:"address"`
}

// ApplyExtendedOptions converts the additional client options without applying
// topology. Config.Validate validates their combined wire representation.
func (d DHCPScope) ApplyExtendedOptions(c *dhcp.Config) error {
	addr := func(name, value string) (netip.Addr, error) {
		if value == "" {
			return netip.Addr{}, nil
		}
		ip, err := netip.ParseAddr(value)
		if err != nil || !ip.Is4() {
			return netip.Addr{}, fmt.Errorf("%s: IPv4 address required", name)
		}
		return ip, nil
	}
	var err error
	c.BootServer, err = addr("bootServer", d.BootServer)
	if err != nil {
		return err
	}
	c.BootServerName, c.BootFile = d.BootServerName, d.BootFile
	c.DomainSearch = slices.Clone(d.DomainSearch)
	c.NTPServers, c.ClasslessRoutes, c.BootProfiles = nil, nil, nil
	for _, value := range d.NTPServers {
		ip, err := addr("ntpServers", value)
		if err != nil || !ip.IsValid() {
			return fmt.Errorf("ntpServers: IPv4 address required")
		}
		c.NTPServers = append(c.NTPServers, ip)
	}
	for _, value := range d.ClasslessRoutes {
		destination, err := netip.ParsePrefix(value.Destination)
		if err != nil {
			return fmt.Errorf("classlessRoutes.destination: %w", err)
		}
		router, err := addr("classlessRoutes.router", value.Router)
		if err != nil {
			return err
		}
		c.ClasslessRoutes = append(c.ClasslessRoutes, dhcp.ClasslessRoute{Destination: destination, Router: router})
	}
	for _, value := range d.BootProfiles {
		ip, err := addr("bootProfiles.bootServer", value.BootServer)
		if err != nil {
			return err
		}
		c.BootProfiles = append(c.BootProfiles, dhcp.BootProfile{Architecture: value.Architecture, BootServer: ip, BootServerName: value.BootServerName, BootFile: value.BootFile})
	}
	return nil
}

func optionalDHCPAddress(ip netip.Addr) string {
	if !ip.IsValid() {
		return ""
	}
	return ip.String()
}

// DHCPScopeFromConfig copies a validated runtime scope to its file/API form.
func DHCPScopeFromConfig(c dhcp.Config) DHCPScope {
	d := DHCPScope{Name: c.Name, Management: c.Management, Interface: c.Interface, ServerIP: c.ServerIP.String(), Subnet: c.Subnet.String(),
		RelayOnly: c.RelayOnly, RangeStart: c.RangeStart.String(), RangeEnd: c.RangeEnd.String(), Router: c.Router.String(),
		DomainName: c.DomainName, LeaseDuration: Duration(c.LeaseDuration), RegisterDNS: c.RegisterDNS, DNSTTL: Duration(c.DNSTTL),
		DNSServers: []string{}, Reservations: []DHCPReservation{}, TrustedRelays: []DHCPTrustedRelay{},
		ConflictDetection: c.ConflictDetection, ProbeTimeout: Duration(c.ProbeTimeout),
		DomainSearch: slices.Clone(c.DomainSearch), BootServer: optionalDHCPAddress(c.BootServer), BootServerName: c.BootServerName, BootFile: c.BootFile}
	for _, ip := range c.DNSServers {
		d.DNSServers = append(d.DNSServers, ip.String())
	}
	for _, ip := range c.NTPServers {
		d.NTPServers = append(d.NTPServers, ip.String())
	}
	for _, r := range c.Reservations {
		d.Reservations = append(d.Reservations, DHCPReservation{MAC: r.MAC, IP: r.IP.String(), Hostname: r.Hostname})
	}
	for _, r := range c.TrustedRelays {
		d.TrustedRelays = append(d.TrustedRelays, DHCPTrustedRelay{SourceIP: r.SourceIP.String(), GatewayIP: r.GatewayIP.String(), SourcePort: r.SourcePort, LinkSelection: optionalDHCPAddress(r.LinkSelection), ServerIdentifier: optionalDHCPAddress(r.ServerIdentifier)})
	}
	for _, r := range c.ClasslessRoutes {
		d.ClasslessRoutes = append(d.ClasslessRoutes, DHCPClasslessRoute{Destination: r.Destination.String(), Router: r.Router.String()})
	}
	for _, p := range c.BootProfiles {
		d.BootProfiles = append(d.BootProfiles, DHCPBootProfile{Architecture: p.Architecture, BootServer: optionalDHCPAddress(p.BootServer), BootServerName: p.BootServerName, BootFile: p.BootFile})
	}
	for _, p := range c.FailoverServers {
		d.FailoverServers = append(d.FailoverServers, DHCPScopeServer{NodeID: p.NodeID, Address: p.Address.String()})
	}
	return d
}
