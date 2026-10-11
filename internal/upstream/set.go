package upstream

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"time"

	proxy "github.com/AdguardTeam/dnsproxy/upstream"
	"github.com/miekg/dns"
)

// Options are the process-level dependencies of a Set.
type Options struct {
	// Logger receives health transitions (server down / recovered). nil
	// means slog.Default. dnsproxy's own per-exchange logging is discarded.
	Logger *slog.Logger
	// Metrics, when non-nil, are updated for every exchange.
	Metrics *Metrics
	// RootCAs replaces the system roots for DoT/DoH/DoQ servers (private CAs
	// and tests).
	RootCAs *x509.CertPool
	// Now is the clock used for backoff; nil means time.Now.
	Now func() time.Time
}

// Set is the immutable collection of groups built from one configuration.
// Reloads build a new Set and retire the old one with CloseReplacedBy.
type Set struct {
	groups      map[string]*Group
	conditional *suffixMap
	ecs         ECSConfig
	metrics     *Metrics
	bootstrap   []proxy.Upstream
}

// New builds every group and its servers. Nothing is dialed: dnsproxy
// connects lazily on the first exchange, and host names are resolved then
// through the bootstrap resolvers.
func New(cfg Config, opts Options) (*Set, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	quiet := slog.New(slog.DiscardHandler)

	s := &Set{groups: make(map[string]*Group, len(cfg.Groups)), ecs: cfg.ECS, metrics: opts.Metrics}
	s.conditional, _ = newSuffixMap(cfg.Conditional, cfg.Groups) // validated above

	var boot proxy.Resolver
	if len(cfg.Bootstrap) > 0 {
		var resolvers proxy.ParallelResolver
		for _, addr := range cfg.Bootstrap {
			r, err := proxy.NewUpstreamResolver(addr, &proxy.Options{Timeout: DefaultTimeout, Logger: quiet})
			if err != nil {
				if r != nil {
					_ = r.Close()
				}
				_ = s.Close()
				return nil, fmt.Errorf("upstream: bootstrap %q: %w (bootstrap servers must be IP addresses)", addr, err)
			}
			s.bootstrap = append(s.bootstrap, r.Upstream)
			resolvers = append(resolvers, proxy.NewCachingResolver(r))
		}
		boot = resolvers
	}

	names := make([]string, 0, len(cfg.Groups))
	for n := range cfg.Groups {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		gc := cfg.Groups[name]
		g := &Group{name: name, strategy: gc.Strategy, timeout: gc.Timeout, now: now}
		if g.strategy == "" {
			g.strategy = ParallelBest
		}
		if g.timeout <= 0 {
			g.timeout = DefaultTimeout
		}
		s.groups[name] = g
		seen := map[string]bool{}
		for _, entry := range gc.Servers {
			display, addr := splitName(entry)
			u, err := proxy.AddressToUpstream(addr, &proxy.Options{
				Logger:    quiet,
				Bootstrap: boot,
				Timeout:   g.timeout,
				RootCAs:   opts.RootCAs,
			})
			if err != nil {
				_ = s.Close()
				return nil, fmt.Errorf("upstream: group %q: server %q: %w", name, entry, err)
			}
			srv := &server{name: displayName(display, addr, u), u: u, hc: cfg.Health, now: now}
			srv.label = name + "/" + srv.name
			g.servers = append(g.servers, srv)
			if seen[srv.name] {
				_ = s.Close()
				return nil, fmt.Errorf("upstream: group %q lists server %q twice", name, srv.name)
			}
			seen[srv.name] = true
			srv.setMetrics(opts.Metrics)
			srv.onChange = func(label string, healthy bool) {
				if healthy {
					logger.Info("upstream recovered", "upstream", label)
				} else {
					logger.Warn("upstream in backoff after consecutive failures", "upstream", label, "failures", cfg.Health.Failures)
				}
			}
		}
	}
	return s, nil
}

// Groups returns the group names, sorted.
func (s *Set) Groups() []string {
	out := make([]string, 0, len(s.groups))
	for n := range s.groups {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Group returns a group by name.
func (s *Set) Group(name string) (*Group, bool) {
	g, ok := s.groups[name]
	return g, ok
}

// Route chooses the group for qname: the hint (pipeline.Request's
// UpstreamGroup) when set, else the longest conditional suffix, else
// DefaultGroup. conditional is true for a hint and for a suffix that maps
// to a group other than DefaultGroup; such answers must not be cached. A
// suffix mapped to DefaultGroup itself (dnsmasq's "#": use the normal
// servers, an exception carved out of a wider conditional suffix) is
// ordinary forwarding. An unknown hint is an error so that a forward zone
// never leaks to the public resolvers.
func (s *Set) Route(qname, hint string) (group string, conditional bool, err error) {
	if hint != "" {
		if _, ok := s.groups[hint]; !ok {
			return hint, true, fmt.Errorf("%w %q", ErrUnknownGroup, hint)
		}
		// A forward zone that points at the default group (the bootstrap
		// root zone does) is ordinary forwarding and stays cacheable, like a
		// conditional suffix mapped to the default group.
		return hint, hint != DefaultGroup, nil
	}
	if g, ok := s.conditional.lookup(qname); ok {
		return g, g != DefaultGroup, nil
	}
	return DefaultGroup, false, nil
}

// Exchange sends m to group following the group's strategy and returns the
// first usable answer. m is sent as is (see PrepareQuery) and not
// modified. The error is an *Error when every server failed.
func (s *Set) Exchange(ctx context.Context, group string, m *dns.Msg) (*Result, error) {
	g, ok := s.groups[group]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownGroup, group)
	}
	return g.exchange(ctx, m)
}

// PrepareQuery builds the upstream query for the client message m from
// client: a fresh non-zero ID, RD set, CD and AD copied, the first question
// only, and kudurru's own OPT record (EDNSBufferSize, the client's DO bit,
// and the client subnet chosen by the ECS policy). No other client EDNS
// option (cookies, padding, keepalive) is forwarded.
func (s *Set) PrepareQuery(m *dns.Msg, client netip.Addr) *dns.Msg {
	q := &dns.Msg{MsgHdr: dns.MsgHdr{
		Id:                nonZeroID(),
		Opcode:            m.Opcode,
		RecursionDesired:  true,
		CheckingDisabled:  m.CheckingDisabled,
		AuthenticatedData: m.AuthenticatedData,
	}}
	if len(m.Question) > 0 {
		q.Question = []dns.Question{m.Question[0]}
	}
	opt := &dns.OPT{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT}}
	opt.SetUDPSize(EDNSBufferSize)
	if copt := m.IsEdns0(); copt != nil && copt.Do() {
		opt.SetDo()
	}
	if p := s.ecs.subnet(m, client); p.IsValid() {
		opt.Option = append(opt.Option, ecsOption(p, 0))
	}
	q.Extra = []dns.RR{opt}
	return q
}

// ECSSubnet returns the client subnet PrepareQuery would send for m from
// client, or an invalid prefix. The cache keys entries by it.
func (s *Set) ECSSubnet(m *dns.Msg, client netip.Addr) netip.Prefix {
	return s.ecs.subnet(m, client)
}

// ServerStatus is a point-in-time view of one server for APIs and probes.
type ServerStatus struct {
	Group, Server       string
	Healthy             bool
	ConsecutiveFailures int
	BackoffUntil        time.Time
	RTT                 time.Duration
}

// Status reports every server, by group and configuration order.
func (s *Set) Status() []ServerStatus {
	var out []ServerStatus
	for _, name := range s.Groups() {
		for _, srv := range s.groups[name].servers {
			srv.mu.Lock()
			st := ServerStatus{
				Group:               name,
				Server:              srv.name,
				Healthy:             srv.fails < srv.hc.Failures,
				ConsecutiveFailures: srv.fails,
				RTT:                 time.Duration(srv.rtt * float64(time.Second)),
			}
			if !st.Healthy {
				st.BackoffUntil = srv.downUntil
			}
			srv.mu.Unlock()
			out = append(out, st)
		}
	}
	return out
}

// Close releases every connection. Metric series stay; see CloseReplacedBy.
func (s *Set) Close() error {
	var errs []error
	for _, g := range s.groups {
		for _, srv := range g.servers {
			errs = append(errs, srv.u.Close())
		}
	}
	for _, b := range s.bootstrap {
		errs = append(errs, b.Close())
	}
	return errors.Join(errs...)
}

// CloseReplacedBy closes s after a reload to next and deletes the metric
// series of servers next no longer has.
func (s *Set) CloseReplacedBy(next *Set) error {
	keep := map[string]bool{}
	if next != nil {
		for _, g := range next.groups {
			for _, srv := range g.servers {
				keep[srv.label] = true
			}
		}
	}
	for _, g := range s.groups {
		for _, srv := range g.servers {
			if !keep[srv.label] {
				s.metrics.forget(srv.label)
			}
		}
	}
	return s.Close()
}

// nonZeroID returns a random message ID other than 0, which DoQ and DoH
// clients use as a fixed ID.
func nonZeroID() uint16 {
	for {
		if id := dns.Id(); id != 0 {
			return id
		}
	}
}
