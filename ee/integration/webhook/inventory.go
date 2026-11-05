package webhook

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/inventory"
)

// This fixed local maximum is measured from the genuine remote LastSeen, never
// the poll or a restored image. It grants no lifetime beyond the original host
// request/source/credential gates and is not a configurable fallback.
const inventoryLifetime = 3 * time.Minute

// Observe accepts only the version-one complete-array contract. Both reads use
// the same original operation. Network references must agree exactly; this does
// not claim a native atomic remote database snapshot. No desired DNS/group write
// is produced, and no local previous poll can manufacture durable acceptance.
func (p *provider) Observe(ctx context.Context) (host.SourceSnapshot, error) {
	tr, a, err := p.captured(ctx, "clients")
	if err != nil {
		return host.SourceSnapshot{}, err
	}
	clientsReply, err := tr.request(ctx, http.MethodGet, "/clients", a.media, nil, nil)
	if err != nil {
		return host.SourceSnapshot{}, err
	}
	var clients []*integration.Client
	if !p.completeArray(clientsReply, a.media) || decode(clientsReply.body, &clients) != nil || len(clients) > maxItems {
		return host.SourceSnapshot{}, errProtocol
	}
	for _, c := range clients {
		if c == nil {
			return host.SourceSnapshot{}, errProtocol
		}
	}
	networksReply, err := tr.request(ctx, http.MethodGet, "/networks", a.media, nil, nil)
	if err != nil {
		return host.SourceSnapshot{}, err
	}
	var networks []*integration.Network
	if !p.completeArray(networksReply, a.media) || decode(networksReply.body, &networks) != nil || len(networks) > maxItems {
		return host.SourceSnapshot{}, errProtocol
	}
	at := p.now()
	out, err := translateInventory(host.SourceID(p.owner), clients, networks, at)
	if err != nil {
		return host.SourceSnapshot{}, err
	}
	if _, _, err := p.captured(ctx, "clients"); err != nil {
		return host.SourceSnapshot{}, err
	}
	return host.SourceSnapshot{Clients: out, Complete: true}, nil
}

func (p *provider) completeArray(r response, media string) bool {
	return r.status == http.StatusOK && r.media == media && !r.partial && arrayJSON(r.body) && validJSON(r.body) == nil && !p.echo(r.body)
}

func translateInventory(source string, clients []*integration.Client, networks []*integration.Network, at time.Time) ([]inventory.Client, error) {
	if source == "" || at.IsZero() || len(clients) > maxItems || len(networks) > maxItems {
		return nil, errProtocol
	}
	byName := make(map[string]integration.Network, len(networks))
	for _, n := range networks {
		if n == nil || !validNetwork(*n) {
			return nil, errProtocol
		}
		if _, exists := byName[n.Name]; exists {
			return nil, errProtocol
		}
		byName[n.Name] = *n
	}
	out := make([]inventory.Client, 0, len(clients))
	seen := make(map[string]bool, len(clients))
	for _, c := range clients {
		if c == nil || !textBound(c.ID, 256) || seen[c.ID] || len(c.Name) > 253 || len(c.Hostname) > 253 || len(c.IPs) > 16 || len(c.MACs) > 16 || len(c.Tags) > 64 || len(c.Attributes) > 64 || c.LastSeen.IsZero() || c.LastSeen.After(at) || at.Sub(c.LastSeen) > 24*time.Hour {
			return nil, errProtocol
		}
		seen[c.ID] = true
		if c.Network != (integration.Network{}) {
			if n, exists := byName[c.Network.Name]; !exists || n != c.Network {
				return nil, errProtocol
			}
		}
		for k, v := range c.Attributes {
			if !textBound(k, 128) || len(v) > 1024 {
				return nil, errProtocol
			}
		}
		entry := inventory.Client{Source: source, ID: c.ID, Name: c.Name, Hostname: c.Hostname,
			ObservedAt: c.LastSeen.UTC(), ExpiresAt: c.LastSeen.Add(inventoryLifetime).UTC(),
			IPs: []netip.Addr{}, MACs: slices.Clone(c.MACs), Tags: slices.Clone(c.Tags)}
		for _, raw := range c.IPs {
			ip, err := netip.ParseAddr(raw)
			if err != nil || ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() {
				return nil, errProtocol
			}
			entry.IPs = append(entry.IPs, ip.Unmap())
		}
		if c.FixedIP != "" {
			ip, err := netip.ParseAddr(c.FixedIP)
			if err != nil || ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() {
				return nil, errProtocol
			}
			// FixedIP describes desired assignment, not evidence it was observed.
		}
		for i, raw := range entry.MACs {
			mac, err := net.ParseMAC(raw)
			if err != nil || len(mac) != 6 || mac[0]&1 != 0 {
				return nil, errProtocol
			}
			entry.MACs[i] = mac.String()
		}
		out = append(out, entry)
	}
	// Reuse the existing owner validator in an isolated holder. This validates
	// normalization/lifetimes only and does not supply a source commit or receipt.
	verifier := inventory.New(func() time.Time { return at })
	if err := verifier.Replace(source, 1, out); err != nil {
		return nil, errProtocol
	}
	return verifier.Snapshot().Clients(), nil
}

func validNetwork(n integration.Network) bool {
	if !textBound(n.Name, 253) || n.VLAN < 0 || n.VLAN > 4094 || len(n.CIDR) > 64 || len(n.Domain) > 253 {
		return false
	}
	if n.CIDR != "" {
		prefix, err := netip.ParsePrefix(n.CIDR)
		if err != nil || prefix.Masked() != prefix {
			return false
		}
	}
	if n.Domain != "" {
		if _, ok := dns.IsDomainName(n.Domain); !ok || strings.ContainsAny(n.Domain, " \\*\t\r\n\x00") {
			return false
		}
	}
	return true
}
