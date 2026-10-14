package secondary

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// NotifyReceiver authorizes RFC 1996 hints without doing network or store I/O
// in the DNS handler. Configured primary names are resolved in Run, with a
// bounded lifetime. A configured TSIG key and primary source must both match.
// Hints never carry authoritative data or reset a copy's expiry.
type NotifyReceiver struct {
	Manager *Manager
	// Accepted is a nonblocking callback for peer fan-out, called only after
	// local authorization. Peer event receivers trigger their manager directly
	// so a hint can never loop around the cluster.
	Accepted func(identity string)
	// Lookup replaces the OS resolver in tests. Each lookup has a 2 s budget.
	Lookup  func(context.Context, string) ([]netip.Addr, error)
	results *prometheus.CounterVec
	sources atomic.Pointer[notifySources]
	wake    chan struct{}
}

type notifySource struct {
	addresses []netip.Addr
	expires   time.Time
}
type notifySources map[string]map[string]notifySource

// NewNotifyReceiver creates the listener-side authorization and resolver.
func NewNotifyReceiver(m *Manager, reg prometheus.Registerer) *NotifyReceiver {
	n := &NotifyReceiver{Manager: m, wake: make(chan struct{}, 1), results: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kudurru_secondary_notify_total", Help: "Inbound secondary NOTIFY messages by authorization and scheduling result.",
	}, []string{"result"})}
	if reg != nil {
		reg.MustRegister(n.results)
	}
	m.opts.Cache.OnPublish(func(_ *zonecache.Snapshot) {
		select {
		case n.wake <- struct{}{}:
		default:
		}
	})
	return n
}

// Handle receives a NOTIFY through authoritative's opcode dispatcher.
func (n *NotifyReceiver) Handle(req *pipeline.Request) (*pipeline.Response, error) {
	reply := func(code int, result string) (*pipeline.Response, error) {
		n.results.WithLabelValues(result).Inc()
		r := pipeline.Reply(req, code, pipeline.StatusAuthoritative)
		r.Msg.Authoritative = true
		return r, nil
	}
	m := req.Msg
	if !validNotify(m) {
		return reply(dns.RcodeFormatError, "malformed")
	}
	if req.Client.Synthetic || (req.Client.Proto != pipeline.ProtoUDP && req.Client.Proto != pipeline.ProtoTCP && req.Client.Proto != pipeline.ProtoDoT) {
		return reply(dns.RcodeRefused, "refused")
	}
	config := n.Manager.opts.Cache.Snapshot()
	if config == nil || !config.Authoritative() {
		return reply(dns.RcodeServerFailure, "configuration")
	}
	name := dns.CanonicalName(m.Question[0].Name)
	zones := config.Objects.Zones()
	if zone.ConflictingApices(zones)[name] {
		return reply(dns.RcodeServerFailure, "configuration")
	}
	var dz *v1alpha1.DNSZone
	for _, candidate := range zones {
		if dns.CanonicalName(candidate.Spec.ZoneName) == name {
			dz = candidate
			break
		}
	}
	if dz == nil || dz.Spec.Type != v1alpha1.ZoneTypeSecondary || dz.Spec.Secondary == nil {
		return reply(dns.RcodeNotAuth, "unknown")
	}
	id := zone.SecondaryIdentity(dz)
	if key := dz.Spec.Secondary.TSIGKeyRef; key != "" && req.Client.TSIGKey != dnsserver.TSIGKeyName(key) {
		return reply(dns.RcodeRefused, "refused")
	}
	if !n.sourceAllowed(dz, id, req.Client.Addr.Addr().Unmap()) {
		return reply(dns.RcodeRefused, "refused")
	}
	if !n.Manager.Trigger(id) {
		return reply(dns.RcodeServerFailure, "busy")
	}
	if n.Accepted != nil {
		n.Accepted(id)
	}
	return reply(dns.RcodeSuccess, "accepted")
}

func validNotify(m *dns.Msg) bool {
	if m == nil || m.Response || m.Opcode != dns.OpcodeNotify || m.Rcode != dns.RcodeSuccess || len(m.Question) != 1 || len(m.Ns) != 0 || len(m.Answer) > 1 {
		return false
	}
	q := m.Question[0]
	if q.Qclass != dns.ClassINET || q.Qtype != dns.TypeSOA {
		return false
	}
	if _, ok := dns.IsDomainName(q.Name); !ok || !dns.IsFqdn(q.Name) {
		return false
	}
	if len(m.Answer) == 1 {
		soa, ok := m.Answer[0].(*dns.SOA)
		if !ok || soa.Hdr.Class != dns.ClassINET || dns.CanonicalName(soa.Hdr.Name) != dns.CanonicalName(q.Name) {
			return false
		}
	}
	for _, rr := range m.Extra {
		if rr == nil || (rr.Header().Rrtype != dns.TypeOPT && rr.Header().Rrtype != dns.TypeTSIG) {
			return false
		}
	}
	return true
}

func (n *NotifyReceiver) sourceAllowed(dz *v1alpha1.DNSZone, id string, addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	sources := n.sources.Load()
	now := n.Manager.opts.Now()
	for _, primary := range dz.Spec.Secondary.Primaries {
		endpoint, err := transferAddress(primary)
		if err != nil {
			continue
		}
		host, _, _ := net.SplitHostPort(endpoint)
		if ip, err := netip.ParseAddr(host); err == nil {
			if ip.Unmap() == addr {
				return true
			}
		} else if sources != nil {
			entry := (*sources)[id][host]
			if now.Before(entry.expires) && slices.Contains(entry.addresses, addr) {
				return true
			}
		}
	}
	return false
}

// Run refreshes hostname authorization away from request handlers. Numeric
// primaries need no DNS. Failed lookups retain the last result for at most
// two minutes; configuration identity changes invalidate it immediately.
func (n *NotifyReceiver) Run(ctx context.Context) error {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		n.resolve(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-n.wake:
		case <-ticker.C:
		}
	}
}

func (n *NotifyReceiver) resolve(ctx context.Context) {
	config := n.Manager.opts.Cache.Snapshot()
	if config == nil {
		return
	}
	next := make(notifySources)
	old := n.sources.Load()
	lookup := n.Lookup
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	// One bounded resolver operation at a time prevents configuration bursts
	// from spawning an unbounded set of goroutines or outbound requests.
	for _, dz := range config.Objects.Zones() {
		if ctx.Err() != nil {
			return
		}
		if dz.Spec.Type != v1alpha1.ZoneTypeSecondary || dz.Spec.Secondary == nil {
			continue
		}
		id := zone.SecondaryIdentity(dz)
		next[id] = make(map[string]notifySource)
		for _, primary := range dz.Spec.Secondary.Primaries {
			endpoint, err := transferAddress(primary)
			if err != nil {
				continue
			}
			host, _, _ := net.SplitHostPort(endpoint)
			if _, err := netip.ParseAddr(host); err == nil {
				continue
			}
			if _, ok := next[id][host]; ok {
				continue
			}
			queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			addresses, err := lookup(queryCtx, host)
			cancel()
			if err == nil {
				// Resolver results are copied, normalized and capped before publication.
				if len(addresses) > 64 {
					addresses = addresses[:64]
				}
				addresses = slices.Clone(addresses)
				for i := range addresses {
					addresses[i] = addresses[i].Unmap()
				}
				next[id][host] = notifySource{addresses: addresses, expires: n.Manager.opts.Now().Add(2 * time.Minute)}
			} else if old != nil {
				next[id][host] = (*old)[id][host]
			}
		}
	}
	n.sources.Store(&next)
}
