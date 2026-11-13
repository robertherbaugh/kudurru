package dhcp

import (
	"maps"
	"net/netip"
	"strings"
	"time"
)

// DNSLease is one immutable DNS candidate derived from a committed binding.
type DNSLease struct {
	Address                               netip.Addr
	Name                                  string
	MAC                                   string
	Owner                                 string
	ExpiresAt                             time.Time
	TTL                                   time.Duration
	ClaimEpoch, ClaimTicket, ClaimOrdinal uint64
}

// BoundLease is the client identity of one bound lease, whether or not the
// scope registers it in DNS. The identify stage fills a query's MAC from it
// when no inventory source supplied one (D397); the inventory API lists it.
type BoundLease struct {
	Address   netip.Addr
	MAC       string
	Hostname  string
	ExpiresAt time.Time
}

const dnsShards = 256

// DNSSnapshot is immutable. Its lookup methods read memory only and return
// values, never mutable slices or maps owned by the lease store.
type DNSSnapshot struct {
	generation uint64
	scope      string
	enabled    bool
	domain     string
	subnet     netip.Prefix
	byName     [dnsShards]map[string][]DNSLease
	byAddress  [dnsShards]map[netip.Addr]DNSLease
	bound      [dnsShards]map[netip.Addr]BoundLease
	reserved   map[string]string
}

// DNS returns the latest committed DNS snapshot, or nil after Close.
func (s *LeaseStore) DNS() *DNSSnapshot { return s.dns.Load() }

// Generation identifies this committed snapshot within one process lifetime.
func (s *DNSSnapshot) Generation() uint64 { return s.generation }

// Scope names the DHCP scope whose bindings this snapshot holds.
func (s *DNSSnapshot) Scope() string { return s.scope }

// Enabled reports whether the scope registers leases in its dedicated domain.
func (s *DNSSnapshot) Enabled() bool { return s.enabled }

// Domain returns the lower-case absolute dedicated DHCP domain.
func (s *DNSSnapshot) Domain() string { return s.domain }

// Subnet is the only reverse-address namespace the scope owns.
func (s *DNSSnapshot) Subnet() netip.Prefix { return s.subnet }

// ForName calls fn for each candidate at name. Expiry and duplicate resolution
// are deliberately evaluated by the caller using one clock per DNS request.
func (s *DNSSnapshot) ForName(name string, fn func(DNSLease)) {
	for _, l := range s.byName[dnsNameShard(name)][name] {
		fn(l)
	}
}

// ForDescendants enumerates candidates strictly below an ancestor name.
func (s *DNSSnapshot) ForDescendants(name string, fn func(DNSLease)) {
	for _, shard := range s.byName {
		for candidate, leases := range shard {
			if candidate != name && strings.HasSuffix(candidate, "."+name) {
				for _, l := range leases {
					fn(l)
				}
			}
		}
	}
}

// Address returns the DNS candidate of an address, if it has a valid hostname.
func (s *DNSSnapshot) Address(ip netip.Addr) (DNSLease, bool) {
	if !ip.Is4() {
		return DNSLease{}, false
	}
	l, ok := s.byAddress[addressNumber(ip)&(dnsShards-1)][ip]
	return l, ok
}

// Bound returns the identity of the bound lease of an address, including a
// lease without a valid hostname or in a scope that does not register DNS.
// Expiry is evaluated by the caller with one clock per request.
func (s *DNSSnapshot) Bound(ip netip.Addr) (BoundLease, bool) {
	if !ip.Is4() {
		return BoundLease{}, false
	}
	l, ok := s.bound[addressNumber(ip)&(dnsShards-1)][ip]
	return l, ok
}

// ForBound calls fn for every bound lease of the snapshot in no fixed order.
func (s *DNSSnapshot) ForBound(fn func(BoundLease)) {
	for _, shard := range s.bound {
		for _, l := range shard {
			fn(l)
		}
	}
}

// ReservationOwner protects a configured reservation's name even while offline.
// An empty MAC with reserved=true means conflicting reservation claims.
func (s *DNSSnapshot) ReservationOwner(name string) (mac string, reserved bool) {
	mac, reserved = s.reserved[name]
	return mac, reserved
}

func leaseDNSName(hostname, domain string) (string, bool) {
	if hostname == "" || domain == "" || !validHostname(hostname) || !validHostname(domain) {
		return "", false
	}
	hostname = strings.ToLower(strings.TrimSuffix(hostname, "."))
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	if !strings.Contains(hostname, ".") {
		hostname += "." + domain
	}
	if hostname == domain || !strings.HasSuffix(hostname, "."+domain) || !validHostname(hostname) {
		return "", false
	}
	return hostname + ".", true
}

func dnsNameShard(name string) uint32 {
	var h uint32 = 2166136261
	for i := range name {
		h = (h ^ uint32(name[i])) * 16777619
	}
	return h & (dnsShards - 1)
}
func (s *LeaseStore) dnsCandidate(l storedLease) (DNSLease, bool) {
	if l.State != "bound" || l.claimTicket == 0 {
		return DNSLease{}, false
	}
	return DNSLease{Address: l.Address, Name: l.claimName, MAC: l.MAC, Owner: s.cfg.Name + "\x00" + l.key, ExpiresAt: l.ExpiresAt, TTL: s.cfg.DNSTTL, ClaimEpoch: l.claimEpoch, ClaimTicket: l.claimTicket, ClaimOrdinal: l.claimOrdinal}, true
}
func boundLease(l storedLease) (BoundLease, bool) {
	if l.State != "bound" || !l.Address.Is4() || l.MAC == "" {
		return BoundLease{}, false
	}
	return BoundLease{Address: l.Address, MAC: l.MAC, Hostname: l.Hostname, ExpiresAt: l.ExpiresAt}, true
}

func (s *LeaseStore) publishDNS(leases []storedLease) {
	s.generation++
	snapshot := &DNSSnapshot{generation: s.generation, scope: s.cfg.Name, enabled: s.cfg.RegisterDNS && s.cfg.DomainName != "", domain: strings.ToLower(strings.TrimSuffix(s.cfg.DomainName, ".")) + ".", subnet: s.cfg.Subnet, reserved: map[string]string{}}
	for i := range snapshot.byName {
		snapshot.byName[i] = map[string][]DNSLease{}
		snapshot.byAddress[i] = map[netip.Addr]DNSLease{}
		snapshot.bound[i] = map[netip.Addr]BoundLease{}
	}
	for _, r := range s.cfg.Reservations {
		name, ok := leaseDNSName(r.Hostname, s.cfg.DomainName)
		if !ok {
			continue
		}
		if old, exists := snapshot.reserved[name]; exists && old != r.MAC {
			snapshot.reserved[name] = ""
		} else {
			snapshot.reserved[name] = r.MAC
		}
	}
	for _, l := range leases {
		if b, ok := boundLease(l); ok {
			snapshot.bound[addressNumber(b.Address)&(dnsShards-1)][b.Address] = b
		}
		if c, ok := s.dnsCandidate(l); ok {
			shard := dnsNameShard(c.Name)
			snapshot.byName[shard][c.Name] = append(snapshot.byName[shard][c.Name], c)
			snapshot.byAddress[addressNumber(c.Address)&(dnsShards-1)][c.Address] = c
		}
	}
	s.dns.Store(snapshot)
}

// Each updated shard is copied before publication. Previous snapshots and all
// untouched shards remain immutable, so queries need neither locks nor SQL.
func (s *LeaseStore) publishDNSChanges(changed map[netip.Addr]storedLease) {
	previous := s.dns.Load()
	if previous == nil {
		s.publishDNS((&leaseTransaction{rows: s.leaseByIP, changed: map[netip.Addr]storedLease{}, now: time.Time{}}).all())
		return
	}
	next := *previous
	s.generation++
	next.generation = s.generation
	var namesCopied, addressesCopied, boundCopied [dnsShards]bool
	nameShard := func(name string) map[string][]DNSLease {
		h := dnsNameShard(name)
		if !namesCopied[h] {
			next.byName[h] = maps.Clone(next.byName[h])
			namesCopied[h] = true
		}
		return next.byName[h]
	}
	for ip, l := range changed {
		h := addressNumber(ip) & (dnsShards - 1)
		if !addressesCopied[h] {
			next.byAddress[h] = maps.Clone(next.byAddress[h])
			addressesCopied[h] = true
		}
		if !boundCopied[h] {
			next.bound[h] = maps.Clone(next.bound[h])
			boundCopied[h] = true
		}
		delete(next.bound[h], ip)
		if b, ok := boundLease(l); ok && b.Address == ip {
			next.bound[h][ip] = b
		}
		if old, ok := previous.Address(ip); ok {
			shard := nameShard(old.Name)
			remaining := make([]DNSLease, 0, len(shard[old.Name]))
			for _, c := range shard[old.Name] {
				if c.Address != ip {
					remaining = append(remaining, c)
				}
			}
			if len(remaining) == 0 {
				delete(shard, old.Name)
			} else {
				shard[old.Name] = remaining
			}
			delete(next.byAddress[h], ip)
		}
		if candidate, ok := s.dnsCandidate(l); ok {
			shard := nameShard(candidate.Name)
			values := append([]DNSLease(nil), shard[candidate.Name]...)
			shard[candidate.Name] = append(values, candidate)
			next.byAddress[h][ip] = candidate
		}
	}
	s.dns.Store(&next)
}
