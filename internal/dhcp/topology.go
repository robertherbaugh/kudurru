package dhcp

import (
	"fmt"
	"net/netip"
)

// ValidateTopology validates complete scope configurations and their shared
// routing and allocation boundaries without accessing interfaces, stores, or
// sockets. Empty input is valid for disabled setup; NewService requires a scope.
// Caller-owned configurations and slices are never modified.
func ValidateTopology(configs []Config) error {
	configs = append([]Config(nil), configs...)
	names := make(map[string]bool, len(configs))
	for i, cfg := range configs {
		cfg = normalizedConfig(cfg)
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("dhcp: scope %q: %w", cfg.Name, err)
		}
		if names[cfg.Name] {
			return fmt.Errorf("dhcp: duplicate scope %q", cfg.Name)
		}
		names[cfg.Name] = true
		configs[i] = cfg
	}
	for i, cfg := range configs {
		for _, other := range configs[:i] {
			if cfg.Subnet.Overlaps(other.Subnet) {
				return fmt.Errorf("dhcp: overlapping scopes %q and %q", cfg.Name, other.Name)
			}
			if cfg.Interface != other.Interface {
				continue
			}
			if cfg.ServerIP != other.ServerIP || cfg.ServerIdentifier != other.ServerIdentifier {
				return fmt.Errorf("dhcp: scopes %q and %q sharing an interface must share server IP", cfg.Name, other.Name)
			}
			if !cfg.RelayOnly && !other.RelayOnly {
				return fmt.Errorf("dhcp: only one direct scope is allowed per interface (%q and %q)", cfg.Name, other.Name)
			}
		}
	}
	type relayKey struct {
		iface string
		TrustedRelay
	}
	type gateway struct {
		ip    netip.Addr
		owner string
	}
	relays := make(map[relayKey]string)
	gatewaySeen := make(map[netip.Addr]bool)
	var gateways []gateway
	for _, cfg := range configs {
		for _, relay := range cfg.TrustedRelays {
			key := relayKey{cfg.Interface, relay}
			if previous, found := relays[key]; found {
				return fmt.Errorf("dhcp: ambiguous trusted relay mapping for scopes %q and %q", previous, cfg.Name)
			}
			relays[key] = cfg.Name
			if !gatewaySeen[relay.GatewayIP] {
				gatewaySeen[relay.GatewayIP] = true
				gateways = append(gateways, gateway{relay.GatewayIP, cfg.Name})
			}
			if relay.ServerIdentifier.IsValid() && !gatewaySeen[relay.ServerIdentifier] {
				gatewaySeen[relay.ServerIdentifier] = true
				gateways = append(gateways, gateway{relay.ServerIdentifier, cfg.Name})
			}
		}
	}
	for _, cfg := range configs {
		// Routers are mutable; only topology exclusions survive live edits.
		excluded := map[netip.Addr]bool{cfg.ServerIP: true}
		if cfg.ServerIdentifier.IsValid() {
			excluded[cfg.ServerIdentifier] = true
		}
		for _, server := range cfg.FailoverServers {
			excluded[server.Address] = true
		}
		for _, relay := range cfg.TrustedRelays {
			excluded[relay.GatewayIP] = true
			if relay.ServerIdentifier.IsValid() {
				excluded[relay.ServerIdentifier] = true
			}
		}
		for _, other := range configs {
			for _, server := range other.FailoverServers {
				if cfg.usable(server.Address) && !excluded[server.Address] {
					return fmt.Errorf("dhcp: scope %q could allocate failover server %s from scope %q", cfg.Name, server.Address, other.Name)
				}
			}
			// Check the entire usable subnet, not just today's pool: mutable pool
			// and reservation edits must never make an infrastructure IP available.
			if cfg.ServerIP != other.ServerIP && cfg.usable(other.ServerIP) {
				return fmt.Errorf("dhcp: scope %q subnet includes another server address %s from scope %q", cfg.Name, other.ServerIP, other.Name)
			}
			if other.ServerIdentifier.IsValid() && !excluded[other.ServerIdentifier] && cfg.usable(other.ServerIdentifier) {
				return fmt.Errorf("dhcp: scope %q subnet includes advertised server address %s from scope %q", cfg.Name, other.ServerIdentifier, other.Name)
			}
		}
		for _, gateway := range gateways {
			if cfg.usable(gateway.ip) && !excluded[gateway.ip] {
				return fmt.Errorf("dhcp: scope %q could allocate relay gateway %s from scope %q", cfg.Name, gateway.ip, gateway.owner)
			}
		}
	}
	return nil
}
