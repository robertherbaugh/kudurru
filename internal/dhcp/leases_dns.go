package dhcp

import (
	"net/netip"
	"strings"
	"time"
)

// DNSLease is one immutable DNS candidate derived from a committed binding.
type DNSLease struct {
	Address   netip.Addr
	Name      string
	MAC       string
	Owner     string
	ExpiresAt time.Time
	TTL       time.Duration
}

// DNSSnapshot is immutable. Its lookup methods read memory only and return
// values, never mutable slices or maps owned by the lease store.
type DNSSnapshot struct {
	generation uint64
	enabled    bool
	domain     string
	subnet     netip.Prefix
	byName     map[string][]DNSLease
	byAddress  map[netip.Addr]DNSLease
	below      map[string][]DNSLease
	reserved   map[string]string
}

// DNS returns the latest committed DNS snapshot, or nil after Close.
func (s *LeaseStore) DNS() *DNSSnapshot { return s.dns.Load() }

// Generation identifies this committed snapshot within one process lifetime.
func (s *DNSSnapshot) Generation() uint64 { return s.generation }

// Enabled reports whether the scope registers leases in its dedicated domain.
func (s *DNSSnapshot) Enabled() bool { return s.enabled }

// Domain returns the lower-case absolute dedicated DHCP domain.
func (s *DNSSnapshot) Domain() string { return s.domain }

// Subnet is the only reverse-address namespace the scope owns.
func (s *DNSSnapshot) Subnet() netip.Prefix { return s.subnet }

// ForName calls fn for each candidate at name. Expiry and duplicate resolution
// are deliberately evaluated by the caller using one clock per DNS request.
func (s *DNSSnapshot) ForName(name string, fn func(DNSLease)) {
	for _, l := range s.byName[name] {
		fn(l)
	}
}

// ForDescendants enumerates candidates strictly below an ancestor name.
func (s *DNSSnapshot) ForDescendants(name string, fn func(DNSLease)) {
	for _, l := range s.below[name] {
		fn(l)
	}
}

// Address returns the DNS candidate of an address, if it has a valid hostname.
func (s *DNSSnapshot) Address(ip netip.Addr) (DNSLease, bool) {
	l, ok := s.byAddress[ip]
	return l, ok
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

func (s *LeaseStore) publishDNS(leases []storedLease) {
	s.generation++
	snapshot := &DNSSnapshot{generation: s.generation, enabled: s.cfg.RegisterDNS,
		domain: strings.ToLower(strings.TrimSuffix(s.cfg.DomainName, ".")) + ".", subnet: s.cfg.Subnet,
		byName: map[string][]DNSLease{}, byAddress: map[netip.Addr]DNSLease{},
		below: map[string][]DNSLease{}, reserved: map[string]string{}}
	if s.cfg.DomainName == "" {
		snapshot.enabled = false
	}
	for _, r := range s.cfg.Reservations {
		name, ok := leaseDNSName(r.Hostname, s.cfg.DomainName)
		if !ok {
			continue
		}
		if previous, exists := snapshot.reserved[name]; exists && previous != r.MAC {
			snapshot.reserved[name] = ""
		} else {
			snapshot.reserved[name] = r.MAC
		}
	}
	for _, l := range leases {
		if l.State != "bound" {
			continue
		}
		hostname := l.Hostname
		if r, reserved := s.byMAC[l.MAC]; reserved && r.Hostname != "" {
			hostname = r.Hostname
		}
		name, ok := leaseDNSName(hostname, s.cfg.DomainName)
		if !ok {
			continue
		}
		candidate := DNSLease{Address: l.Address, Name: name, MAC: l.MAC, Owner: s.cfg.Name + "\x00" + l.key,
			ExpiresAt: l.ExpiresAt, TTL: s.cfg.DNSTTL}
		snapshot.byName[name] = append(snapshot.byName[name], candidate)
		snapshot.byAddress[l.Address] = candidate
		for ancestor := name; ; {
			_, ancestor, _ = strings.Cut(ancestor, ".")
			if ancestor == "" || len(ancestor) < len(snapshot.domain) {
				break
			}
			snapshot.below[ancestor] = append(snapshot.below[ancestor], candidate)
			if ancestor == snapshot.domain {
				break
			}
		}
	}
	s.dns.Store(snapshot)
}
