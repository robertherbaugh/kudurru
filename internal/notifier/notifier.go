// Package notifier sends NOTIFY (RFC 1996) to a zone's secondaries when its
// SOA serial changes. It is a leader-only runnable (dev-docs/design-record.md
// §3.1, §4.6): the leader stamps serials (D12), so the leader notifies, and
// a new leader re-notifies every zone once when its term starts, which is
// harmless (a secondary that is current just compares serials) and covers
// a NOTIFY the previous leader may not have finished.
//
// The notifier reads the zone cache's published zone set (the same
// authoritative.Source the query path reads), so it notifies only once the
// new serial is actually being served. For every primary zone whose
// spec.policy.notify is not none it sends NOTIFY to
//
//   - notifyTargets (specified, both), and
//   - the apex NS hosts except the SOA MNAME (zoneNameServers, both; the
//     RFC 1996 §2 notify set),
//     addressed by the zone's own glue or, for hosts outside the zone,
//     through Options.Resolver,
//
// signed with the zone's notifyTSIGKeyRef, or its first transferTSIGKeyRefs key
// when the explicit NOTIFY key is empty.
// Each target is tried up to Options.Attempts times (RFC 1996 §3.6: 5)
// with exponential backoff while it stays silent; a definitive answer
// (an error rcode, a TSIG failure) is not retried. A newer serial
// supersedes the round in flight for the zone, so bursts coalesce into
// one NOTIFY per target. NOTIFY in stays NOTIMP until secondary zones
// (v0.2).
package notifier

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// Defaults for Options.
const (
	DefaultPollInterval = time.Second
	DefaultAttempts     = 5
	DefaultBackoff      = 2 * time.Second
	DefaultMaxBackoff   = time.Minute
	DefaultTimeout      = zone.DefaultNotifyTimeout
)

// Results of kudurru_zone_notify_total. The failure classes are those of
// zone.NotifyError for the last attempt.
const (
	ResultSuccess    = "success"
	ResultTimeout    = "timeout"
	ResultNetwork    = "network"
	ResultRcode      = "rcode"
	ResultTSIG       = "tsig"
	ResultNoKey      = "nokey"
	ResultSuperseded = "superseded"
)

// Source supplies the served zone set; zoneview.Source and zone.Holder
// implement it. The set must also list its zones (zone.Snapshot does):
// interface{ Zones() []*zone.Zone }.
type Source interface {
	Zones() zone.Set
}

// Resolver looks up the addresses of NS hosts outside the zone;
// *net.Resolver implements it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Options configures a Notifier.
type Options struct {
	// Source is required.
	Source Source
	// Keyring holds the TSIG secrets selected by zone NOTIFY policy. Nil
	// means no keys: zones that name one are not notified (result nokey).
	Keyring dnsserver.TSIGKeyring
	// SourceAddr is the local address NOTIFY is sent from (notify.source);
	// the zero value lets the kernel choose.
	SourceAddr netip.Addr
	// Resolver resolves out-of-zone NS hosts; default net.DefaultResolver.
	Resolver Resolver
	// PollInterval is how often the zone set is compared with the serials
	// already notified when Trigger is not called. Default 1 s.
	PollInterval time.Duration
	// Attempts per target (RFC 1996 §3.6). Default 5.
	Attempts int
	// Backoff is the wait before the first retry; it doubles per retry up
	// to MaxBackoff. Defaults 2 s and 60 s.
	Backoff, MaxBackoff time.Duration
	// Timeout bounds one attempt. Default 3 s.
	Timeout time.Duration
	// SkipInitial records the serials found when a term starts without
	// notifying them. By default every zone is notified once per term.
	SkipInitial bool
	// Registerer receives kudurru_zone_notify_total; nil leaves it
	// unregistered.
	Registerer prometheus.Registerer
	// Logger defaults to slog.Default().
	Logger *slog.Logger

	// send replaces zone.SendNotify in tests.
	send func(context.Context, zone.Notify) error
}

// Notifier is the NOTIFY-out runnable.
type Notifier struct {
	opts    Options
	log     *slog.Logger
	total   *prometheus.CounterVec
	trigger chan struct{}
}

// New validates opts and returns a Notifier. It does no I/O.
func New(opts Options) (*Notifier, error) {
	if opts.Source == nil {
		return nil, errors.New("notifier: Options.Source is required")
	}
	if opts.Resolver == nil {
		opts.Resolver = net.DefaultResolver
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.Attempts <= 0 {
		opts.Attempts = DefaultAttempts
	}
	if opts.Backoff <= 0 {
		opts.Backoff = DefaultBackoff
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = DefaultMaxBackoff
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.send == nil {
		opts.send = zone.SendNotify
	}
	total := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kudurru_zone_notify_total",
		Help: "Outbound NOTIFY per zone and target, by final result (success, timeout, network, rcode, tsig, nokey, superseded).",
	}, []string{"zone", "result"})
	if opts.Registerer != nil {
		if err := opts.Registerer.Register(total); err != nil {
			return nil, err
		}
	}
	return &Notifier{
		opts:    opts,
		log:     opts.Logger.With("component", "notifier"),
		total:   total,
		trigger: make(chan struct{}, 1),
	}, nil
}

// NeedLeaderElection reports that the notifier runs on the leader only.
func (n *Notifier) NeedLeaderElection() bool { return true }

// Trigger asks Run to compare serials now instead of at the next poll. It
// never blocks; hook it to zonecache.Cache.OnPublish.
func (n *Notifier) Trigger() {
	select {
	case n.trigger <- struct{}{}:
	default:
	}
}

// round is the NOTIFY round in flight for one zone.
type round struct {
	serial uint32
	cancel context.CancelFunc
}

// Run notifies until ctx ends (the end of the leader term), then cancels
// the rounds in flight, waits for them and returns nil.
func (n *Notifier) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	rounds := map[string]*round{}
	defer func() {
		for _, r := range rounds {
			r.cancel()
		}
		wg.Wait()
	}()
	t := time.NewTicker(n.opts.PollInterval)
	defer t.Stop()
	first := true
	for {
		if n.scan(ctx, rounds, &wg, first) {
			first = false
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-n.trigger:
		}
	}
}

// lister is the enumeration a zone set needs for the notifier.
type lister interface{ Zones() []*zone.Zone }

// scan starts a round for every notifying zone whose serial differs from
// the last one notified, and cancels rounds of zones that disappeared. It
// returns false while the zone set is not open (nothing is notified from
// unsynced or stale data), so the first open scan still counts as the
// start of the term.
func (n *Notifier) scan(ctx context.Context, rounds map[string]*round, wg *sync.WaitGroup, first bool) bool {
	set := n.opts.Source.Zones()
	if set == nil || set.Gate() != zone.GateOpen {
		return false
	}
	l, ok := set.(lister)
	if !ok {
		n.log.Error("zone set cannot list its zones; nothing is notified")
		return false
	}
	seen := map[string]bool{}
	for _, z := range l.Zones() {
		if z.Type() != v1alpha1.ZoneTypePrimary || z.NotifyMode() == v1alpha1.NotifyNone || z.Serial() == 0 {
			continue
		}
		name := z.Name()
		seen[name] = true
		r := rounds[name]
		switch {
		case r == nil && first && n.opts.SkipInitial:
			rounds[name] = &round{serial: z.Serial(), cancel: func() {}}
			continue
		case r != nil && r.serial == z.Serial():
			continue
		case r != nil:
			r.cancel() // superseded by the newer serial
		}
		rctx, cancel := context.WithCancel(ctx)
		rounds[name] = &round{serial: z.Serial(), cancel: cancel}
		wg.Go(func() {
			defer cancel()
			n.notifyZone(rctx, z)
		})
	}
	for name, r := range rounds {
		if !seen[name] {
			r.cancel()
			delete(rounds, name)
		}
	}
	return true
}

// notifyZone sends one round for z to every target in parallel.
func (n *Notifier) notifyZone(ctx context.Context, z *zone.Zone) {
	targets := n.targets(ctx, z)
	if len(targets) == 0 {
		return
	}
	source := z.NotifySource()
	if !source.IsValid() {
		source = n.opts.SourceAddr
	}
	base := zone.Notify{Zone: z.Name(), Serial: z.Serial(), SOA: z.SOA(), Source: source, Timeout: n.opts.Timeout}
	if key := z.NotifyKey(); key != "" {
		_, alg, ok := n.lookup(key)
		if !ok {
			n.log.Warn("zone names a TSIG key this node does not have; NOTIFY not sent", "zone", z.Name(), "key", key)
			for range targets {
				n.total.WithLabelValues(z.Name(), ResultNoKey).Inc()
			}
			return
		}
		base.KeyName, base.Algorithm, base.TSIG = key, alg, dnsserver.TSIGProvider(n.opts.Keyring)
	}
	n.log.Debug("notifying", "zone", z.Name(), "serial", z.Serial(), "targets", len(targets))
	var wg sync.WaitGroup
	for _, t := range targets {
		msg := base
		msg.Target = t
		wg.Go(func() {
			result := n.notifyTarget(ctx, msg)
			n.total.WithLabelValues(z.Name(), result).Inc()
		})
	}
	wg.Wait()
}

func (n *Notifier) lookup(key string) ([]byte, string, bool) {
	if n.opts.Keyring == nil {
		return nil, "", false
	}
	return n.opts.Keyring.Lookup(key)
}

// notifyTarget tries one target up to Attempts times and returns the
// metric result of the round.
func (n *Notifier) notifyTarget(ctx context.Context, msg zone.Notify) string {
	backoff := n.opts.Backoff
	result := ResultTimeout
	for attempt := 1; attempt <= n.opts.Attempts; attempt++ {
		err := n.opts.send(ctx, msg)
		if err == nil {
			return ResultSuccess
		}
		if ctx.Err() != nil {
			return ResultSuperseded
		}
		result = ResultNetwork
		var ne *zone.NotifyError
		if errors.As(err, &ne) {
			result = ne.Class
		}
		n.log.Debug("notify attempt failed", "zone", msg.Zone, "target", msg.Target, "attempt", attempt, "err", err)
		if result == ResultRcode || result == ResultTSIG {
			// A definitive answer; repeating the question will not change it.
			break
		}
		if attempt == n.opts.Attempts {
			break
		}
		select {
		case <-ctx.Done():
			return ResultSuperseded
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, n.opts.MaxBackoff)
	}
	n.log.Warn("notify failed", "zone", msg.Zone, "serial", msg.Serial, "target", msg.Target, "result", result)
	return result
}

// targets returns the addresses to notify for z, without duplicates.
func (n *Notifier) targets(ctx context.Context, z *zone.Zone) []netip.AddrPort {
	var out []netip.AddrPort
	add := func(ap netip.AddrPort) {
		if !slices.Contains(out, ap) {
			out = append(out, ap)
		}
	}
	mode := z.NotifyMode()
	if mode == v1alpha1.NotifySpecified || mode == v1alpha1.NotifyBoth {
		for _, t := range z.NotifyTargets() {
			add(t)
		}
	}
	if mode == v1alpha1.NotifyZoneNameServers || mode == v1alpha1.NotifyBoth {
		var mname string
		if soa := z.SOA(); soa != nil {
			mname = zone.KeyName(soa.Ns)
		}
		for _, ns := range z.NameServers() {
			if zone.KeyName(ns.Host) == mname {
				continue // the notify set excludes the SOA MNAME (RFC 1996 §2)
			}
			addrs := ns.Addrs
			if len(addrs) == 0 {
				addrs = n.resolve(ctx, ns.Host)
			}
			for _, a := range addrs {
				add(netip.AddrPortFrom(a, zone.DefaultNotifyPort))
			}
		}
	}
	return out
}

func (n *Notifier) resolve(ctx context.Context, host string) []netip.Addr {
	ctx, cancel := context.WithTimeout(ctx, n.opts.Timeout)
	defer cancel()
	addrs, err := n.opts.Resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		n.log.Warn("cannot resolve a nameserver to notify", "host", host, "err", err)
		return nil
	}
	for i, a := range addrs {
		addrs[i] = a.Unmap()
	}
	return addrs
}
