// Package stub discovers bounded delegations outside the query path and asks
// their captured authorities directly. It is not an iterative resolver.
package stub

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// Resolver resolves bootstrap and missing NS addresses during discovery only.
type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// Discoverer supplies a complete bounded delegation for durable publication.
type Discoverer interface {
	Discover(context.Context, *v1alpha1.DNSZone) (*zone.StubData, error)
}

// Client is safe for concurrent use when its configuration is immutable.
// Defaults: discovery 5s, query 2s, system host resolver. TLS is unsupported;
// explicit TCP stays TCP, UDP retries TCP only on a matching truncated reply.
type Client struct {
	Resolver              Resolver
	Timeout, QueryTimeout time.Duration
	Now                   func() time.Time
}

type endpoint struct {
	host, protocol string
	port           uint16
}

func parseEndpoint(value string) (endpoint, error) {
	e := endpoint{protocol: "udp", port: 53}
	host := value
	if p, tail, ok := strings.Cut(value, "://"); ok {
		e.protocol, host = p, tail
	}
	if e.protocol != "udp" && e.protocol != "tcp" {
		return e, fmt.Errorf("unsupported stub protocol %q", e.protocol)
	}
	if host == "" || strings.ContainsAny(host, "/?#@ \t\r\n") {
		return e, errors.New("invalid stub endpoint")
	}
	if h, port, err := net.SplitHostPort(host); err == nil {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return e, errors.New("invalid stub port")
		}
		e.port, host = uint16(n), h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	} else if strings.Contains(host, ":") {
		if _, err := netip.ParseAddr(host); err != nil {
			return e, errors.New("invalid stub endpoint port")
		}
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if !validIP(ip) {
			return e, errors.New("invalid stub address")
		}
	} else if _, ok := dns.IsDomainName(host); !ok {
		return e, errors.New("invalid stub host")
	}
	e.host = host
	return e, nil
}

func validIP(ip netip.Addr) bool {
	return ip.IsValid() && ip.Zone() == "" && !ip.IsUnspecified() && !ip.IsMulticast()
}

// ValidateConfig is useful to callers reporting configuration diagnostics.
func ValidateConfig(dz *v1alpha1.DNSZone) error {
	if dz == nil || dz.Spec.Type != v1alpha1.ZoneTypeStub || dz.Spec.Stub == nil || len(dz.Spec.Stub.Nameservers) == 0 || len(dz.Spec.Stub.Nameservers) > zone.MaxStubNameservers {
		return errors.New("stub requires 1..16 bootstrap nameservers")
	}
	if _, err := zone.BuildStubPlaceholder(dz); err != nil {
		return err
	}
	for _, value := range dz.Spec.Stub.Nameservers {
		if _, err := parseEndpoint(value); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}
func bounded(ctx context.Context, timeout, fallback time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = fallback
	}
	return context.WithTimeout(ctx, timeout)
}

func (c *Client) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{ip.Unmap()}, nil
	}
	resolver := c.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	ips, err := resolver.LookupNetIP(ctx, "ip", strings.TrimSuffix(host, "."))
	if err != nil {
		return nil, err
	}
	if len(ips) > zone.MaxStubServers {
		return nil, errors.New("stub host resolution exceeds address bound")
	}
	out := make([]netip.Addr, 0, len(ips))
	seen := map[netip.Addr]bool{}
	for _, ip := range ips {
		ip = ip.Unmap()
		if validIP(ip) && !seen[ip] {
			out = append(out, ip)
			seen[ip] = true
		}
	}
	if len(out) == 0 {
		return nil, errors.New("stub host resolution returned no usable addresses")
	}
	return out, nil
}

// Discover asks one bootstrap for coherent SOA/NS/SOA, then resolves missing
// authority addresses. The whole attempt, including all failover, is bounded.
func (c *Client) Discover(ctx context.Context, dz *v1alpha1.DNSZone) (*zone.StubData, error) {
	if err := ValidateConfig(dz); err != nil {
		return nil, err
	}
	ctx, cancel := bounded(ctx, c.Timeout, 5*time.Second)
	defer cancel()
	var failures []error
	apex := dns.CanonicalName(dz.Spec.ZoneName)
	attempts := 0
	for i, value := range dz.Spec.Stub.Nameservers {
		ep, _ := parseEndpoint(value)
		bootstrapCtx, bootstrapCancel := attemptContext(ctx, len(dz.Spec.Stub.Nameservers)-i)
		ips, err := c.resolve(bootstrapCtx, ep.host)
		if err != nil {
			bootstrapCancel()
			failures = append(failures, err)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		for j, ip := range ips {
			attempts++
			if attempts > zone.MaxStubServers {
				bootstrapCancel()
				return nil, errors.New("stub bootstrap attempts exceed bound")
			}
			server := zone.StubServer{Address: netip.AddrPortFrom(ip, ep.port), Protocol: ep.protocol}
			attemptCtx, attemptCancel := attemptContext(bootstrapCtx, len(ips)-j)
			data, err := c.discoverAt(attemptCtx, apex, server)
			attemptCancel()
			if err == nil {
				if _, err = zone.BuildStub(dz, data); err == nil {
					bootstrapCancel()
					return data, nil
				}
			}
			failures = append(failures, err)
			if ctx.Err() != nil {
				bootstrapCancel()
				return nil, ctx.Err()
			}
		}
		bootstrapCancel()
	}

	return nil, fmt.Errorf("stub discovery failed: %w", errors.Join(failures...))
}

func discoveryQuestion(apex string, t uint16) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(apex, t)
	m.RecursionDesired = false
	m.SetEdns0(1232, true)
	return m
}

func (c *Client) discoverAt(ctx context.Context, apex string, server zone.StubServer) (*zone.StubData, error) {
	first, err := exchange(ctx, server, discoveryQuestion(apex, dns.TypeSOA))
	if err != nil {
		return nil, err
	}
	soa, err := discoverySOA(first, apex)
	if err != nil {
		return nil, err
	}
	ns, err := exchange(ctx, server, discoveryQuestion(apex, dns.TypeNS))
	if err != nil {
		return nil, err
	}
	if !ns.Authoritative || ns.Rcode != dns.RcodeSuccess {
		return nil, errors.New("stub NS response is not authoritative success")
	}
	hosts := map[string]bool{}
	records := []dns.RR{soa}
	for _, rr := range first.Answer {
		if _, ok := rr.(*dns.RRSIG); ok {
			records = append(records, rr)
		}
	}
	for _, rr := range ns.Answer {
		if dns.CanonicalName(rr.Header().Name) != apex {
			return nil, errors.New("stub NS response has non-apex answer")
		}
		switch r := rr.(type) {
		case *dns.NS:
			if !dns.IsFqdn(r.Ns) {
				return nil, errors.New("stub NS target is not absolute")
			}
			hosts[dns.CanonicalName(r.Ns)] = true
			records = append(records, r)
		case *dns.RRSIG:
			if r.TypeCovered != dns.TypeNS {
				return nil, errors.New("stub NS signature covers wrong type")
			}
			records = append(records, r)
		default:
			return nil, errors.New("stub NS response contains unexpected answer")
		}
	}
	if len(hosts) == 0 || len(hosts) > zone.MaxStubNameservers {
		return nil, errors.New("stub NS count exceeds bounds")
	}
	addrs := map[string][]netip.Addr{}
	for _, rr := range ns.Extra {
		host := dns.CanonicalName(rr.Header().Name)
		if !hosts[host] || !dns.IsSubDomain(apex, host) {
			continue
		}
		var ip netip.Addr
		switch r := rr.(type) {
		case *dns.A:
			ip, _ = netip.AddrFromSlice(r.A)
		case *dns.AAAA:
			ip, _ = netip.AddrFromSlice(r.AAAA)
		case *dns.RRSIG:
			if r.TypeCovered == dns.TypeA || r.TypeCovered == dns.TypeAAAA {
				records = append(records, rr)
			}
			continue
		default:
			continue
		}
		ip = ip.Unmap()
		if !validIP(ip) {
			return nil, errors.New("stub NS glue has invalid address")
		}
		addrs[host] = append(addrs[host], ip)
		records = append(records, rr)
	}
	last, err := exchange(ctx, server, discoveryQuestion(apex, dns.TypeSOA))
	if err != nil {
		return nil, err
	}
	confirmedSOA, err := discoverySOA(last, apex)
	if err != nil {
		return nil, err
	}
	if !dns.IsDuplicate(soa, confirmedSOA) || soa.Hdr.Ttl != confirmedSOA.Hdr.Ttl {
		return nil, errors.New("stub SOA changed during discovery")
	}
	confirmed := c.now()
	expiry := confirmed.Add(time.Duration(soa.Expire) * time.Second)
	for _, rr := range records[1:] {
		if sig, ok := rr.(*dns.RRSIG); ok && sig.TypeCovered == dns.TypeSOA {
			continue
		}
		expiry = earlier(expiry, confirmed.Add(time.Duration(rr.Header().Ttl)*time.Second))
	}
	data := &zone.StubData{Records: records, ConfirmedAt: confirmed}
	// Canonical host ordering makes durable copies and failover deterministic.
	for _, host := range sortedHosts(hosts) {
		ips := addrs[host]
		if len(ips) == 0 {
			ips, err = c.resolve(ctx, host)
			if err != nil {
				return nil, err
			}
			expiry = earlier(expiry, confirmed.Add(5*time.Minute))
		}
		for _, ip := range ips {
			s := zone.StubServer{Name: host, Address: netip.AddrPortFrom(ip, server.Address.Port()), Protocol: server.Protocol}
			duplicate := false
			for _, existing := range data.Servers {
				if existing == s {
					duplicate = true
					break
				}
			}
			if !duplicate {
				data.Servers = append(data.Servers, s)
			}
			if len(data.Servers) > zone.MaxStubServers {
				return nil, errors.New("stub authorities exceed endpoint bound")
			}
		}
	}
	data.ExpiresAt = expiry
	return data, nil
}

func sortedHosts(hosts map[string]bool) []string { return slices.Sorted(maps.Keys(hosts)) }

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func discoverySOA(m *dns.Msg, apex string) (*dns.SOA, error) {
	if !m.Authoritative || m.Rcode != dns.RcodeSuccess {
		return nil, errors.New("stub SOA response is not authoritative success")
	}
	var soa *dns.SOA
	for _, rr := range m.Answer {
		if dns.CanonicalName(rr.Header().Name) != apex {
			return nil, errors.New("stub SOA has non-apex answer")
		}
		switch r := rr.(type) {
		case *dns.SOA:
			if soa != nil {
				return nil, errors.New("duplicate stub SOA")
			}
			soa = r
		case *dns.RRSIG:
			if r.TypeCovered != dns.TypeSOA {
				return nil, errors.New("stub SOA signature covers wrong type")
			}
		default:
			return nil, errors.New("stub SOA contains unexpected answer")
		}
	}
	if soa == nil || soa.Expire == 0 {
		return nil, errors.New("missing or invalid stub SOA")
	}
	return soa, nil
}

// Exchange asks only captured IP endpoints, with RD=0; referrals are returned
// without following them. Client DNSSEC records are preserved, AD is cleared.
func (c *Client) Exchange(ctx context.Context, z *zone.Zone, query *dns.Msg) (*dns.Msg, error) {
	if z == nil || !z.StubAvailableAt(c.now()) || z.Type() != v1alpha1.ZoneTypeStub || query == nil || len(query.Question) != 1 {
		return nil, errors.New("stub route/query is unavailable")
	}
	q := query.Question[0]
	if !z.Contains(q.Name) || q.Qclass != dns.ClassINET || q.Qtype == dns.TypeAXFR || q.Qtype == dns.TypeIXFR {
		return nil, errors.New("unsupported stub query")
	}
	ctx, cancel := bounded(ctx, c.QueryTimeout, 2*time.Second)
	defer cancel()
	m := new(dns.Msg)
	m.SetQuestion(q.Name, q.Qtype)
	m.RecursionDesired = false
	m.CheckingDisabled = query.CheckingDisabled
	if opt := query.IsEdns0(); opt != nil {
		m.SetEdns0(1232, opt.Do())
	}
	var failures []error
	servers := z.StubServers()
	for i, server := range servers {
		attemptCtx, attemptCancel := attemptContext(ctx, len(servers)-i)
		reply, err := exchange(attemptCtx, server, m)
		attemptCancel()
		if err == nil && (reply.Rcode == dns.RcodeServerFailure || reply.Rcode == dns.RcodeRefused) && i+1 < len(servers) {
			err = fmt.Errorf("stub authority returned %s", dns.RcodeToString[reply.Rcode])
		}
		if err == nil {
			reply.AuthenticatedData = false
			return reply, nil
		}
		failures = append(failures, err)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, errors.Join(failures...)
}

func exchange(ctx context.Context, server zone.StubServer, query *dns.Msg) (*dns.Msg, error) {
	reply, err := wireExchange(ctx, server.Protocol, server.Address, query)
	if err != nil {
		return nil, err
	}
	if reply.Truncated && server.Protocol == "udp" {
		return wireExchange(ctx, "tcp", server.Address, query)
	}
	return reply, nil
}

func wireExchange(ctx context.Context, protocol string, address netip.AddrPort, query *dns.Msg) (*dns.Msg, error) {
	if protocol != "udp" && protocol != "tcp" {
		return nil, errors.New("unsupported stub protocol")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, protocol, address.String())
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = conn.Close(); close(done) })
	defer func() {
		if !stop() {
			<-done
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}
	packed, err := query.Pack()
	if err != nil {
		return nil, err
	}
	if len(packed) > dns.MaxMsgSize {
		return nil, errors.New("stub request exceeds DNS wire limit")
	}
	if protocol == "tcp" {
		frame := make([]byte, 2+len(packed))
		binary.BigEndian.PutUint16(frame, uint16(len(packed))) //nolint:gosec // DNS wire maximum checked above.
		copy(frame[2:], packed)
		for len(frame) > 0 {
			n, err := conn.Write(frame)
			if err != nil {
				return nil, err
			}
			if n == 0 {
				return nil, io.ErrShortWrite
			}
			frame = frame[n:]
		}
	} else {
		if n, err := conn.Write(packed); err != nil {
			return nil, err
		} else if n != len(packed) {
			return nil, io.ErrShortWrite
		}
	}
	var wire []byte
	if protocol == "tcp" {
		var length [2]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return nil, err
		}
		size := int(binary.BigEndian.Uint16(length[:]))
		if size < 12 {
			return nil, errors.New("short stub DNS frame")
		}
		wire = make([]byte, size)
		if _, err := io.ReadFull(conn, wire); err != nil {
			return nil, err
		}
	} else {
		buf := make([]byte, dns.MaxMsgSize+1)
		n, err := conn.Read(buf)
		if err != nil {
			return nil, err
		}
		wire = buf[:n]
	}
	reply, err := unpackReply(wire, query, protocol == "udp")
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return reply, err
}

// Check framing independently of Msg.Unpack, which can accept trailing data.
// A truncated UDP body may be retried only after matching header/question.
func unpackReply(wire []byte, query *dns.Msg, allowTC bool) (*dns.Msg, error) {
	if len(wire) < 12 || binary.BigEndian.Uint16(wire) != query.Id {
		return nil, errors.New("short or mismatched stub response")
	}
	flags := binary.BigEndian.Uint16(wire[2:])
	if flags&0x8000 == 0 || flags&0x0040 != 0 || (flags>>11)&15 != dns.OpcodeQuery || binary.BigEndian.Uint16(wire[4:]) != 1 {
		return nil, errors.New("invalid stub response header")
	}
	name, off, err := dns.UnpackDomainName(wire, 12)
	if err != nil || off+4 > len(wire) {
		return nil, errors.New("malformed stub question")
	}
	q := query.Question[0]
	if dns.CanonicalName(name) != dns.CanonicalName(q.Name) || binary.BigEndian.Uint16(wire[off:]) != q.Qtype || binary.BigEndian.Uint16(wire[off+2:]) != q.Qclass {
		return nil, errors.New("mismatched stub question")
	}
	if flags&0x0200 != 0 {
		if allowTC {
			return &dns.Msg{MsgHdr: dns.MsgHdr{Truncated: true}}, nil
		}
		return nil, errors.New("truncated stub TCP response")
	}
	off += 4
	answers := int(binary.BigEndian.Uint16(wire[6:]))
	authorities := int(binary.BigEndian.Uint16(wire[8:]))
	count := answers + authorities + int(binary.BigEndian.Uint16(wire[10:]))
	if count > zone.MaxStubRecords {
		return nil, errors.New("stub response exceeds record bound")
	}
	bytes := 0
	opt := false
	for i := range count {
		rr, end, err := dns.UnpackRR(wire, off)
		if err != nil || rr == nil || end <= off {
			return nil, errors.New("malformed stub record framing")
		}
		switch rr.Header().Rrtype {
		case dns.TypeOPT:
			if i < answers+authorities || opt || rr.Header().Name != "." {
				return nil, errors.New("invalid stub OPT")
			}
			opt = true
		case dns.TypeTSIG, dns.TypeTKEY, dns.TypeAXFR, dns.TypeIXFR:
			return nil, errors.New("unexpected stub pseudo record")
		default:
			if rr.Header().Class != dns.ClassINET || (rr.Header().Rdlength == 0 && rr.Header().Rrtype != dns.TypeNULL && rr.Header().Rrtype != dns.TypeAPL) {
				return nil, errors.New("invalid stub record class/data")
			}
		}
		if sig, ok := rr.(*dns.SIG); ok && sig.TypeCovered == 0 {
			return nil, errors.New("unexpected stub SIG(0)")
		}
		bytes += dns.Len(rr)
		if bytes > zone.MaxStubBytes {
			return nil, errors.New("stub response exceeds expanded byte bound")
		}
		off = end
	}
	if off != len(wire) {
		return nil, errors.New("trailing stub response bytes")
	}
	m := new(dns.Msg)
	if err := m.Unpack(wire); err != nil {
		return nil, err
	}
	return m, nil
}

// Divide the remaining overall deadline among pending endpoints, so a silent
// first authority cannot consume the entire failover budget.
func attemptContext(ctx context.Context, remaining int) (context.Context, context.CancelFunc) {
	deadline, _ := ctx.Deadline()
	return context.WithTimeout(ctx, time.Until(deadline)/time.Duration(max(remaining, 1)))
}
