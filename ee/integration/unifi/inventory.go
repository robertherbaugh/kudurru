package unifi

import (
	"encoding/json"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func prepare(in TranslationInput) (TranslationInput, Diagnostics, error) {
	diag := Diagnostics{}
	if err := validateConfig(in.Config); err != nil {
		return in, diag, err
	}
	o := in.Observation
	if in.Owner.Validate() != nil || !o.Complete || !validUUID(o.SiteID) || o.ObservedAt.IsZero() {
		return in, diag, ErrInvalid
	}
	if len(o.Clients) > maxClients || len(o.Networks) > maxNetworks {
		return in, diag, ErrCapacity
	}
	encoded, err := json.Marshal(o)
	if err != nil || len(encoded) > maxSnapshot {
		return in, diag, ErrCapacity
	}
	if in.Config.Inventory != nil {
		z := in.Zone
		if z == nil || z.Name != in.Config.Inventory.ZoneRef.Name || z.Namespace != "" || z.UID == "" || z.ResourceVersion == "" || z.Spec.Type != public.ZoneTypePrimary || public.IsDisabled(z) || !z.DeletionTimestamp.IsZero() {
			return in, diag, ErrInvalid
		}
		if !dnsName(z.Spec.ZoneName) || dns.CanonicalName(z.Spec.ZoneName) == "." {
			return in, diag, ErrInvalid
		}
	}
	o.Networks = slices.Clone(o.Networks)
	seen := map[string]bool{}
	for i, n := range o.Networks {
		if !validUUID(n.ID) || seen[n.ID] || n.Name == "" || len(n.Name) > 253 || n.VLAN < 1 || n.VLAN > 4009 || len(n.Prefixes) > 64 || n.Domain != "" && !dnsName(n.Domain) {
			return in, diag, ErrInvalid
		}
		seen[n.ID] = true
		n.Prefixes = slices.Clone(n.Prefixes)
		for j, prefix := range n.Prefixes {
			if !prefix.IsValid() || prefix.Bits() <= 0 || prefix.Addr().Zone() != "" || prefix.Addr().Is4In6() {
				return in, diag, ErrInvalid
			}
			n.Prefixes[j] = prefix.Masked()
		}
		o.Networks[i] = n
	}
	for name := range in.Config.Groups.ByNetwork {
		if networkCount(name, o.Networks) != 1 {
			return in, diag, ErrInvalid
		}
	}
	if in.Config.Inventory != nil {
		for _, name := range in.Config.Inventory.IncludeNetworks {
			if networkCount(name, o.Networks) != 1 {
				return in, diag, ErrInvalid
			}
		}
	}
	seen = map[string]bool{}
	type claim struct {
		id        string
		at        time.Time
		ambiguous bool
	}
	claims := map[netip.Addr]claim{}
	o.Clients = slices.Clone(o.Clients)
	for i, d := range o.Clients {
		if d.ID == "" || len(d.ID) > 256 || seen[d.ID] || len(d.IPs) > 16 || len(d.Name) > 253 || len(d.Alias) > 253 || len(d.Hostname) > 253 || len(d.LocalDNSName) > 253 || len(d.Note) > 4096 || d.ObservedAt.IsZero() || d.ObservedAt.After(o.ObservedAt) || o.ObservedAt.Sub(d.ObservedAt) > 24*time.Hour || !d.ExpiresAt.After(d.ObservedAt) || d.ExpiresAt.Sub(d.ObservedAt) > 24*time.Hour {
			return in, diag, ErrInvalid
		}
		seen[d.ID] = true
		if d.MAC != "" {
			mac, err := normalizeMAC(d.MAC)
			if err != nil {
				return in, diag, err
			}
			d.MAC = mac
		}
		if d.Hostname != "" && !dnsName(d.Hostname) || d.LocalDNSName != "" && !dnsName(d.LocalDNSName) {
			return in, diag, ErrInvalid
		}
		d.IPs = slices.Clone(d.IPs)
		for j, ip := range d.IPs {
			if !ip.IsValid() || ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() {
				return in, diag, ErrInvalid
			}
			ip = ip.Unmap()
			d.IPs[j] = ip
			old, exists := claims[ip]
			if !exists || d.ObservedAt.After(old.at) {
				claims[ip] = claim{id: d.ID, at: d.ObservedAt}
			} else if d.ObservedAt.Equal(old.at) && d.ID != old.id {
				old.ambiguous = true
				claims[ip] = old
			}
		}
		slices.SortFunc(d.IPs, func(a, b netip.Addr) int { return a.Compare(b) })
		d.IPs = slices.Compact(d.IPs)
		o.Clients[i] = d
	}
	for _, c := range claims {
		if c.ambiguous {
			diag.AmbiguousIPs++
		}
	}
	for i, d := range o.Clients {
		ips := []netip.Addr{}
		for _, ip := range d.IPs {
			c := claims[ip]
			if !c.ambiguous && c.id == d.ID && o.ObservedAt.Before(d.ExpiresAt) {
				ips = append(ips, ip)
			}
		}
		o.Clients[i].IPs = ips
	}
	in.Observation = o
	return in, diag, nil
}

// Translate is pure: inputs and allocations remain immutable; no clock, store,
// resolver, allocation or network callback is invoked.
func Translate(in TranslationInput) (host.SourceSnapshot, Diagnostics, error) {
	in, diag, err := prepare(in)
	if err != nil {
		return host.SourceSnapshot{}, diag, err
	}
	source := host.SourceID(in.Owner)
	out := host.SourceSnapshot{Complete: true}
	for _, d := range in.Observation.Clients {
		client := inventory.Client{ID: d.ID, Source: source, Name: d.Name, Hostname: d.Hostname, IPs: slices.Clone(d.IPs), ObservedAt: d.ObservedAt, ExpiresAt: d.ExpiresAt}
		if d.Alias != "" {
			client.Name = d.Alias
		}
		if d.MAC != "" {
			client.MACs = []string{d.MAC}
		}
		if n := networkFor(d, in.Observation.Networks); n != nil {
			client.Tags = []string{"unifi:network:" + n.ID, "unifi:vlan:" + intString(n.VLAN)}
		} else if len(d.IPs) > 0 {
			diag.UnassignedNetworks++
		}
		out.Clients = append(out.Clients, client)
	}
	verifier := inventory.New(func() time.Time { return in.Observation.ObservedAt })
	if err := verifier.Replace(source, 1, out.Clients); err != nil {
		return host.SourceSnapshot{}, diag, ErrInvalid
	}
	if in.Config.Inventory != nil || len(in.Config.Groups.ByNetwork) > 0 || in.Config.Groups.ByNote != "" {
		out.Desired = &host.DesiredBatch{SourceID: source}
	}
	if in.Config.Inventory != nil {
		requests, err := requests(in)
		if err != nil {
			return host.SourceSnapshot{}, diag, err
		}
		assigned, err := names(requests, in.Allocations)
		if err != nil {
			return host.SourceSnapshot{}, diag, err
		}
		zones := map[string]*public.DNSZone{}
		ttl := in.Config.Inventory.TTL
		if ttl == 0 {
			ttl = 120
		}
		// Coverage follows authenticated selected prefixes, including empty
		// children, so a client move cannot renumber or churn reverse authority.
		if in.Config.Inventory.ReverseZones != "disabled" {
			for _, n := range in.Observation.Networks {
				if !n.Enabled || !included(in.Config.Inventory.IncludeNetworks, &n) {
					continue
				}
				for _, prefix := range n.Prefixes {
					apices, err := reverseApices(prefix)
					if err != nil {
						return host.SourceSnapshot{}, diag, err
					}
					for _, apex := range apices {
						if zones[apex] == nil {
							zones[apex] = reverseZone(in.Owner, apex, uint32(ttl))
							if len(zones) > 256 {
								return host.SourceSnapshot{}, diag, ErrCapacity
							}
						}
					}
				}
			}
		}
		for _, d := range in.Observation.Clients {
			name := assigned[d.ID]
			if name == "" {
				continue
			}
			fqdn := name + "." + dns.CanonicalName(in.Zone.Spec.ZoneName)
			if !dnsName(fqdn) {
				return host.SourceSnapshot{}, diag, ErrInvalid
			}
			rr := recordSet(in.Owner, d.ID, in.Zone.Name)
			for _, ip := range d.IPs {
				typ := "AAAA"
				if ip.Is4() {
					typ = "A"
				}
				rr.Spec.Endpoints = append(rr.Spec.Endpoints, endpointFor(fqdn, typ, ip.String(), ttl, d.ExpiresAt))
			}
			out.Desired.RecordSets = append(out.Desired.RecordSets, rr)
			if in.Config.Inventory.ReverseZones == "disabled" {
				continue
			}
			for _, ip := range d.IPs {
				prefix, ok := reversePrefix(ip, in.Observation.Networks)
				if !ok {
					continue
				}
				apices, err := reverseApices(prefix)
				if err != nil {
					return host.SourceSnapshot{}, diag, err
				}
				ptr, _ := dns.ReverseAddr(ip.String())
				for _, apex := range apices {
					if !dns.IsSubDomain(apex, ptr) {
						continue
					}
					zone := zones[apex]
					if zone == nil {
						continue // prefix was not selected for reverse publication
					}
					reverse := recordSet(in.Owner, d.ID+"/"+ip.String(), zone.Name)
					reverse.Spec.Endpoints = []public.Endpoint{endpointFor(ptr, "PTR", fqdn, ttl, d.ExpiresAt)}
					out.Desired.RecordSets = append(out.Desired.RecordSets, reverse)
				}
			}
		}
		for _, z := range zones {
			out.Desired.Zones = append(out.Desired.Zones, z)
		}
	}
	if out.Desired != nil {
		groups, noMAC, err := groups(in)
		if err != nil {
			return host.SourceSnapshot{}, diag, err
		}
		diag.NoMACGroups = noMAC
		out.Desired.ClientGroups = groups
		slices.SortFunc(out.Desired.Zones, func(a, b *public.DNSZone) int { return strings.Compare(a.Name, b.Name) })
		slices.SortFunc(out.Desired.RecordSets, func(a, b *public.DNSRecordSet) int { return strings.Compare(a.Name, b.Name) })
		if len(out.Desired.RecordSets) > 10000 {
			return host.SourceSnapshot{}, diag, ErrCapacity
		}
	}
	encoded, err := json.Marshal(out)
	if err != nil || len(encoded) > maxSnapshot {
		return host.SourceSnapshot{}, diag, ErrCapacity
	}
	return out, diag, nil
}

func dnsName(name string) bool {
	_, ok := dns.IsDomainName(name)
	return ok && strings.TrimSpace(name) == name && !strings.ContainsAny(name, " \\*\t\r\n\x00")
}
func networkCount(name string, networks []Network) int {
	count := 0
	for _, n := range networks {
		if n.Name == name {
			count++
		}
	}
	return count
}
func included(names []string, network *Network) bool {
	return len(names) == 0 || network != nil && slices.Contains(names, network.Name)
}
func networkFor(d Device, networks []Network) *Network {
	if d.NetworkID != "" {
		for i, n := range networks {
			if n.ID != d.NetworkID || !n.Enabled {
				continue
			}
			for _, ip := range d.IPs {
				if len(n.Prefixes) > 0 {
					contains := false
					for _, prefix := range n.Prefixes {
						contains = contains || prefix.Contains(ip)
					}
					if !contains {
						return nil
					}
				}
			}
			return &networks[i]
		}
		return nil
	}
	var selected *Network
	for _, ip := range d.IPs {
		current, _ := networkForIP(ip, networks)
		if current == nil || selected != nil && selected.ID != current.ID {
			return nil
		}
		selected = current
	}
	return selected
}
func networkForIP(ip netip.Addr, networks []Network) (*Network, netip.Prefix) {
	var selected *Network
	best := netip.Prefix{}
	ambiguous := false
	for i, n := range networks {
		if !n.Enabled {
			continue
		}
		for _, prefix := range n.Prefixes {
			if !prefix.Contains(ip) {
				continue
			}
			if !best.IsValid() || prefix.Bits() > best.Bits() {
				selected, best, ambiguous = &networks[i], prefix, false
			} else if prefix.Bits() == best.Bits() && selected.ID != n.ID {
				ambiguous = true
			}
		}
	}
	if ambiguous {
		return nil, netip.Prefix{}
	}
	return selected, best
}
func reversePrefix(ip netip.Addr, networks []Network) (netip.Prefix, bool) {
	n, prefix := networkForIP(ip, networks)
	return prefix, n != nil
}
func endpointFor(name, typ, target string, ttl int64, expires time.Time) public.Endpoint {
	return public.Endpoint{DNSName: name, RecordType: typ, Targets: []string{target}, RecordTTL: ttl, Kudurru: &public.EndpointExt{ExpiresAt: &metav1.Time{Time: expires}}}
}
func recordSet(owner host.InstanceID, id, zone string) *public.DNSRecordSet {
	return &public.DNSRecordSet{ObjectMeta: metadata(owner, "unifi-rr-"+kinds.Digest([]byte(host.SourceID(owner) + id))[:32]), Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: zone}}}
}
func metadata(owner host.InstanceID, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: owner.Namespace, Annotations: map[string]string{"kudurru.io/integration-source": host.SourceID(owner)}}
}
