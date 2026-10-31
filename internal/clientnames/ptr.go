// Package clientnames acquires optional, untrusted client-name observations off
// the DNS query path. PTR names never authenticate a client.
package clientnames

import (
	"container/list"
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// Options are fixed host authority for one process lifetime. Sources are IP
// literals, avoiding recursive use of the resolver being served.
type Options struct {
	Servers, Networks            []string
	MaxEntries, Workers          int
	Timeout, MaxAge, NegativeTTL time.Duration
}

// Validate rejects unbounded acquisition and implicit network scope.
func (o Options) Validate() error {
	if len(o.Servers) < 1 || len(o.Servers) > 4 || len(o.Networks) < 1 || len(o.Networks) > 32 || o.MaxEntries < 1 || o.MaxEntries > 65536 || o.Workers < 1 || o.Workers > 8 || o.Timeout < 100*time.Millisecond || o.Timeout > 5*time.Second || o.MaxAge < time.Second || o.MaxAge > time.Hour || o.NegativeTTL < time.Second || o.NegativeTTL > 5*time.Minute {
		return errors.New("reverse PTR requires 1-4 servers, 1-32 networks, entries 1-65536, workers 1-8, timeout 100ms-5s, maxAge 1s-1h and negativeTTL 1s-5m")
	}
	for _, value := range o.Servers {
		addr, err := netip.ParseAddrPort(value)
		if err != nil || addr.Port() == 0 || addr.Addr().Zone() != "" || addr.Addr().IsUnspecified() || addr.Addr().IsMulticast() {
			return errors.New("reverse PTR servers must be explicit unicast IP:port addresses")
		}
	}
	for _, value := range o.Networks {
		p, err := netip.ParsePrefix(value)
		if err != nil || p.Addr().Is4In6() {
			return errors.New("reverse PTR networks must be IPv4 or IPv6 CIDRs")
		}
	}
	return nil
}

type entry struct {
	addr    netip.Addr
	name    string
	until   time.Time
	pending bool
	element *list.Element
}

// Resolver implements policy.HostnameResolver. Hostnames uses bounded memory
// and a nonblocking queue only; Run owns and joins all network workers.
type Resolver struct {
	opts          Options
	networks      []netip.Prefix
	mu            sync.Mutex
	entries       map[netip.Addr]*entry
	lru           list.List
	queue         chan *entry
	running, used bool
}

// New validates and captures host sources without starting network work.
func New(o Options) (*Resolver, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	o.Servers = append([]string(nil), o.Servers...)
	o.Networks = append([]string(nil), o.Networks...)
	r := &Resolver{opts: o, entries: make(map[netip.Addr]*entry), queue: make(chan *entry, min(o.MaxEntries, 256))}
	for _, value := range o.Networks {
		r.networks = append(r.networks, netip.MustParsePrefix(value).Masked())
	}
	return r, nil
}

// Hostnames returns a copy of one unexpired name, or nil immediately. Cold and
// expired addresses request background acquisition without holding the query.
func (r *Resolver) Hostnames(addr netip.Addr) []string {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.Zone() != "" || addr.IsUnspecified() || addr.IsMulticast() {
		return nil
	}
	allowed := false
	for _, p := range r.networks {
		if p.Contains(addr) {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return nil
	}
	now := time.Now()
	e := r.entries[addr]
	if e == nil {
		if len(r.entries) == r.opts.MaxEntries {
			old := r.lru.Back().Value.(*entry)
			delete(r.entries, old.addr)
			r.lru.Remove(old.element)
		}
		e = &entry{addr: addr}
		e.element = r.lru.PushFront(e)
		r.entries[addr] = e
	} else {
		r.lru.MoveToFront(e.element)
	}
	if now.Before(e.until) {
		if e.name != "" {
			return []string{e.name}
		}
		return nil
	}
	e.name = ""
	if !e.pending {
		select {
		case r.queue <- e:
			e.pending = true
		default:
		}
	}
	return nil
}

// Run may be called once. Cancellation invalidates cached observations before
// joining workers; queued jobs cannot restore names after shutdown.
func (r *Resolver) Run(ctx context.Context) error {
	r.mu.Lock()
	if r.used {
		r.mu.Unlock()
		return errors.New("reverse PTR resolver already run")
	}
	r.used = true
	r.running = ctx.Err() == nil
	r.mu.Unlock()
	var wg sync.WaitGroup
	for range r.opts.Workers {
		wg.Go(func() { r.worker(ctx) })
	}
	<-ctx.Done()
	r.mu.Lock()
	r.running = false
	clear(r.entries)
	r.lru.Init()
	r.mu.Unlock()
	wg.Wait()
	return nil
}

func (r *Resolver) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-r.queue:
			r.mu.Lock()
			current := r.running && r.entries[e.addr] == e
			r.mu.Unlock()
			if !current || ctx.Err() != nil {
				continue
			}
			started := time.Now()
			queryCtx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
			name, ttl := r.lookup(queryCtx, e.addr)
			// A late response never acquires a new lease on the name.
			if queryCtx.Err() != nil || time.Since(started) >= r.opts.Timeout {
				name = ""
				ttl = 0
			}
			cancel()
			if name == "" {
				ttl = r.opts.NegativeTTL
			}
			until := started.Add(ttl)
			r.mu.Lock()
			if r.running && ctx.Err() == nil && r.entries[e.addr] == e {
				e.name, e.until, e.pending = name, until, false
			}
			r.mu.Unlock()
		}
	}
}

func (r *Resolver) lookup(ctx context.Context, addr netip.Addr) (string, time.Duration) {
	reverse, err := dns.ReverseAddr(addr.String())
	if err != nil {
		return "", 0
	}
	q := new(dns.Msg)
	q.SetQuestion(reverse, dns.TypePTR)
	client := dns.Client{Net: "udp", UDPSize: 1232, Timeout: r.opts.Timeout}
	for _, server := range r.opts.Servers {
		if ctx.Err() != nil {
			break
		}
		answer, _, err := client.ExchangeContext(ctx, q, server)
		if err != nil {
			continue
		}
		name, ttl, valid := ptrAnswer(q, answer)
		if valid {
			return name, min(ttl, r.opts.MaxAge)
		}
	}
	return "", 0
}

// ptrAnswer accepts a direct, single canonical PTR target. No CNAME chasing,
// truncated answers or ambiguous identity is admitted. TTL starts at acquisition.
func ptrAnswer(q, a *dns.Msg) (string, time.Duration, bool) {
	if a == nil || !a.Response || a.Id != q.Id || a.Opcode != dns.OpcodeQuery || a.Truncated || len(a.Question) != 1 || a.Question[0].Qtype != dns.TypePTR || a.Question[0].Qclass != dns.ClassINET || !strings.EqualFold(a.Question[0].Name, q.Question[0].Name) {
		return "", 0, false
	}
	if a.Rcode == dns.RcodeNameError && len(a.Answer) == 0 {
		return "", 0, true
	}
	if a.Rcode != dns.RcodeSuccess {
		return "", 0, false
	}
	var name string
	ttl := time.Duration(1<<63 - 1)
	for _, rr := range a.Answer {
		ptr, ok := rr.(*dns.PTR)
		if !ok || ptr.Hdr.Class != dns.ClassINET || !strings.EqualFold(ptr.Hdr.Name, q.Question[0].Name) || ptr.Hdr.Ttl == 0 {
			return "", 0, false
		}
		target := strings.ToLower(strings.TrimSuffix(ptr.Ptr, "."))
		if !hostname(target) || name != "" && name != target {
			return "", 0, false
		}
		name = target
		ttl = min(ttl, time.Duration(ptr.Hdr.Ttl)*time.Second)
	}
	return name, ttl, true
}

func hostname(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}
