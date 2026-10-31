package unifi

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	uni "github.com/unpoller/unifi/v6"
)

type page[T any] struct {
	Offset, Limit, Count, TotalCount *int
	Data                             *[]T
}
type clientRow struct {
	ID, Name, Type, IPAddress, MACAddress string
	ConnectedAt                           *time.Time
}
type networkRow struct {
	ID, Name, Management string
	Enabled              *bool
	VLANID               *int
}
type addressConfiguration struct {
	HostIPAddress           string
	PrefixLength            *int
	AdditionalHostIPSubnets []string
	InterfaceType           string
	DHCPConfiguration       *struct{ Mode, DomainName string }
}
type networkDetail struct {
	ID, Name, Management                 string
	Enabled                              *bool
	VLANID                               *int
	IPv4Configuration, IPv6Configuration *addressConfiguration
}

func list[T any](ctx context.Context, p *Provider, budget *pollBudget, path string, limit int, id func(T) string) ([]T, error) {
	var out []T
	seen := map[string]bool{}
	total, offset := -1, 0
	for {
		if budget.pages >= maxPages {
			return nil, ErrCapacity
		}
		budget.pages++
		body, err := p.get(ctx, budget, path+"?offset="+strconv.Itoa(offset)+"&limit="+strconv.Itoa(pageSize))
		if err != nil {
			return nil, err
		}
		var paged page[T]
		if err := decode(body, &paged, false); err != nil {
			return nil, err
		}
		if paged.Offset == nil || paged.Limit == nil || paged.Count == nil || paged.TotalCount == nil || paged.Data == nil {
			return nil, ErrInvalid
		}
		if *paged.Offset != offset || *paged.Limit < 1 || *paged.Limit > pageSize || *paged.Count != len(*paged.Data) || *paged.Count > *paged.Limit || *paged.TotalCount < 0 {
			return nil, ErrInvalid
		}
		if *paged.TotalCount > limit {
			return nil, ErrCapacity
		}
		if total == -1 {
			total = *paged.TotalCount
		} else if total != *paged.TotalCount {
			return nil, ErrDrift
		}
		if len(out)+*paged.Count > total || *paged.Count == 0 && offset < total {
			return nil, ErrInvalid
		}
		for _, row := range *paged.Data {
			key := id(row)
			if !validUUID(key) || seen[key] {
				return nil, ErrInvalid
			}
			seen[key] = true
			out = append(out, row)
		}
		offset += *paged.Count
		if offset == total {
			break
		}
	}
	slices.SortFunc(out, func(a, b T) int { return strings.Compare(id(a), id(b)) })
	return out, nil
}

func (p *Provider) identity(ctx context.Context, budget *pollBudget) (uni.IntegrationSite, string, error) {
	body, err := p.get(ctx, budget, "/info")
	if err != nil {
		return uni.IntegrationSite{}, "", err
	}
	var info uni.IntegrationInfo
	if err := decode(body, &info, false); err != nil || len(info.ApplicationVersion) > 64 || strings.ContainsAny(info.ApplicationVersion, " \x00\r\n\t") {
		return uni.IntegrationSite{}, "", ErrInvalid
	}
	parts := strings.Split(info.ApplicationVersion, ".")
	if len(parts) < 2 {
		return uni.IntegrationSite{}, "", ErrInvalid
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil || major < 9 {
		return uni.IntegrationSite{}, "", ErrInvalid
	}
	for _, part := range parts[1:] {
		if part == "" {
			return uni.IntegrationSite{}, "", ErrInvalid
		}
		for _, r := range part {
			if !asciiAlphaNumeric(r) && r != '-' {
				return uni.IntegrationSite{}, "", ErrInvalid
			}
		}
	}
	sites, err := list(ctx, p, budget, "/sites", 256, func(s uni.IntegrationSite) string { return s.ID })
	if err != nil {
		return uni.IntegrationSite{}, "", err
	}
	requested := p.config.Site
	if requested == "" {
		requested = "default"
	}
	var selected *uni.IntegrationSite
	for _, site := range sites {
		if !objectName(site.InternalReference) {
			return uni.IntegrationSite{}, "", ErrInvalid
		}
		if site.ID == requested || site.InternalReference == requested {
			if selected != nil {
				return uni.IntegrationSite{}, "", ErrDrift
			}
			candidate := site
			selected = &candidate
		}
	}
	if selected == nil {
		return uni.IntegrationSite{}, "", ErrInvalid
	}
	if p.config.Legacy && major != 9 && major != 10 {
		return uni.IntegrationSite{}, "", ErrInvalid
	}
	return *selected, info.ApplicationVersion, nil
}

func (p *Provider) networks(ctx context.Context, budget *pollBudget, site uni.IntegrationSite) ([]Network, error) {
	path := "/sites/" + site.ID + "/networks"
	rows, err := list(ctx, p, budget, path, maxNetworks, func(n networkRow) string { return n.ID })
	if err != nil {
		return nil, err
	}
	var out []Network
	for _, row := range rows {
		if row.Enabled == nil || row.VLANID == nil || row.Name == "" || len(row.Name) > 253 || *row.VLANID < 1 || *row.VLANID > 4009 || row.Management != "GATEWAY" && row.Management != "SWITCH" && row.Management != "UNMANAGED" {
			return nil, ErrInvalid
		}
		body, err := p.get(ctx, budget, path+"/"+row.ID)
		if err != nil {
			return nil, err
		}
		var detail networkDetail
		if err := decode(body, &detail, false); err != nil {
			return nil, err
		}
		if detail.ID != row.ID || detail.Name != row.Name || detail.Enabled == nil || *detail.Enabled != *row.Enabled || detail.VLANID == nil || *detail.VLANID != *row.VLANID || detail.Management != row.Management {
			return nil, ErrDrift
		}
		network := Network{ID: row.ID, Name: row.Name, VLAN: *row.VLANID, Enabled: *row.Enabled}
		for family, config := range []*addressConfiguration{detail.IPv4Configuration, detail.IPv6Configuration} {
			if config == nil {
				continue
			}
			if config.HostIPAddress != "" {
				ip, err := netip.ParseAddr(config.HostIPAddress)
				if err != nil || ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() || ip.Is4In6() || ip.Is4() != (family == 0) || config.PrefixLength == nil || *config.PrefixLength <= 0 || *config.PrefixLength > ip.BitLen() {
					return nil, ErrInvalid
				}
				network.Prefixes = append(network.Prefixes, netip.PrefixFrom(ip, *config.PrefixLength).Masked())
			} else if config.PrefixLength != nil {
				return nil, ErrInvalid
			}
			for _, raw := range config.AdditionalHostIPSubnets {
				prefix, err := netip.ParsePrefix(raw)
				if err != nil || prefix.Bits() <= 0 || prefix.Addr().Zone() != "" || prefix.Addr().Is4In6() || prefix.Addr().Is4() != (family == 0) || prefix.Addr().IsUnspecified() || prefix.Addr().IsMulticast() {
					return nil, ErrInvalid
				}
				network.Prefixes = append(network.Prefixes, prefix.Masked())
			}
			if config.DHCPConfiguration != nil && config.DHCPConfiguration.Mode == "SERVER" {
				network.Domain = config.DHCPConfiguration.DomainName
			}
		}
		if len(network.Prefixes) > 64 {
			return nil, ErrCapacity
		}
		slices.SortFunc(network.Prefixes, func(a, b netip.Prefix) int { return strings.Compare(a.String(), b.String()) })
		network.Prefixes = slices.Compact(network.Prefixes)
		out = append(out, network)
	}
	return out, nil
}

func (p *Provider) clients(ctx context.Context, budget *pollBudget, site uni.IntegrationSite, observed time.Time) ([]Device, error) {
	rows, err := list(ctx, p, budget, "/sites/"+site.ID+"/clients", maxClients, func(c clientRow) string { return c.ID })
	if err != nil {
		return nil, err
	}
	var out []Device
	seen := map[string]bool{}
	for _, row := range rows {
		if row.Name == "" || len(row.Name) > 253 || row.ConnectedAt != nil && row.ConnectedAt.After(observed) {
			return nil, ErrInvalid
		}
		// Desired endpoints use metav1.Time's whole-second wire format. Choose
		// the same conservative absolute deadline at initial acquisition, before
		// inventory or DNS proof exists, so persistence never renews or changes it.
		d := Device{ID: site.ID + "/" + row.ID, Name: row.Name, ObservedAt: observed, ExpiresAt: observed.Add(3 * pollInterval(p.config)).Truncate(time.Second)}
		switch row.Type {
		case "WIRED", "WIRELESS":
			mac, err := normalizeMAC(row.MACAddress)
			if err != nil {
				return nil, err
			}
			d.MAC, d.ID = mac, site.ID+"/"+mac
		case "VPN", "TELEPORT":
			if row.MACAddress != "" {
				return nil, ErrInvalid
			}
		default:
			return nil, ErrInvalid
		}
		if seen[d.ID] {
			return nil, ErrDrift
		}
		seen[d.ID] = true
		if row.IPAddress != "" {
			ip, err := netip.ParseAddr(row.IPAddress)
			if err != nil || ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() {
				return nil, ErrInvalid
			}
			d.IPs = []netip.Addr{ip.Unmap()}
		}
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b Device) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (p *Provider) acquire(ctx context.Context) (Observation, Diagnostics, error) {
	budget := &pollBudget{}
	site, version, err := p.identity(ctx, budget)
	if err != nil {
		return Observation{}, Diagnostics{}, err
	}
	observed := p.now()
	networks, err := p.networks(ctx, budget, site)
	if err != nil {
		return Observation{}, Diagnostics{}, err
	}
	clients, err := p.clients(ctx, budget, site, observed)
	if err != nil {
		return Observation{}, Diagnostics{}, err
	}
	second, err := p.clients(ctx, budget, site, observed)
	if err != nil {
		return Observation{}, Diagnostics{}, err
	}
	if !same(clients, second) {
		return Observation{}, Diagnostics{}, ErrDrift
	}
	otherNetworks, err := p.networks(ctx, budget, site)
	if err != nil {
		return Observation{}, Diagnostics{}, err
	}
	otherSite, otherVersion, err := p.identity(ctx, budget)
	if err != nil {
		return Observation{}, Diagnostics{}, err
	}
	if !same(networks, otherNetworks) || site.ID != otherSite.ID || site.InternalReference != otherSite.InternalReference || version != otherVersion {
		return Observation{}, Diagnostics{}, ErrDrift
	}
	diagnostics := Diagnostics{}
	if p.config.Legacy {
		if err := p.enrich(ctx, budget, site, clients, networks, &diagnostics); err != nil {
			return Observation{}, Diagnostics{}, err
		}
	}
	current := map[string]bool{}
	for _, c := range clients {
		current[c.ID] = true
	}
	p.mu.RLock()
	for id, old := range p.previous {
		if !current[id] && !old.ObservedAt.IsZero() && observed.Sub(old.ObservedAt) <= 24*time.Hour {
			old.IPs = nil
			clients = append(clients, old)
		}
	}
	p.mu.RUnlock()
	if len(clients) > maxClients {
		return Observation{}, Diagnostics{}, ErrCapacity
	}
	slices.SortFunc(clients, func(a, b Device) int { return strings.Compare(a.ID, b.ID) })
	return Observation{SiteID: site.ID, ObservedAt: observed, Clients: clients, Networks: networks, Complete: true}, diagnostics, nil
}

func same(a, b any) bool {
	x, err := json.Marshal(a)
	if err != nil {
		return false
	}
	y, err := json.Marshal(b)
	return err == nil && string(x) == string(y)
}
func normalizeMAC(raw string) (string, error) {
	mac, err := net.ParseMAC(raw)
	if err != nil || len(mac) != 6 || mac[0]&1 != 0 || mac.String() == "00:00:00:00:00:00" {
		return "", ErrInvalid
	}
	return mac.String(), nil
}
