// Package dhcpdns answers from committed DHCP bindings in dedicated local
// namespaces. Specific configured DNS zones take precedence; a catch-all
// forward root permits these answers without changing its data or SOA serials.
package dhcpdns

import (
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/dhcp"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
)

// Resolver reads immutable snapshots; it needs no goroutine or database access.
type Resolver struct {
	stores []*dhcp.LeaseStore
	now    func() time.Time
}

var _ policy.HostnameResolver = (*Resolver)(nil)

// New copies the store registry. Callers retain ownership of the stores.
func New(stores map[string]*dhcp.LeaseStore) *Resolver {
	var names []string
	for name := range stores {
		names = append(names, name)
	}
	slices.Sort(names)
	r := &Resolver{now: time.Now}
	for _, name := range names {
		if stores[name] != nil {
			r.stores = append(r.stores, stores[name])
		}
	}
	return r
}

func (r *Resolver) snapshots() []*dhcp.DNSSnapshot {
	result := make([]*dhcp.DNSSnapshot, 0, len(r.stores))
	for _, store := range r.stores {
		if snapshot := store.DNS(); snapshot != nil && snapshot.Enabled() {
			result = append(result, snapshot)
		}
	}
	return result
}

func ownedDomain(name string, snapshots []*dhcp.DNSSnapshot) string {
	var domain string
	for _, snapshot := range snapshots {
		candidate := snapshot.Domain()
		if len(candidate) > len(domain) && (name == candidate || strings.HasSuffix(name, "."+candidate)) {
			domain = candidate
		}
	}
	return domain
}

func recordForName(name string, snapshots []*dhcp.DNSSnapshot, now time.Time) (dhcp.DNSLease, bool) {
	domain := ownedDomain(name, snapshots)
	var record dhcp.DNSLease
	found := false
	for _, snapshot := range snapshots {
		if snapshot.Domain() != domain {
			continue
		}
		snapshot.ForName(name, func(candidate dhcp.DNSLease) {
			if !now.Before(candidate.ExpiresAt) {
				return
			}
			for _, claims := range snapshots {
				if claims.Domain() != domain {
					continue
				}
				if mac, reserved := claims.ReservationOwner(name); reserved && mac != candidate.MAC {
					return
				}
			}
			if !found || candidate.ClaimEpoch < record.ClaimEpoch || candidate.ClaimEpoch == record.ClaimEpoch && (candidate.ClaimTicket < record.ClaimTicket || candidate.ClaimTicket == record.ClaimTicket && (candidate.ClaimOrdinal < record.ClaimOrdinal || candidate.ClaimOrdinal == record.ClaimOrdinal && candidate.Owner < record.Owner)) {
				record = candidate
				found = true
			}
		})
	}
	return record, found
}

func nameExists(name, domain string, snapshots []*dhcp.DNSSnapshot, now time.Time) bool {
	if name == domain {
		return true
	}
	if _, found := recordForName(name, snapshots, now); found {
		return true
	}
	found := false
	// A large imported set may contain many claims for the same name. Check
	// that name once so an empty-ancestor query stays linear in candidates.
	checked := make(map[string]bool)
	for _, snapshot := range snapshots {
		if snapshot.Domain() != domain {
			continue
		}
		snapshot.ForDescendants(name, func(candidate dhcp.DNSLease) {
			if found || !now.Before(candidate.ExpiresAt) || checked[candidate.Name] {
				return
			}
			checked[candidate.Name] = true
			if _, valid := recordForName(candidate.Name, snapshots, now); valid {
				found = true
			}
		})
	}
	return found
}

func reverseAddress(name string) (netip.Addr, bool) {
	const suffix = ".in-addr.arpa."
	if !strings.HasSuffix(name, suffix) {
		return netip.Addr{}, false
	}
	labels := strings.Split(strings.TrimSuffix(name, suffix), ".")
	if len(labels) != 4 {
		return netip.Addr{}, false
	}
	slices.Reverse(labels)
	address, err := netip.ParseAddr(strings.Join(labels, "."))
	return address, err == nil && address.Is4()
}

func recordForAddress(address netip.Addr, snapshots []*dhcp.DNSSnapshot, now time.Time) (dhcp.DNSLease, bool, bool) {
	owned := false
	for _, snapshot := range snapshots {
		if !snapshot.Subnet().Contains(address) {
			continue
		}
		owned = true
		candidate, found := snapshot.Address(address)
		if !found || !now.Before(candidate.ExpiresAt) {
			continue
		}
		if forward, valid := recordForName(candidate.Name, snapshots, now); valid && forward.Owner == candidate.Owner {
			return candidate, true, true
		}
	}
	return dhcp.DNSLease{}, false, owned
}

func recordTTL(record dhcp.DNSLease, now time.Time) uint32 {
	seconds := max(int64(0), min(int64(time.Hour/time.Second), int64(record.TTL/time.Second), int64(record.ExpiresAt.Sub(now)/time.Second)))
	return uint32(seconds) // #nosec G115 -- clamped to 0..3600 seconds
}

func localSOA(name string) *dns.SOA {
	// Zero negative TTL prevents a cached miss from hiding a new binding.
	return &dns.SOA{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeSOA, Class: dns.ClassINET},
		Ns: ".", Mbox: "."}
}

// ResolveLocal answers only within dedicated DHCP domains and exact IPv4
// reverse owners belonging to a configured scope subnet. false means continue
// through the ordinary DNS pipeline. It runs after specific zone routing;
// a catch-all forward root may also invoke it.
func (r *Resolver) ResolveLocal(req *pipeline.Request) (*pipeline.Response, bool) {
	if req == nil || req.Msg == nil || req.Msg.Opcode != dns.OpcodeQuery || len(req.Msg.Question) != 1 || req.Msg.Question[0].Qclass != dns.ClassINET {
		return nil, false
	}
	snapshots, now := r.snapshots(), r.now()
	q := req.Msg.Question[0]
	name := dns.CanonicalName(q.Name)
	domain := ownedDomain(name, snapshots)
	var record dhcp.DNSLease
	var found, exists, reverse, owned bool
	if domain != "" {
		owned = true
		record, found = recordForName(name, snapshots, now)
		exists = found || nameExists(name, domain, snapshots, now)
	} else if address, valid := reverseAddress(name); valid {
		record, found, owned = recordForAddress(address, snapshots, now)
		reverse, exists = true, found
	}
	if !owned {
		return nil, false
	}
	if q.Qtype == dns.TypeAXFR || q.Qtype == dns.TypeIXFR {
		response := pipeline.Reply(req, dns.RcodeRefused, pipeline.StatusRefusedACL)
		response.Upstream = "dhcp"
		return response, true
	}
	rcode := dns.RcodeSuccess
	if !exists {
		rcode = dns.RcodeNameError
	}
	response := pipeline.Reply(req, rcode, pipeline.StatusLocalRecord)
	response.Upstream, response.Msg.Authoritative = "dhcp", true
	response.Msg.RecursionAvailable = true
	if found && !reverse && (q.Qtype == dns.TypeA || q.Qtype == dns.TypeANY) {
		address := record.Address.As4()
		response.Msg.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: recordTTL(record, now)}, A: address[:]}}
	} else if found && reverse && (q.Qtype == dns.TypePTR || q.Qtype == dns.TypeANY) {
		response.Msg.Answer = []dns.RR{&dns.PTR{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: recordTTL(record, now)}, Ptr: record.Name}}
	} else if !reverse && name == domain && q.Qtype == dns.TypeSOA {
		response.Msg.Answer = []dns.RR{localSOA(domain)}
	} else {
		negativeOwner := domain
		if reverse {
			// Only this exact reverse owner is owned; do not claim its
			// surrounding reverse zone, particularly for non-octet scopes.
			negativeOwner = name
		}
		response.Msg.Ns = []dns.RR{localSOA(negativeOwner)}
	}
	return response, true
}

// Hostnames implements policy.HostnameResolver from the same current bindings.
func (r *Resolver) Hostnames(address netip.Addr) []string {
	record, found, _ := recordForAddress(address, r.snapshots(), r.now())
	if !found {
		return nil
	}
	return []string{strings.TrimSuffix(record.Name, ".")}
}
