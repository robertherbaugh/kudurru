package dhcpsetup

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
)

func hostConfigured(d config.DHCP) bool {
	return d.Failover.Enabled || d.Failover.ClusterID != "" || len(d.Failover.Peers) > 0 || d.ConflictDetection != "" && d.ConflictDetection != "auto" || d.ProbeTimeout != 0 || len(d.NTPServers) > 0 || len(d.DomainSearch) > 0 || len(d.ClasslessRoutes) > 0 || d.BootServer != "" || d.BootServerName != "" || d.BootFile != "" || len(d.BootProfiles) > 0 || len(d.FailoverServers) > 0 || d.Enabled || len(d.Scopes) > 0 || d.Interface != "" || d.ServerIP != "" || d.Subnet != "" ||
		d.RangeStart != "" || d.RangeEnd != "" || d.Router != "" || len(d.DNSServers) > 0 ||
		d.DomainName != "" || d.RelayOnly || len(d.TrustedRelays) > 0 || len(d.Reservations) > 0 || d.RegisterDNS ||
		d.Management != "" && d.Management != "file" ||
		d.LeaseDuration != 0 && d.LeaseDuration != config.Duration(12*time.Hour) ||
		d.DNSTTL != 0 && d.DNSTTL != config.Duration(30*time.Second)
}

func hostScopes(host config.DHCP) ([]config.DHCPScope, error) {
	scopes := cloneScopes(host.Scopes)
	if len(scopes) == 0 {
		scopes = []config.DHCPScope{{Name: "default", Management: host.Management, Interface: host.Interface, ServerIP: host.ServerIP,
			Subnet: host.Subnet, RelayOnly: host.RelayOnly, TrustedRelays: host.TrustedRelays, RangeStart: host.RangeStart,
			RangeEnd: host.RangeEnd, Router: host.Router, DNSServers: host.DNSServers, DomainName: host.DomainName,
			LeaseDuration: host.LeaseDuration, Reservations: host.Reservations, RegisterDNS: host.RegisterDNS, DNSTTL: host.DNSTTL, ConflictDetection: host.ConflictDetection, ProbeTimeout: host.ProbeTimeout, NTPServers: host.NTPServers, DomainSearch: host.DomainSearch, ClasslessRoutes: host.ClasslessRoutes, BootServer: host.BootServer, BootServerName: host.BootServerName, BootFile: host.BootFile, BootProfiles: host.BootProfiles, FailoverServers: host.FailoverServers}}
	}
	normalized, err := validateScopes(context.Background(), host.Enabled, scopes, false)
	if err == nil || host.Enabled {
		return normalized, err
	}
	// Disabled operator configuration may be an incomplete draft. Preserve its
	// fields for UI adoption; every subsequently saved row must validate fully.
	scopes = cloneScopes(scopes)
	for i := range scopes {
		defaultScope(&scopes[i])
	}
	return scopes, nil
}

func defaultScope(scope *config.DHCPScope) {
	if scope.Management == "" {
		scope.Management = "file"
	}
	if scope.LeaseDuration == 0 {
		scope.LeaseDuration = config.Duration(12 * time.Hour)
	}
	if scope.DNSTTL == 0 {
		scope.DNSTTL = config.Duration(30 * time.Second)
	}
}

func validateScopes(ctx context.Context, enabled bool, scopes []config.DHCPScope, apiOnly bool) ([]config.DHCPScope, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(scopes) > maxScopes {
		return nil, fmt.Errorf("%w: at most %d scopes are supported", ErrInvalidConfiguration, maxScopes)
	}
	if len(scopes) == 0 {
		if enabled {
			return nil, fmt.Errorf("%w: enabled DHCP requires at least one scope", ErrInvalidConfiguration)
		}
		return []config.DHCPScope{}, nil
	}
	scopes = cloneScopes(scopes)
	for i := range scopes {
		if apiOnly && scopes[i].Management != "api" {
			return nil, fmt.Errorf("%w: scope %q must use management=api", ErrInvalidConfiguration, scopes[i].Name)
		}
		defaultScope(&scopes[i])
	}
	// Configs checks every scope structurally regardless of enabled, plus name,
	// prefix, interface sharing, and pool/reservation infrastructure rules.
	configs, err := (config.DHCP{Scopes: scopes}).Configs()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfiguration, err)
	}
	if err := dhcp.ValidateTopology(configs); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfiguration, err)
	}
	normalized := make([]config.DHCPScope, 0, len(configs))
	for _, cfg := range configs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		scope := config.DHCPScopeFromConfig(cfg)
		for i := range scope.Reservations {
			mac, _ := net.ParseMAC(scope.Reservations[i].MAC)
			scope.Reservations[i].MAC = mac.String()
		}
		slices.SortFunc(scope.Reservations, func(a, b config.DHCPReservation) int {
			if a.MAC < b.MAC {
				return -1
			}
			if a.MAC > b.MAC {
				return 1
			}
			return 0
		})
		slices.SortFunc(scope.TrustedRelays, func(a, b config.DHCPTrustedRelay) int {
			if n := netip.MustParseAddr(a.SourceIP).Compare(netip.MustParseAddr(b.SourceIP)); n != 0 {
				return n
			}
			return netip.MustParseAddr(a.GatewayIP).Compare(netip.MustParseAddr(b.GatewayIP))
		})
		normalized = append(normalized, scope)
	}
	slices.SortFunc(normalized, func(a, b config.DHCPScope) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return normalized, nil
}
