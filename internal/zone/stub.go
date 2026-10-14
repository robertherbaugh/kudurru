package zone

import (
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// Stub discovery and durable restore share these strict delegation bounds.
const (
	MaxStubRecords     = 256
	MaxStubBytes       = 256 << 10
	MaxStubServers     = 32
	MaxStubNameservers = 16
)

// StubServer is a pre-resolved authority. Queries never resolve its Name.
type StubServer struct {
	Name     string         `json:"name"`
	Address  netip.AddrPort `json:"address"`
	Protocol string         `json:"protocol"`
}

// StubData is a complete discovery result. BuildStub privately copies it.
type StubData struct {
	Records                []dns.RR
	Servers                []StubServer
	ConfirmedAt, ExpiresAt time.Time
}

type stubRoute struct {
	identity           string
	servers            []StubServer
	records            []dns.RR
	confirmed, expires time.Time
}

// StubIdentity binds discovered data to the full configuration and object UID.
func StubIdentity(dz *v1alpha1.DNSZone) string { return SecondaryIdentity(dz) }

// BuildStubPlaceholder retains coverage even when query policy is invalid.
func BuildStubPlaceholder(dz *v1alpha1.DNSZone) (*Zone, error) {
	if dz == nil || dz.Spec.Type != v1alpha1.ZoneTypeStub {
		return nil, fmt.Errorf("stub zone configuration is required")
	}
	cfg := dz.DeepCopy()
	cfg.Spec.Type = v1alpha1.ZoneTypeSecondary
	z, err := BuildSecondaryPlaceholder(cfg)
	if err != nil {
		return nil, err
	}
	z.typ, z.secondaryIdentity = v1alpha1.ZoneTypeStub, ""
	z.stub = &stubRoute{identity: StubIdentity(dz)}
	return z, nil
}

// BuildStub builds immutable delegation/routing data, never a local authority.
// Only apex SOA/NS, advertised in-bailiwick address glue and their raw RRSIGs
// are accepted. Absolute validity cannot exceed SOA expire or record TTLs.
func BuildStub(dz *v1alpha1.DNSZone, data *StubData) (*Zone, error) {
	if data == nil {
		return BuildStubPlaceholder(dz)
	}
	if dz == nil || dz.Spec.Type != v1alpha1.ZoneTypeStub {
		return nil, fmt.Errorf("stub zone configuration is required")
	}
	if data.ConfirmedAt.IsZero() || !data.ExpiresAt.After(data.ConfirmedAt) || len(data.Servers) == 0 || len(data.Servers) > MaxStubServers || len(data.Records) > MaxStubRecords {
		return nil, fmt.Errorf("invalid stub timestamps or delegation bounds")
	}
	// Check cumulative expanded size before allocating a full immutable tree.
	size := 0
	for _, rr := range data.Records {
		_, wire, err := copySecondaryRR(rr)
		if err != nil {
			return nil, err
		}
		size += len(wire)
		if size > MaxStubBytes {
			return nil, fmt.Errorf("stub delegation exceeds byte bound")
		}
	}
	cfg := dz.DeepCopy()
	cfg.Spec.Type = v1alpha1.ZoneTypeSecondary
	z, err := BuildSecondary(cfg, data.Records, data.ExpiresAt)
	if err != nil {
		return nil, err
	}
	hosts := map[string]bool{}
	glue := map[string][]netip.Addr{}
	for _, ns := range z.NameServers() {
		if _, ok := dns.IsDomainName(ns.Host); !ok || !dns.IsFqdn(ns.Host) {
			return nil, fmt.Errorf("invalid stub NS target")
		}
		hosts[dns.CanonicalName(ns.Host)] = true
		glue[dns.CanonicalName(ns.Host)] = ns.Addrs
	}
	if len(hosts) == 0 || len(hosts) > MaxStubNameservers {
		return nil, fmt.Errorf("stub NS count exceeds bounds")
	}
	limit := data.ConfirmedAt.Add(time.Duration(z.soa.Expire) * time.Second)
	records := make([]dns.RR, 0, len(data.Records))
	for _, input := range data.Records {
		rr, _, err := copySecondaryRR(input)
		if err != nil {
			return nil, err
		}
		owner := dns.CanonicalName(rr.Header().Name)
		t := rr.Header().Rrtype
		if sig, ok := rr.(*dns.RRSIG); ok {
			t = sig.TypeCovered
		}
		switch t {
		case dns.TypeSOA, dns.TypeNS:
			if owner != z.name {
				return nil, fmt.Errorf("stub delegation requires apex SOA/NS")
			}
		case dns.TypeA, dns.TypeAAAA:
			if !hosts[owner] {
				return nil, fmt.Errorf("stub glue owner is not an advertised NS")
			}
		default:
			return nil, fmt.Errorf("unsupported stub delegation record type")
		}
		if t != dns.TypeSOA {
			limit = minTime(limit, data.ConfirmedAt.Add(time.Duration(rr.Header().Ttl)*time.Second))
		}
		records = append(records, rr)
	}
	if data.ExpiresAt.After(limit) {
		return nil, fmt.Errorf("stub expiry exceeds SOA/TTL validity")
	}
	servers := slices.Clone(data.Servers)
	for i := range servers {
		server := &servers[i]
		server.Name = dns.CanonicalName(server.Name)
		if !hosts[server.Name] || !server.Address.IsValid() || server.Address.Port() == 0 || server.Address.Addr().IsUnspecified() || server.Address.Addr().IsMulticast() || server.Address.Addr().Zone() != "" || (server.Protocol != "udp" && server.Protocol != "tcp") {
			return nil, fmt.Errorf("invalid discovered stub authority")
		}
		if ips := glue[server.Name]; len(ips) > 0 {
			if !slices.Contains(ips, server.Address.Addr().Unmap()) {
				return nil, fmt.Errorf("stub server does not match advertised glue")
			}
		} else if data.ExpiresAt.After(data.ConfirmedAt.Add(5 * time.Minute)) {
			return nil, fmt.Errorf("externally resolved stub authority exceeds validity cap")
		}
		z.access.nsAddrs = append(z.access.nsAddrs, server.Address.Addr().Unmap())
	}
	z.typ, z.secondaryIdentity = v1alpha1.ZoneTypeStub, ""
	z.stub = &stubRoute{identity: StubIdentity(dz), servers: servers, records: records, confirmed: data.ConfirmedAt, expires: data.ExpiresAt}
	return z, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// StubIdentity returns the captured configuration identity, or empty otherwise.
func (z *Zone) StubIdentity() string {
	if z.stub == nil {
		return ""
	}
	return z.stub.identity
}

// StubAvailableAt never renews expiry on reads, failures or restarts.
func (z *Zone) StubAvailableAt(now time.Time) bool {
	return z.typ != v1alpha1.ZoneTypeStub || (z.stub != nil && len(z.stub.servers) > 0 && !now.Before(z.stub.confirmed) && now.Before(z.stub.expires))
}

// RoutingAvailableAt checks both secondary data and stub routing validity.
func (z *Zone) RoutingAvailableAt(now time.Time) bool {
	return z.AvailableAt(now) && z.StubAvailableAt(now)
}

// StubServers returns a private copy of captured, pre-resolved authorities.
func (z *Zone) StubServers() []StubServer {
	if z.stub == nil {
		return nil
	}
	return slices.Clone(z.stub.servers)
}

// StubReferral returns TTL-aged apex NS and in-bailiwick glue; SOA is omitted.
func (z *Zone) StubReferral(now time.Time, do bool) (authority, additional []dns.RR) {
	if z.stub == nil || !z.StubAvailableAt(now) {
		return nil, nil
	}
	age := uint32(now.Sub(z.stub.confirmed) / time.Second)     //nolint:gosec // Validity is bounded by uint32 SOA expire and checked above.
	remaining := uint32(z.stub.expires.Sub(now) / time.Second) //nolint:gosec // Same bounded validity window.
	for _, input := range z.stub.records {
		t := input.Header().Rrtype
		if sig, ok := input.(*dns.RRSIG); ok {
			if !do {
				continue
			}
			t = sig.TypeCovered
		}
		if t == dns.TypeSOA {
			continue
		}
		rr := dns.Copy(input)
		ttl := rr.Header().Ttl
		if ttl > age {
			ttl -= age
		} else {
			ttl = 0
		}
		rr.Header().Ttl = min(ttl, remaining)
		if t == dns.TypeNS {
			authority = append(authority, rr)
		} else {
			additional = append(additional, rr)
		}
	}
	return
}
