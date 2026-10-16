package config

import (
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/robertherbaugh/kudurru/internal/dhcp"
)

// DHCP configures node-local DHCPv4 scopes. Host settings take effect at
// restart; scopes explicitly managed by the API keep live pool options and
// reservations in their lease database.
type DHCP struct {
	Management    string             `koanf:"management" json:"management"`
	RelayOnly     bool               `koanf:"relayOnly" json:"relayOnly"`
	TrustedRelays []DHCPTrustedRelay `koanf:"trustedRelays" json:"trustedRelays"`
	RegisterDNS   bool               `koanf:"registerDNS" json:"registerDNS"`
	DNSTTL        Duration           `koanf:"dnsTTL" json:"dnsTTL"`
	Scopes        []DHCPScope        `koanf:"scopes" json:"scopes"`
	Enabled       bool               `koanf:"enabled" json:"enabled"`
	Interface     string             `koanf:"interface" json:"interface"`
	ServerIP      string             `koanf:"serverIP" json:"serverIP"`
	Subnet        string             `koanf:"subnet" json:"subnet"`
	RangeStart    string             `koanf:"rangeStart" json:"rangeStart"`
	RangeEnd      string             `koanf:"rangeEnd" json:"rangeEnd"`
	Router        string             `koanf:"router" json:"router"`
	DNSServers    []string           `koanf:"dnsServers" json:"dnsServers"`
	DomainName    string             `koanf:"domainName" json:"domainName"`
	LeaseDuration Duration           `koanf:"leaseDuration" json:"leaseDuration"`
	Reservations  []DHCPReservation  `koanf:"reservations" json:"reservations"`
}

// DHCPReservation assigns an address to one six-octet Ethernet MAC.
// Hostname is optional lease metadata and names local records when registerDNS is enabled.
type DHCPReservation struct {
	MAC      string `koanf:"mac" json:"mac"`
	IP       string `koanf:"ip" json:"ip"`
	Hostname string `koanf:"hostname" json:"hostname"`
}

// DHCPTrustedRelay identifies the exact source and gateway addresses of a relay.
type DHCPTrustedRelay struct {
	SourceIP  string `koanf:"sourceIP" json:"sourceIP"`
	GatewayIP string `koanf:"gatewayIP" json:"gatewayIP"`
}

// DHCPScope is a named scope; it cannot be combined with legacy flat fields.
type DHCPScope struct {
	Name          string             `koanf:"name" json:"name"`
	Management    string             `koanf:"management" json:"management"`
	Interface     string             `koanf:"interface" json:"interface"`
	ServerIP      string             `koanf:"serverIP" json:"serverIP"`
	Subnet        string             `koanf:"subnet" json:"subnet"`
	RelayOnly     bool               `koanf:"relayOnly" json:"relayOnly"`
	TrustedRelays []DHCPTrustedRelay `koanf:"trustedRelays" json:"trustedRelays"`
	RangeStart    string             `koanf:"rangeStart" json:"rangeStart"`
	RangeEnd      string             `koanf:"rangeEnd" json:"rangeEnd"`
	Router        string             `koanf:"router" json:"router"`
	DNSServers    []string           `koanf:"dnsServers" json:"dnsServers"`
	DomainName    string             `koanf:"domainName" json:"domainName"`
	LeaseDuration Duration           `koanf:"leaseDuration" json:"leaseDuration"`
	Reservations  []DHCPReservation  `koanf:"reservations" json:"reservations"`
	RegisterDNS   bool               `koanf:"registerDNS" json:"registerDNS"`
	DNSTTL        Duration           `koanf:"dnsTTL" json:"dnsTTL"`
}

// Config preserves the single-scope conversion API. Use Configs for multiple scopes.
func (d DHCP) Config() (dhcp.Config, error) {
	configs, err := d.Configs()
	if err != nil {
		return dhcp.Config{}, err
	}
	if len(configs) != 1 {
		return dhcp.Config{}, errors.New("dhcp: Config requires exactly one scope; use Configs")
	}
	return configs[0], nil
}

// Configs translates and validates scopes without binding network sockets.
func (d DHCP) Configs() ([]dhcp.Config, error) {
	legacy := DHCPScope{Name: "default", Management: d.Management, Interface: d.Interface,
		ServerIP: d.ServerIP, Subnet: d.Subnet, RelayOnly: d.RelayOnly, TrustedRelays: d.TrustedRelays,
		RangeStart: d.RangeStart, RangeEnd: d.RangeEnd, Router: d.Router, DNSServers: d.DNSServers,
		DomainName: d.DomainName, LeaseDuration: d.LeaseDuration, Reservations: d.Reservations,
		RegisterDNS: d.RegisterDNS, DNSTTL: d.DNSTTL}
	scopes := d.Scopes
	if len(scopes) == 0 {
		scopes = []DHCPScope{legacy}
	} else if d.hasLegacyScope() {
		return nil, errors.New("dhcp.scopes: named scopes and legacy flat scope fields are mutually exclusive")
	}
	configs := make([]dhcp.Config, 0, len(scopes))
	for i, scope := range scopes {
		path := "dhcp"
		if len(d.Scopes) > 0 {
			path = fmt.Sprintf("dhcp.scopes[%d]", i)
			if scope.Name == "" {
				return nil, fmt.Errorf("%s.name: required", path)
			}
			if scope.LeaseDuration == 0 {
				scope.LeaseDuration = Duration(12 * time.Hour)
			}
		}
		if scope.Management == "" {
			scope.Management = "file"
		}
		if scope.DNSTTL == 0 {
			scope.DNSTTL = Duration(30 * time.Second)
		}
		cfg, err := scope.runtime(path)
		if err != nil {
			return nil, err
		}
		for _, prev := range configs {
			switch {
			case prev.Name == cfg.Name:
				return nil, fmt.Errorf("%s.name: duplicate scope %q", path, cfg.Name)
			case prev.Subnet.Overlaps(cfg.Subnet):
				return nil, fmt.Errorf("%s.subnet: overlaps scope %q", path, prev.Name)
			case prev.Interface == cfg.Interface && prev.ServerIP != cfg.ServerIP:
				return nil, fmt.Errorf("%s.serverIP: scopes on one interface require the same serverIP", path)
			case prev.Interface == cfg.Interface && !prev.RelayOnly && !cfg.RelayOnly:
				return nil, fmt.Errorf("%s.interface: only one direct scope is allowed per interface", path)
			}
		}
		configs = append(configs, cfg)
	}
	return configs, nil
}

func (d DHCP) hasLegacyScope() bool {
	return d.Interface != "" || d.ServerIP != "" || d.Subnet != "" || d.RangeStart != "" ||
		d.RangeEnd != "" || d.Router != "" || len(d.DNSServers) > 0 || d.DomainName != "" ||
		len(d.Reservations) > 0 || d.RelayOnly || len(d.TrustedRelays) > 0 || d.RegisterDNS ||
		d.Management != "" && d.Management != "file" ||
		d.LeaseDuration != 0 && d.LeaseDuration != Duration(12*time.Hour) ||
		d.DNSTTL != 0 && d.DNSTTL != Duration(30*time.Second)
}

func (d DHCPScope) runtime(path string) (dhcp.Config, error) {
	var errs []error
	addr := func(field, value string) netip.Addr {
		a, err := netip.ParseAddr(value)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s.%s: %w", path, field, err))
		}
		return a
	}
	cfg := dhcp.Config{Name: d.Name, Management: d.Management, Interface: d.Interface,
		ServerIP: addr("serverIP", d.ServerIP), RangeStart: addr("rangeStart", d.RangeStart),
		RangeEnd: addr("rangeEnd", d.RangeEnd), Router: addr("router", d.Router),
		DomainName: d.DomainName, LeaseDuration: d.LeaseDuration.Std(), RelayOnly: d.RelayOnly,
		RegisterDNS: d.RegisterDNS, DNSTTL: d.DNSTTL.Std()}
	var err error
	cfg.Subnet, err = netip.ParsePrefix(d.Subnet)
	if err != nil {
		errs = append(errs, fmt.Errorf("%s.subnet: %w", path, err))
	}
	for i, server := range d.DNSServers {
		cfg.DNSServers = append(cfg.DNSServers, addr(fmt.Sprintf("dnsServers[%d]", i), server))
	}
	for i, r := range d.Reservations {
		cfg.Reservations = append(cfg.Reservations, dhcp.Reservation{MAC: r.MAC, IP: addr(fmt.Sprintf("reservations[%d].ip", i), r.IP), Hostname: r.Hostname})
	}
	for i, r := range d.TrustedRelays {
		cfg.TrustedRelays = append(cfg.TrustedRelays, dhcp.TrustedRelay{SourceIP: addr(fmt.Sprintf("trustedRelays[%d].sourceIP", i), r.SourceIP), GatewayIP: addr(fmt.Sprintf("trustedRelays[%d].gatewayIP", i), r.GatewayIP)})
	}
	if err := errors.Join(errs...); err != nil {
		return dhcp.Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return dhcp.Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func (c *Config) validateDHCP() []error {
	if len(c.DHCP.Scopes) > 0 && c.DHCP.hasLegacyScope() {
		return []error{errors.New("dhcp.scopes: named scopes and legacy flat scope fields are mutually exclusive")}
	}
	if !c.DHCP.Enabled {
		return nil
	}
	var errs []error
	apiManaged := c.DHCP.Management == "api"
	if len(c.DHCP.Scopes) > 0 {
		apiManaged = false
		for _, scope := range c.DHCP.Scopes {
			apiManaged = apiManaged || scope.Management == "api"
		}
	}
	if apiManaged {
		// Persisted API options replace seeds on restart. An environment lock
		// would promise precedence the lease database cannot honor.
		for _, path := range c.EnvLocked {
			switch path {
			case "dhcp.rangeStart", "dhcp.rangeEnd", "dhcp.router", "dhcp.dnsServers",
				"dhcp.domainName", "dhcp.leaseDuration", "dhcp.registerDNS", "dhcp.dnsTTL",
				"dhcp.reservations", "dhcp.scopes":
				errs = append(errs, fmt.Errorf("%s: environment overrides are incompatible with enabled DHCP management=api; remove the environment variable and use a config file seed plus the DHCP API, or set management=file", path))
			}
		}
	}
	if c.Store.Driver != "sqlite" {
		errs = append(errs, errors.New("dhcp.enabled: requires store.driver=sqlite for a standalone node"))
	}
	if _, err := c.DHCP.Configs(); err != nil {
		errs = append(errs, err)
	}
	return errs
}
