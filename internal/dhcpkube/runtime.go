package dhcpkube

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	dhcpv1 "github.com/robertherbaugh/kudurru/api/dhcp/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// ElectionName is the store.Elector name of the DHCP allocator: Lease
// kudurru-dhcp in the release namespace.
const ElectionName = "dhcp"

// Defaults of Options.
const (
	defaultInterval = 30 * time.Second
	// pendingWindow bounds how long a replica ignores watch events for an
	// object it wrote, waiting for the event of its own write.
	pendingWindow = 30 * time.Second
	// expiredGrace keeps an expired lease object briefly, so a renewal
	// racing its expiry finds it.
	expiredGrace = time.Minute
)

// Options configures a Runtime.
type Options struct {
	// Store holds the DHCPScope objects; their status is written here.
	Store store.Store
	// Leases reads and writes the DHCPLease objects.
	Leases LeaseAPI
	// Pods labels the serving pod; nil skips labeling (tests).
	Pods PodLabeler
	// Elector campaigns for the allocator term (Store.Elector(ElectionName)).
	Elector store.Elector
	// PodName is this replica (node.name).
	PodName string
	// Interface receives the relayed packets (dhcp.interface, default eth0).
	Interface string
	// ServerIdentifier is the LoadBalancer address relays send to
	// (dhcp.serverIP), advertised as option 54.
	ServerIdentifier netip.Addr
	// DataDir holds the local working copies (dhcp-kube/<scope>.db), which
	// are rebuilt from DHCPLease objects at every start.
	DataDir string
	Logger  *slog.Logger
	// Now is the clock of the janitor; default time.Now.
	Now func() time.Time
	// Interval paces the janitor, status updates and listener retries;
	// default 30s.
	Interval time.Duration
	// LocalAddress returns the IPv4 address of Interface; default reads
	// the interface. Tests override it.
	LocalAddress func(iface string) (netip.Addr, error)
	// Listen binds a service; default (*dhcp.Service).Listen. Tests
	// override it.
	Listen func(*dhcp.Service) error
	// Serve runs a bound service until ctx ends; default
	// (*dhcp.Service).Run. Tests override it.
	Serve func(context.Context, *dhcp.Service) error
}

type leaseEntry struct {
	scope string
	lease dhcp.ExternalLease
	rv    string
}

// pendingWrite is this replica's own write of an object. Watch events for
// that object are skipped until the event of the write itself arrives:
// events before it are older, and the watch delivers events in order.
type pendingWrite struct {
	rv      string
	deleted bool
	at      time.Time
}

// generation is one dhcp.Service and its stores. A changed scope set or
// topology replaces the whole generation; option and reservation edits
// apply to the running one.
type generation struct {
	service   *dhcp.Service
	stores    map[string]*dhcp.LeaseStore
	configs   map[string]dhcp.Config
	revisions map[string]uint64
	cancel    context.CancelFunc
	done      chan struct{}
}

// Runtime is the DHCP service of one replica on the Kubernetes store.
type Runtime struct {
	opts   Options
	log    *slog.Logger
	leader atomic.Bool

	mu        sync.Mutex // the active generation and the scope view
	gen       *generation
	published atomic.Pointer[[]*dhcp.LeaseStore]
	scopes    map[string]*v1alpha1.DHCPScope
	invalid   map[string]string
	listenErr string
	serverIP  netip.Addr

	leaseMu sync.Mutex // the lease cache, pending and in-flight writes
	cache   map[string]leaseEntry
	pending map[string]pendingWrite
	// inflight marks objects this replica is writing or rereading; watch
	// events for them wait until the cache and pending entry are recorded.
	inflight map[string]chan struct{}

	kick        chan struct{}
	scopesReady chan struct{}
	leasesReady chan struct{}
	scopesOnce  sync.Once
	leasesOnce  sync.Once
}

// New validates opts.
func New(opts Options) (*Runtime, error) {
	if opts.Store == nil || opts.Leases == nil || opts.Elector == nil {
		return nil, errors.New("dhcpkube: store, leases and elector are required")
	}
	if opts.PodName == "" || opts.DataDir == "" || !opts.ServerIdentifier.Is4() {
		return nil, errors.New("dhcpkube: pod name, data directory and an IPv4 server identifier are required")
	}
	if opts.Interface == "" {
		opts.Interface = "eth0"
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Interval <= 0 {
		opts.Interval = defaultInterval
	}
	if opts.LocalAddress == nil {
		opts.LocalAddress = interfaceAddress
	}
	if opts.Listen == nil {
		opts.Listen = (*dhcp.Service).Listen
	}
	if opts.Serve == nil {
		opts.Serve = func(ctx context.Context, s *dhcp.Service) error { return s.Run(ctx) }
	}
	return &Runtime{opts: opts, log: opts.Logger.With("component", "dhcp"),
		scopes: map[string]*v1alpha1.DHCPScope{}, invalid: map[string]string{},
		cache: map[string]leaseEntry{}, pending: map[string]pendingWrite{}, inflight: map[string]chan struct{}{},
		kick: make(chan struct{}, 1), scopesReady: make(chan struct{}), leasesReady: make(chan struct{})}, nil
}

func interfaceAddress(name string) (netip.Addr, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return netip.Addr{}, err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return netip.Addr{}, err
	}
	for _, a := range addrs {
		if prefix, err := netip.ParsePrefix(a.String()); err == nil && prefix.Addr().Unmap().Is4() {
			return prefix.Addr().Unmap(), nil
		}
	}
	return netip.Addr{}, fmt.Errorf("interface %s has no IPv4 address", name)
}

func (r *Runtime) trigger() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Run serves until ctx ends. It is an always-on runnable: every replica
// keeps its working copy, and the elected one answers DHCP.
func (r *Runtime) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() { r.watchScopes(ctx) })
	wg.Go(func() { r.watchLeases(ctx) })
	defer func() {
		cancel()
		wg.Wait()
		r.mu.Lock()
		r.stop(r.gen)
		r.setGen(nil)
		r.mu.Unlock()
	}()
	for _, ready := range []chan struct{}{r.scopesReady, r.leasesReady} {
		select {
		case <-ready:
		case <-ctx.Done():
			return nil
		}
	}
	wg.Go(func() {
		if err := r.opts.Elector.Run(ctx, r.term, r.stepDown); err != nil {
			r.log.Error("DHCP election stopped", "error", err)
		}
	})
	ticker := time.NewTicker(r.opts.Interval)
	defer ticker.Stop()
	r.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.kick:
			r.reconcile(ctx)
			if r.leader.Load() {
				// Report an applied change at once, not at the next tick.
				r.writeStatuses(ctx)
			}
		case <-ticker.C:
			r.reconcile(ctx)
		}
	}
}

// Leader reports whether this replica serves DHCP.
func (r *Runtime) Leader() bool { return r.leader.Load() }

// LeaderName is the replica that serves DHCP, as last observed, or "".
func (r *Runtime) LeaderName() string {
	if r.leader.Load() {
		return r.opts.PodName
	}
	return r.opts.Elector.Leader()
}

func (r *Runtime) serving(context.Context) error {
	if !r.leader.Load() {
		return dhcp.ErrNotServing
	}
	return nil
}

// --- DHCPScope objects ---

func (r *Runtime) watchScopes(ctx context.Context) {
	backoff := 100 * time.Millisecond
	for ctx.Err() == nil {
		w, err := r.opts.Store.Watch(ctx, v1alpha1.KindDHCPScope, store.WatchOptions{})
		if err != nil {
			r.log.Warn("DHCPScope watch failed; retrying", "error", err, "after", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, 30*time.Second)
			continue
		}
		backoff = 100 * time.Millisecond
		r.scopeSession(ctx, w)
		w.Stop()
	}
}

func (r *Runtime) scopeSession(ctx context.Context, w store.Watcher) {
	listing, seen := true, map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.ResultChan():
			if !ok {
				return
			}
			switch ev.Type {
			case store.Bookmark:
				if listing {
					listing = false
					r.mu.Lock()
					for name := range r.scopes {
						if !seen[name] {
							delete(r.scopes, name)
						}
					}
					r.mu.Unlock()
					r.scopesOnce.Do(func() { close(r.scopesReady) })
					r.trigger()
				}
			case store.Added, store.Modified:
				scope, ok := ev.Object.(*v1alpha1.DHCPScope)
				if !ok {
					continue
				}
				if listing {
					seen[scope.Name] = true
				}
				r.mu.Lock()
				old := r.scopes[scope.Name]
				r.scopes[scope.Name] = scope.DeepCopy()
				r.mu.Unlock()
				if !listing && (old == nil || old.Generation != scope.Generation || !reflect.DeepEqual(old.Spec, scope.Spec)) {
					r.trigger()
				}
			case store.Deleted:
				if scope, ok := ev.Object.(*v1alpha1.DHCPScope); ok {
					r.mu.Lock()
					delete(r.scopes, scope.Name)
					r.mu.Unlock()
					r.trigger()
				}
			}
		}
	}
}

// ScopeConfig converts a DHCPScope into the runtime configuration this
// replica serves: relay-only, dashboard-managed, on the pod interface,
// advertising the LoadBalancer address.
func ScopeConfig(scope *v1alpha1.DHCPScope, iface string, serverIP, identifier netip.Addr) (dhcp.Config, error) {
	s := scope.Spec
	c := config.DHCPScope{Name: scope.Name, Management: "api", Interface: iface, ServerIP: serverIP.String(),
		Subnet: s.Subnet, RelayOnly: true, RangeStart: s.RangeStart, RangeEnd: s.RangeEnd, Router: s.Router,
		DNSServers: s.DNSServers, DomainName: s.DomainName, LeaseDuration: config.Duration(s.LeaseDuration.Duration),
		RegisterDNS: s.RegisterDNS, DNSTTL: config.Duration(s.DNSTTL.Duration), ConflictDetection: s.ConflictDetection,
		ProbeTimeout: config.Duration(s.ProbeTimeout.Duration), NTPServers: s.NTPServers, DomainSearch: s.DomainSearch,
		BootServer: s.BootServer, BootServerName: s.BootServerName, BootFile: s.BootFile}
	for _, relay := range s.TrustedRelays {
		c.TrustedRelays = append(c.TrustedRelays, config.DHCPTrustedRelay{SourceIP: relay.SourceIP, GatewayIP: relay.GatewayIP,
			SourcePort: uint16(relay.SourcePort), LinkSelection: relay.LinkSelection, ServerIdentifier: relay.ServerIdentifier}) // #nosec G115 -- the CRD bounds the port
	}
	for _, route := range s.ClasslessRoutes {
		c.ClasslessRoutes = append(c.ClasslessRoutes, config.DHCPClasslessRoute{Destination: route.Destination, Router: route.Router})
	}
	for _, p := range s.BootProfiles {
		c.BootProfiles = append(c.BootProfiles, config.DHCPBootProfile{Architecture: uint16(p.Architecture), BootServer: p.BootServer, BootServerName: p.BootServerName, BootFile: p.BootFile}) // #nosec G115 -- the CRD bounds the architecture
	}
	for _, res := range s.Reservations {
		c.Reservations = append(c.Reservations, config.DHCPReservation{MAC: res.MAC, IP: res.IP, Hostname: res.Hostname})
	}
	cfg, err := c.Runtime("spec")
	if err != nil {
		return dhcp.Config{}, err
	}
	cfg.ServerIdentifier = identifier
	if err := cfg.Validate(); err != nil {
		return dhcp.Config{}, fmt.Errorf("spec: %w", err)
	}
	return cfg, nil
}

func revisionOf(scope *v1alpha1.DHCPScope) uint64 {
	return uint64(max(scope.Generation, 1)) // #nosec G115 -- positive
}

// desired converts every scope in name order, leaving out invalid ones and
// any scope whose subnet or relays conflict with an earlier scope.
func (r *Runtime) desired() (map[string]dhcp.Config, map[string]uint64, map[string]string) {
	names := make([]string, 0, len(r.scopes))
	for name := range r.scopes {
		names = append(names, name)
	}
	sort.Strings(names)
	configs, revisions, invalid := map[string]dhcp.Config{}, map[string]uint64{}, map[string]string{}
	var accepted []dhcp.Config
	for _, name := range names {
		cfg, err := ScopeConfig(r.scopes[name], r.opts.Interface, r.serverIP, r.opts.ServerIdentifier)
		if err == nil {
			err = dhcp.ValidateTopology(append(slices.Clone(accepted), cfg))
		}
		if err != nil {
			invalid[name] = err.Error()
			continue
		}
		accepted = append(accepted, cfg)
		configs[name], revisions[name] = cfg, revisionOf(r.scopes[name])
	}
	return configs, revisions, invalid
}

// reconcile brings the generation in line with the scopes.
func (r *Runtime) reconcile(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.serverIP.IsValid() {
		ip, err := r.opts.LocalAddress(r.opts.Interface)
		if err != nil {
			r.listenErr = err.Error()
			r.log.Error("DHCP interface unavailable", "interface", r.opts.Interface, "error", err)
			return
		}
		r.serverIP = ip
	}
	configs, revisions, invalid := r.desired()
	for name, msg := range invalid {
		if r.invalid[name] != msg {
			r.log.Warn("DHCP scope not served", "scope", name, "error", msg)
		}
	}
	r.invalid = invalid
	gen := r.gen
	rebuild := gen == nil && len(configs) > 0 || gen != nil && (len(gen.configs) != len(configs) || gen.service == nil && len(configs) > 0)
	if !rebuild && gen != nil {
		for name, cfg := range configs {
			old, ok := gen.configs[name]
			if !ok || !dhcp.SameExternalTopology(old, cfg) {
				rebuild = true
				break
			}
		}
	}
	if gen != nil && gen.done != nil {
		select {
		case <-gen.done:
			rebuild = true // the listener stopped
		default:
		}
	}
	if rebuild {
		r.rebuild(ctx, configs, revisions)
		return
	}
	if gen == nil {
		return
	}
	for name, cfg := range configs {
		if gen.revisions[name] == revisions[name] && reflect.DeepEqual(gen.configs[name], cfg) {
			continue
		}
		leader := r.leader.Load()
		err := gen.service.MutateScope(ctx, name, func(ctx context.Context, s *dhcp.LeaseStore) error {
			return s.SetExternalConfig(ctx, cfg, revisions[name], leader)
		})
		if err != nil {
			r.log.Error("DHCP scope settings not applied", "scope", name, "error", err)
			continue
		}
		gen.configs[name], gen.revisions[name] = cfg, revisions[name]
		r.log.Info("DHCP scope settings applied", "scope", name, "revision", revisions[name])
	}
}

// setGen installs gen and publishes its stores for lock-free DNS reads.
func (r *Runtime) setGen(gen *generation) {
	r.gen = gen
	var stores []*dhcp.LeaseStore
	if gen != nil {
		names := make([]string, 0, len(gen.stores))
		for name := range gen.stores {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			stores = append(stores, gen.stores[name])
		}
	}
	r.published.Store(&stores)
}

func (r *Runtime) stop(gen *generation) {
	if gen == nil {
		return
	}
	if gen.service != nil {
		_ = gen.service.Close()
	}
	if gen.cancel != nil {
		gen.cancel()
		<-gen.done
	}
	for _, s := range gen.stores {
		_ = s.Close()
	}
}

// rebuild replaces the generation. The working copies are new files filled
// from the lease cache; DHCPLease objects are the only durable state.
func (r *Runtime) rebuild(ctx context.Context, configs map[string]dhcp.Config, revisions map[string]uint64) {
	r.stop(r.gen)
	r.setGen(nil)
	r.listenErr = ""
	if len(configs) == 0 {
		return
	}
	dir := filepath.Join(r.opts.DataDir, "dhcp-kube")
	if err := os.RemoveAll(dir); err != nil {
		r.listenErr = err.Error()
		return
	}
	gen := &generation{stores: map[string]*dhcp.LeaseStore{}, configs: map[string]dhcp.Config{}, revisions: map[string]uint64{}}
	fail := func(err error) {
		r.listenErr = err.Error()
		r.log.Error("DHCP service not started", "error", err)
		r.stop(gen)
	}
	names := make([]string, 0, len(configs))
	list := make([]dhcp.Config, 0, len(configs))
	for name := range configs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cfg := configs[name]
		s, err := dhcp.OpenExternalLeases(filepath.Join(dir, name+".db"), cfg, r.persister(name))
		if err != nil {
			fail(fmt.Errorf("scope %s: %w", name, err))
			return
		}
		gen.stores[name] = s
		if err := s.SetExternalConfig(ctx, cfg, revisions[name], false); err != nil {
			fail(fmt.Errorf("scope %s: %w", name, err))
			return
		}
		if err := s.ResetExternal(ctx, r.scopeLeases(name)); err != nil {
			fail(fmt.Errorf("scope %s: %w", name, err))
			return
		}
		if r.leader.Load() {
			if err := s.SetExternalConfig(ctx, cfg, revisions[name], true); err != nil {
				r.log.Warn("DHCP claims not ranked", "scope", name, "error", err)
			}
		}
		gen.configs[name], gen.revisions[name] = cfg, revisions[name]
		list = append(list, cfg)
	}
	service, err := dhcp.NewService(list, gen.stores, r.opts.Logger)
	if err != nil {
		fail(err)
		return
	}
	service.SetServingCheck(r.serving)
	if err := r.opts.Listen(service); err != nil {
		_ = service.Close()
		// Keep the working copies: the API and lease DNS still need them.
		r.setGen(gen)
		r.listenErr = err.Error()
		r.log.Error("DHCP listener not started; retrying", "interface", r.opts.Interface, "error", err)
		return
	}
	gen.service = service
	runCtx, cancel := context.WithCancel(ctx)
	gen.cancel, gen.done = cancel, make(chan struct{})
	go func() {
		defer close(gen.done)
		if err := r.opts.Serve(runCtx, service); err != nil {
			r.log.Error("DHCP listener stopped", "error", err)
			r.trigger()
		}
	}()
	r.setGen(gen)
	r.log.Info("DHCP scopes ready", "scopes", names, "interface", r.opts.Interface, "serverIdentifier", r.opts.ServerIdentifier)
}

// mutate runs fn on a scope's store, serialized with its packets.
func (r *Runtime) mutate(ctx context.Context, scope string, fn func(context.Context, *dhcp.LeaseStore) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mutateLocked(ctx, scope, fn)
}

func (r *Runtime) mutateLocked(ctx context.Context, scope string, fn func(context.Context, *dhcp.LeaseStore) error) error {
	gen := r.gen
	if gen == nil || gen.stores[scope] == nil {
		return fmt.Errorf("dhcp: unknown scope %q", scope)
	}
	if gen.service == nil {
		// No listener: nothing else writes these stores.
		return fn(ctx, gen.stores[scope])
	}
	return gen.service.MutateScope(ctx, scope, fn)
}

// --- DHCPLease objects ---

func (r *Runtime) scopeLeases(scope string) []dhcp.ExternalLease {
	r.leaseMu.Lock()
	defer r.leaseMu.Unlock()
	var out []dhcp.ExternalLease
	for _, e := range r.cache {
		if e.scope == scope {
			out = append(out, e.lease)
		}
	}
	return out
}

// watchLeases lists once, then resumes the watch from the last
// resourceVersion it saw; only an expired watch (410 Gone) or a failed
// resume lists again.
func (r *Runtime) watchLeases(ctx context.Context) {
	backoff := 100 * time.Millisecond
	rv := ""
	for ctx.Err() == nil {
		var err error
		if rv == "" {
			var items []dhcpv1.DHCPLease
			if items, rv, err = r.opts.Leases.List(ctx); err == nil {
				r.replaceLeases(ctx, items)
				r.leasesOnce.Do(func() { close(r.leasesReady) })
			}
		}
		if err == nil {
			var w watch.Interface
			if w, err = r.opts.Leases.Watch(ctx, rv); err == nil {
				backoff = 100 * time.Millisecond
				rv = r.leaseSession(ctx, w, rv)
				w.Stop()
				continue
			}
			rv = "" // the resume failed: list again
		}
		if ctx.Err() != nil {
			return
		}
		r.log.Warn("DHCPLease watch failed; retrying", "error", err, "after", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 30*time.Second)
	}
}

// leaseSession applies events until the watch ends and returns the
// resourceVersion to resume from, or "" when the watch expired.
func (r *Runtime) leaseSession(ctx context.Context, w watch.Interface, rv string) string {
	for {
		select {
		case <-ctx.Done():
			return rv
		case ev, ok := <-w.ResultChan():
			if !ok {
				return rv
			}
			switch ev.Type {
			case watch.Added, watch.Modified, watch.Deleted:
				if obj, ok := ev.Object.(*dhcpv1.DHCPLease); ok {
					r.observe(ctx, obj, ev.Type == watch.Deleted)
					rv = obj.ResourceVersion
				}
			case watch.Bookmark:
				if obj, ok := ev.Object.(*dhcpv1.DHCPLease); ok && obj.ResourceVersion != "" {
					rv = obj.ResourceVersion
				}
			case watch.Error:
				return ""
			}
		}
	}
}

// replaceLeases installs a complete list. Writes of this replica that the
// list does not show yet stay in the cache until their own events arrive.
func (r *Runtime) replaceLeases(ctx context.Context, items []dhcpv1.DHCPLease) {
	next := map[string]leaseEntry{}
	for i := range items {
		scope, lease, err := leaseFromObject(&items[i])
		if err != nil {
			r.log.Warn("DHCPLease ignored", "name", items[i].Name, "error", err)
			continue
		}
		next[items[i].Name] = leaseEntry{scope: scope, lease: lease, rv: items[i].ResourceVersion}
	}
	r.leaseMu.Lock()
	now := r.opts.Now()
	for name, p := range r.pending {
		listed, ok := next[name]
		switch {
		case now.Sub(p.at) > pendingWindow:
			delete(r.pending, name)
		case p.deleted && !ok, !p.deleted && ok && listed.rv == p.rv:
			delete(r.pending, name)
		case p.deleted:
			delete(next, name)
		default:
			if own, ok := r.cache[name]; ok {
				next[name] = own
			}
		}
	}
	r.cache = next
	r.leaseMu.Unlock()
	r.resetStores(ctx)
}

// resetStores refills every working copy from the cache. Each store reads
// the cache while holding its scope's packet lock, so no write of this
// replica is half-way through.
func (r *Runtime) resetStores(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gen == nil {
		return
	}
	for name := range r.gen.stores {
		err := r.mutateLocked(ctx, name, func(ctx context.Context, s *dhcp.LeaseStore) error {
			return s.ResetExternal(ctx, r.scopeLeases(name))
		})
		if err != nil {
			r.log.Error("DHCP working copy not refreshed", "scope", name, "error", err)
		}
	}
}

// observe applies one watch event unless it predates this replica's own
// write of the object.
func (r *Runtime) observe(ctx context.Context, obj *dhcpv1.DHCPLease, deleted bool) {
	name := obj.Name
	r.leaseMu.Lock()
	// While this replica writes the object, the pending entry that tells its
	// own events from older ones does not exist yet: wait for the write.
	for {
		done, writing := r.inflight[name]
		if !writing {
			break
		}
		r.leaseMu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return
		}
		r.leaseMu.Lock()
	}
	if p, ok := r.pending[name]; ok {
		if deleted && p.deleted || !deleted && obj.ResourceVersion == p.rv {
			delete(r.pending, name)
			r.leaseMu.Unlock()
			return
		}
		if r.opts.Now().Sub(p.at) <= pendingWindow {
			r.leaseMu.Unlock()
			return
		}
		delete(r.pending, name)
	}
	var scope string
	var lease dhcp.ExternalLease
	if deleted {
		old, ok := r.cache[name]
		delete(r.cache, name)
		r.leaseMu.Unlock()
		if !ok {
			return
		}
		scope = old.scope
		lease = old.lease
	} else {
		var err error
		scope, lease, err = leaseFromObject(obj)
		if err != nil {
			r.leaseMu.Unlock()
			r.log.Warn("DHCPLease ignored", "name", name, "error", err)
			return
		}
		r.cache[name] = leaseEntry{scope: scope, lease: lease, rv: obj.ResourceVersion}
		r.leaseMu.Unlock()
	}
	rv := obj.ResourceVersion
	err := r.mutate(ctx, scope, func(ctx context.Context, s *dhcp.LeaseStore) error {
		// Decide under the scope's packet lock: a write that committed
		// while this event waited updated the cache and the store already,
		// and applying the event now would revert it.
		if !r.cached(name, rv, deleted) {
			return nil
		}
		if deleted {
			return s.SyncExternal(ctx, nil, []netip.Addr{lease.Record.Address})
		}
		return s.SyncExternal(ctx, []dhcp.ExternalLease{lease}, nil)
	})
	if err != nil && r.knownScope(scope) {
		r.log.Warn("DHCP working copy not updated", "lease", name, "error", err)
	}
}

// cached reports whether the cache still holds the object at rv, or, for a
// delete, holds no object.
func (r *Runtime) cached(name, rv string, deleted bool) bool {
	r.leaseMu.Lock()
	defer r.leaseMu.Unlock()
	e, ok := r.cache[name]
	if deleted {
		return !ok
	}
	return ok && e.rv == rv
}

// begin marks name in flight and returns the cache entry the write is based
// on; end records the outcome and releases waiting watch events.
func (r *Runtime) begin(name string) (leaseEntry, bool, chan struct{}) {
	r.leaseMu.Lock()
	defer r.leaseMu.Unlock()
	for {
		if done, busy := r.inflight[name]; busy {
			r.leaseMu.Unlock()
			<-done
			r.leaseMu.Lock()
			continue
		}
		break
	}
	done := make(chan struct{})
	r.inflight[name] = done
	entry, known := r.cache[name]
	return entry, known, done
}

func (r *Runtime) end(name string, done chan struct{}, record func()) {
	r.leaseMu.Lock()
	defer r.leaseMu.Unlock()
	if record != nil {
		record()
	}
	delete(r.inflight, name)
	close(done)
}

func (r *Runtime) knownScope(scope string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gen != nil && r.gen.stores[scope] != nil
}

// persister commits a scope's changes as DHCPLease writes: deletes first,
// then updates and creates, each with a resourceVersion precondition. It
// runs inside the store's transaction and the scope's packet lock, so it
// takes only leaseMu. On failure the transaction rolls back and the touched
// objects are reread.
func (r *Runtime) persister(scope string) dhcp.ExternalPersister {
	return func(ctx context.Context, changes []dhcp.ExternalChange) error {
		ordered := slices.Clone(changes)
		sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Lease == nil && ordered[j].Lease != nil })
		names := make([]string, 0, len(ordered))
		for _, c := range ordered {
			names = append(names, ObjectName(scope, c.Address))
		}
		for i, c := range ordered {
			name := names[i]
			entry, known, done := r.begin(name)
			var err error
			var record func()
			if c.Lease == nil {
				if known {
					err = r.opts.Leases.Delete(ctx, name, entry.rv)
					if err == nil || errors.Is(err, ErrNotFound) {
						err = nil
						record = func() {
							delete(r.cache, name)
							r.pending[name] = pendingWrite{deleted: true, at: r.opts.Now()}
						}
					}
				}
			} else {
				obj := leaseObject("", scope, *c.Lease)
				if known {
					obj.ResourceVersion = entry.rv
					err = r.opts.Leases.Update(ctx, obj)
				} else {
					err = r.opts.Leases.Create(ctx, obj)
				}
				if err == nil {
					lease := *c.Lease
					record = func() {
						r.cache[name] = leaseEntry{scope: scope, lease: lease, rv: obj.ResourceVersion}
						r.pending[name] = pendingWrite{rv: obj.ResourceVersion, at: r.opts.Now()}
					}
				}
			}
			r.end(name, done, record)
			if err != nil {
				go r.reread(context.WithoutCancel(ctx), scope, names)
				return fmt.Errorf("dhcp lease %s: %w", name, err)
			}
		}
		return nil
	}
}

// reread replaces cached objects with their current state after a failed
// write, then applies them to the working copy once the failed
// transaction has released the scope.
func (r *Runtime) reread(ctx context.Context, scope string, names []string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	type reread struct {
		name, rv string
		lease    dhcp.ExternalLease
		address  netip.Addr
		deleted  bool
	}
	var found []reread
	for _, name := range names {
		// In flight: watch events for name wait until the cache holds the
		// current object, then apply on top of it.
		_, _, done := r.begin(name)
		obj, err := r.opts.Leases.Get(ctx, name)
		var record func()
		switch {
		case errors.Is(err, ErrNotFound):
			record = func() {
				delete(r.pending, name)
				if old, ok := r.cache[name]; ok {
					found = append(found, reread{name: name, address: old.lease.Record.Address, deleted: true})
					delete(r.cache, name)
				} else if ip, ok := addressOf(scope, name); ok {
					found = append(found, reread{name: name, address: ip, deleted: true})
				}
			}
		case err != nil:
			r.log.Warn("DHCPLease reread failed; the next list repairs it", "lease", name, "error", err)
		default:
			if s, lease, err := leaseFromObject(obj); err == nil && s == scope {
				record = func() {
					delete(r.pending, name)
					r.cache[name] = leaseEntry{scope: scope, lease: lease, rv: obj.ResourceVersion}
					found = append(found, reread{name: name, rv: obj.ResourceVersion, lease: lease})
				}
			}
		}
		r.end(name, done, record)
	}
	if len(found) == 0 {
		return
	}
	if err := r.mutate(ctx, scope, func(ctx context.Context, s *dhcp.LeaseStore) error {
		var upserts []dhcp.ExternalLease
		var deletes []netip.Addr
		for _, f := range found {
			if !r.cached(f.name, f.rv, f.deleted) {
				continue // a newer write or event already applied
			}
			if f.deleted {
				deletes = append(deletes, f.address)
			} else {
				upserts = append(upserts, f.lease)
			}
		}
		return s.SyncExternal(ctx, upserts, deletes)
	}); err != nil {
		r.log.Warn("DHCP working copy not repaired", "scope", scope, "error", err)
	}
}

func addressOf(scope, name string) (netip.Addr, bool) {
	if len(name) <= len(scope)+1 || name[:len(scope)+1] != scope+"." {
		return netip.Addr{}, false
	}
	ip, err := netip.ParseAddr(name[len(scope)+1:])
	return ip, err == nil
}

// --- The allocator term ---

// term serves DHCP while this replica holds the kudurru-dhcp Lease: rebuild
// the working copies from a consistent list, rank DNS claims, take the
// Service's endpoint, then keep the leases and statuses tidy.
func (r *Runtime) term(ctx context.Context) {
	r.log.Info("DHCP allocator term started", "pod", r.opts.PodName)
	for ctx.Err() == nil {
		items, _, err := r.opts.Leases.List(ctx)
		if err == nil {
			r.replaceLeases(ctx, items)
			break
		}
		r.log.Warn("DHCPLease list failed before serving; retrying", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	r.mu.Lock()
	if gen := r.gen; gen != nil {
		for name, cfg := range gen.configs {
			err := r.mutateLocked(ctx, name, func(ctx context.Context, s *dhcp.LeaseStore) error {
				return s.SetExternalConfig(ctx, cfg, gen.revisions[name], true)
			})
			if err != nil {
				r.log.Warn("DHCP claims not ranked", "scope", name, "error", err)
			}
		}
	}
	r.leader.Store(true)
	r.mu.Unlock()
	r.label(ctx)
	ticker := time.NewTicker(r.opts.Interval)
	defer ticker.Stop()
	r.maintain(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.label(ctx)
			r.maintain(ctx)
		}
	}
}

func (r *Runtime) stepDown() {
	r.leader.Store(false)
	r.log.Info("DHCP allocator term ended", "pod", r.opts.PodName)
	if r.opts.Pods == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.opts.Pods.SetActive(ctx, r.opts.PodName, false); err != nil {
		r.log.Warn("DHCP endpoint label not removed", "pod", r.opts.PodName, "error", err)
	}
}

// label points the DHCP Service at this pod only.
func (r *Runtime) label(ctx context.Context) {
	if r.opts.Pods == nil {
		return
	}
	active, err := r.opts.Pods.ActivePods(ctx)
	if err != nil {
		r.log.Warn("DHCP endpoint pods not listed", "error", err)
		return
	}
	for _, pod := range active {
		if pod != r.opts.PodName {
			if err := r.opts.Pods.SetActive(ctx, pod, false); err != nil {
				r.log.Warn("DHCP endpoint label not removed", "pod", pod, "error", err)
			}
		}
	}
	if !slices.Contains(active, r.opts.PodName) {
		if err := r.opts.Pods.SetActive(ctx, r.opts.PodName, true); err != nil {
			r.log.Warn("DHCP endpoint label not set", "pod", r.opts.PodName, "error", err)
		}
	}
}

// maintain deletes expired lease objects and writes scope statuses.
func (r *Runtime) maintain(ctx context.Context) {
	cutoff := r.opts.Now().Add(-expiredGrace)
	r.leaseMu.Lock()
	type expired struct{ name, rv string }
	var stale []expired
	for name, e := range r.cache {
		if _, writing := r.pending[name]; !writing && e.lease.Record.ExpiresAt.Before(cutoff) {
			stale = append(stale, expired{name, e.rv})
		}
	}
	r.leaseMu.Unlock()
	for _, e := range stale {
		err := r.opts.Leases.Delete(ctx, e.name, e.rv)
		if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrConflict) {
			r.log.Warn("expired DHCPLease not deleted", "lease", e.name, "error", err)
		}
	}
	r.writeStatuses(ctx)
}

// writeStatuses records which replica serves each scope and what it holds.
func (r *Runtime) writeStatuses(ctx context.Context) {
	r.mu.Lock()
	type update struct {
		scope  *v1alpha1.DHCPScope
		status v1alpha1.DHCPScopeStatus
	}
	var updates []update
	now := metav1.NewTime(r.opts.Now())
	for name, scope := range r.scopes {
		status := v1alpha1.DHCPScopeStatus{ObservedGeneration: scope.Generation, ServingPod: r.opts.PodName,
			Conditions: slices.Clone(scope.Status.Conditions)}
		cond := metav1.Condition{Type: v1alpha1.ConditionReady, ObservedGeneration: scope.Generation, LastTransitionTime: now}
		var s *dhcp.LeaseStore
		if r.gen != nil {
			s = r.gen.stores[name]
		}
		switch {
		case r.invalid[name] != "":
			cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, v1alpha1.ReasonInvalid, r.invalid[name]
		case s == nil || r.gen.service == nil:
			cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, v1alpha1.ReasonNotServing, "DHCP listener not started: "+r.listenErr
		default:
			leases, err := s.List(ctx)
			if err != nil {
				r.mu.Unlock()
				r.log.Warn("DHCP scope status not computed", "scope", name, "error", err)
				return
			}
			for _, l := range leases {
				if l.State == "bound" {
					status.ActiveLeases++
				}
			}
			status.ConflictingLeases = s.ConflictingLeases()
			cond.Status, cond.Reason, cond.Message = metav1.ConditionTrue, v1alpha1.ReasonServing, "serving through "+r.opts.ServerIdentifier.String()
			if status.ConflictingLeases > 0 {
				cond.Reason = v1alpha1.ReasonLeaseConflict
				cond.Message = fmt.Sprintf("%d live leases are not allowed by this spec; they are not renewed and end at expiry", status.ConflictingLeases)
			}
		}
		meta.SetStatusCondition(&status.Conditions, cond)
		if !statusEqual(scope.Status, status) {
			updates = append(updates, update{scope, status})
		}
	}
	r.mu.Unlock()
	for _, u := range updates {
		next := u.scope.DeepCopy()
		next.ResourceVersion = ""
		next.Status = u.status
		if err := r.opts.Store.UpdateStatus(ctx, next); err != nil && !errors.Is(err, store.ErrNotFound) {
			r.log.Warn("DHCP scope status not written", "scope", u.scope.Name, "error", err)
		}
	}
}

func statusEqual(a, b v1alpha1.DHCPScopeStatus) bool {
	if a.ObservedGeneration != b.ObservedGeneration || a.ServingPod != b.ServingPod || a.ActiveLeases != b.ActiveLeases ||
		a.ConflictingLeases != b.ConflictingLeases || len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for _, c := range a.Conditions {
		d := meta.FindStatusCondition(b.Conditions, c.Type)
		if d == nil || d.Status != c.Status || d.Reason != c.Reason || d.Message != c.Message || d.ObservedGeneration != c.ObservedGeneration {
			return false
		}
	}
	return true
}

// --- Accessors for the API and DNS ---

// Store returns a scope's working copy for reads.
func (r *Runtime) Store(scope string) (*dhcp.LeaseStore, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gen == nil || r.gen.stores[scope] == nil {
		return nil, fmt.Errorf("dhcp: unknown scope %q", scope)
	}
	return r.gen.stores[scope], nil
}

// Stores returns every working copy, for lease DNS records. It never
// blocks: a DNS query must not wait for a scope rebuild or a lease write.
func (r *Runtime) Stores() []*dhcp.LeaseStore {
	if stores := r.published.Load(); stores != nil {
		return *stores
	}
	return nil
}

// Import merges leases into a scope through DHCPLease writes. Any replica
// may import; resourceVersion preconditions keep it safe beside the
// allocator.
func (r *Runtime) Import(ctx context.Context, scope, format string, data []byte) (dhcp.ImportResult, error) {
	var result dhcp.ImportResult
	err := r.mutate(ctx, scope, func(ctx context.Context, s *dhcp.LeaseStore) error {
		var err error
		result, err = s.Import(ctx, format, data)
		return err
	})
	return result, err
}

// Status describes this replica's DHCP service.
type Status struct {
	Interface  string
	Leader     string
	Serving    bool
	Listening  bool
	Error      string
	Scopes     []string
	Invalid    map[string]string
	ServerIP   netip.Addr
	Identifier netip.Addr
}

// Status reports the scopes, the serving replica and any startup error.
func (r *Runtime) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := Status{Interface: r.opts.Interface, Leader: r.LeaderName(), Serving: r.leader.Load(), Error: r.listenErr, Invalid: map[string]string{},
		ServerIP: r.serverIP, Identifier: r.opts.ServerIdentifier}
	for name, msg := range r.invalid {
		st.Invalid[name] = msg
	}
	if r.gen != nil {
		st.Listening = r.gen.service != nil
		for name := range r.gen.stores {
			st.Scopes = append(st.Scopes, name)
		}
		sort.Strings(st.Scopes)
	}
	return st
}

// Config returns the configuration a scope would run with, for dashboard
// validation of a candidate spec.
func (r *Runtime) Config(scope *v1alpha1.DHCPScope) (dhcp.Config, error) {
	r.mu.Lock()
	ip := r.serverIP
	r.mu.Unlock()
	if !ip.IsValid() {
		ip = r.opts.ServerIdentifier
	}
	return ScopeConfig(scope, r.opts.Interface, ip, r.opts.ServerIdentifier)
}
