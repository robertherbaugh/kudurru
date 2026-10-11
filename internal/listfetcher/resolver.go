package listfetcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
)

// InitStrategy says how a node treats its blocklists at start
// (blocking.initStrategy; blocky's loading.strategy).
type InitStrategy string

// Init strategies.
const (
	// InitFast serves at once with whatever the disk cache holds and loads
	// the rest in the background; readiness does not wait for lists.
	InitFast InitStrategy = "fast"
	// InitBlocking reports not ready until every source has a body or has
	// failed definitively (unless AllowDegraded).
	InitBlocking InitStrategy = "blocking"
	// InitFailOnError is InitBlocking, and Run returns an error (the
	// process exits) when a source failed definitively during the initial
	// load.
	InitFailOnError InitStrategy = "failOnError"
)

// ParseInitStrategy validates a blocking.initStrategy value; "" is fast.
func ParseInitStrategy(s string) (InitStrategy, error) {
	switch InitStrategy(s) {
	case "", InitFast:
		return InitFast, nil
	case InitBlocking, InitFailOnError:
		return InitStrategy(s), nil
	}
	return "", fmt.Errorf("listfetcher: unknown init strategy %q (fast, blocking, failOnError)", s)
}

// Resolver defaults.
const (
	// DefaultFallbackAfter is how long a node waits for the leader before
	// downloading sources itself (§4.6, §7.3, D15).
	DefaultFallbackAfter = 10 * time.Minute
	// DefaultRetryInterval is the pass interval while something is
	// pending.
	DefaultRetryInterval = 30 * time.Second
	// idleInterval is the pass interval when everything is current; store
	// changes trigger passes in between.
	idleInterval = 10 * time.Minute
)

// ErrInitialLoad is returned by Run under InitFailOnError.
var ErrInitialLoad = errors.New("listfetcher: initial blocklist load failed")

// ListSink receives compiled lists: zonecache.Cache.SetLists.
type ListSink interface {
	SetLists(*blocking.Snapshot)
}

// ResolverOptions configure a Resolver.
type ResolverOptions struct {
	// Objects supplies the BlockLists with the leader's status digests;
	// required.
	Objects Objects
	// Cache is the node's list cache; required.
	Cache *DiskCache
	// Sink receives every compiled snapshot; required.
	Sink ListSink
	// Membership finds the leader; default cluster.Self (single node).
	Membership cluster.Membership
	// Client pulls bodies from the leader's internal listener; nil
	// disables pulls (single node).
	Client *internalapi.Client
	// Downloader fetches sources directly after FallbackAfter; nil
	// disables the fallback.
	Downloader *Downloader
	// RefreshInterval and MinRefreshInterval as for the Fetcher; they
	// pace self-fetches during a fallback.
	RefreshInterval, MinRefreshInterval time.Duration
	// InitStrategy; default InitFast.
	InitStrategy InitStrategy
	// AllowDegraded keeps the node ready while InitBlocking or
	// InitFailOnError are still loading (blocking.allowDegraded).
	AllowDegraded bool
	// FallbackAfter; default DefaultFallbackAfter.
	FallbackAfter time.Duration
	// RetryInterval; default DefaultRetryInterval.
	RetryInterval time.Duration
	// MaxListBytes caps a body pulled from the leader; default
	// DefaultMaxHTTPSize.
	MaxListBytes int64
	// Compile tunes blocking.Rebuild.
	Compile blocking.CompileOptions
	// Registerer receives kudurru_blocklist_pulls_total and friends.
	Registerer prometheus.Registerer
	Logger     *slog.Logger
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// Resolver runs on every node: it turns the BlockLists and their published
// digests into list bodies (local cache, else the leader by digest, else,
// after FallbackAfter, its own download), compiles them with
// blocking.Rebuild and hands the snapshot to zonecache.
type Resolver struct {
	opts ResolverOptions
	log  *slog.Logger
	m    *resolverMetrics

	passMu             sync.Mutex // serialises passes; guards the fields below
	pendingSince       map[string]time.Time
	leaderFailingSince time.Time
	selfNext           map[string]time.Time
	failed             map[string]string
	lastFP             string
	published          bool
	snap               *blocking.Snapshot

	mu          sync.Mutex // guards the reporting state
	initialDone bool
	loadedCh    chan struct{}
	lists       []internalapi.ListStatus
	waiting     []string
	notes       []string
}

// NewResolver returns a Resolver. It does no I/O.
func NewResolver(opts ResolverOptions) (*Resolver, error) {
	switch {
	case opts.Objects == nil:
		return nil, errors.New("listfetcher: ResolverOptions.Objects is required")
	case opts.Cache == nil:
		return nil, errors.New("listfetcher: ResolverOptions.Cache is required")
	case opts.Sink == nil:
		return nil, errors.New("listfetcher: ResolverOptions.Sink is required")
	}
	if opts.Membership == nil {
		opts.Membership = cluster.Self{}
	}
	if opts.InitStrategy == "" {
		opts.InitStrategy = InitFast
	}
	if _, err := ParseInitStrategy(string(opts.InitStrategy)); err != nil {
		return nil, err
	}
	if opts.RefreshInterval <= 0 {
		opts.RefreshInterval = DefaultRefreshInterval
	}
	if opts.MinRefreshInterval <= 0 {
		opts.MinRefreshInterval = DefaultMinRefreshInterval
	}
	if opts.FallbackAfter <= 0 {
		opts.FallbackAfter = DefaultFallbackAfter
	}
	if opts.RetryInterval <= 0 {
		opts.RetryInterval = DefaultRetryInterval
	}
	if opts.MaxListBytes <= 0 {
		opts.MaxListBytes = DefaultMaxHTTPSize
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	m, err := newResolverMetrics(opts.Registerer)
	if err != nil {
		return nil, err
	}
	return &Resolver{
		opts: opts, log: opts.Logger.With("component", "listresolver"), m: m,
		pendingSince: map[string]time.Time{}, selfNext: map[string]time.Time{}, failed: map[string]string{},
		loadedCh: make(chan struct{}),
	}, nil
}

// NeedLeaderElection marks the Resolver as always-on.
func (r *Resolver) NeedLeaderElection() bool { return false }

// PassResult summarises one resolve pass.
type PassResult struct {
	// Lists is the number of BlockLists seen.
	Lists int
	// Waiting counts sources without any body that have not failed
	// definitively; the initial load is complete when it is zero.
	Waiting int
	// Pending counts sources that are not at their published digest.
	Pending int
	// Failures describes sources that failed definitively.
	Failures []string
	// Rebuilt is true when a new snapshot went to the sink.
	Rebuilt bool
}

// Run resolves until ctx ends: first a pass from the disk cache only (so
// a restarted node serves its last lists at once), then passes after
// every BlockList change, every RetryInterval while something is pending
// and every 10 minutes otherwise.
func (r *Resolver) Run(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case <-r.opts.Objects.Initialized():
	}
	changes := r.opts.Objects.Subscribe()
	r.pass(ctx, true)
	for {
		res := r.pass(ctx, false)
		if ctx.Err() != nil {
			return nil
		}
		if err := r.finishInitial(res); err != nil {
			return err
		}
		wait := idleInterval
		if res.Waiting > 0 || res.Pending > 0 {
			wait = r.opts.RetryInterval
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-changes:
		case <-t.C:
		}
		t.Stop()
	}
}

// finishInitial marks the initial load complete once nothing is waiting
// and applies InitFailOnError.
func (r *Resolver) finishInitial(res PassResult) error {
	if res.Waiting > 0 {
		return nil
	}
	r.mu.Lock()
	first := !r.initialDone
	if first {
		r.initialDone = true
		close(r.loadedCh)
	}
	r.mu.Unlock()
	if first {
		r.log.Info("blocklists loaded", "lists", res.Lists, "pending", res.Pending, "failures", len(res.Failures))
		if r.opts.InitStrategy == InitFailOnError && len(res.Failures) > 0 {
			return fmt.Errorf("%w: %v", ErrInitialLoad, res.Failures)
		}
	}
	return nil
}

// Loaded is closed when the initial load is complete.
func (r *Resolver) Loaded() <-chan struct{} { return r.loadedCh }

// Ready implements health.ReadyFunc for the §5.2 rule "blocklists loaded
// or blocking.allowDegraded".
func (r *Resolver) Ready() (bool, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.initialDone {
		return true, slices.Clone(r.notes)
	}
	msgs := append([]string{"blocklists still loading"}, r.waiting...)
	switch {
	case r.opts.InitStrategy == InitFast:
		msgs[0] = "blocklists loading in the background (initStrategy fast)"
		return true, msgs
	case r.opts.AllowDegraded:
		msgs[0] = "blocklists still loading (blocking.allowDegraded)"
		return true, msgs
	}
	return false, msgs
}

// Lists reports the compiled lists and their source digests, for
// GET /internal/status.
func (r *Resolver) Lists() []internalapi.ListStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.lists)
}

// Resolve runs one online pass and applies the initial-load rules; it is
// what Run does on every wake-up, exported for tests and tools.
func (r *Resolver) Resolve(ctx context.Context) (PassResult, error) {
	res := r.pass(ctx, false)
	return res, r.finishInitial(res)
}

// leaderLookup resolves the leader at most once per pass.
type leaderLookup struct {
	done bool
	peer cluster.Peer
	err  error
}

// pass resolves every source and republishes the snapshot when the inputs
// changed. offline passes use the disk cache only.
func (r *Resolver) pass(ctx context.Context, offline bool) PassResult {
	r.passMu.Lock()
	defer r.passMu.Unlock()
	now := r.opts.Now()
	var res PassResult
	var leader leaderLookup
	var specs []blocking.Spec
	var statuses []internalapi.ListStatus
	var opened openedSources
	defer opened.close()
	var waiting, notes []string
	referenced := map[string]bool{}
	fp := &fpBuilder{}

	for _, bl := range r.opts.Objects.BlockLists() {
		res.Lists++
		pl := plan(bl, r.opts.RefreshInterval, r.opts.MinRefreshInterval)
		spec := blocking.Spec{Name: pl.name, Mode: pl.mode}
		ls := internalapi.ListStatus{Name: pl.name}
		fp.add("list", pl.name, string(pl.mode))
		for i := range pl.sources {
			ps := &pl.sources[i]
			if ps.key == "" {
				d := inlineDigest(ps.inline)
				spec.Sources = append(spec.Sources, blocking.LinesSource(ps.name, ps.format, ps.inline))
				ls.Digests = append(ls.Digests, d)
				fp.add(ps.name, string(ps.format), d)
				continue
			}
			referenced[ps.key] = true
			var wanted string
			if ps.status != nil {
				wanted = ps.status.Digest
			}
			e, have := r.opts.Cache.Lookup(ps.key)
			current := have && (wanted == "" || e.Digest == wanted)
			if !current && !offline {
				e, have, current = r.obtain(ctx, &pl, ps, wanted, now, &leader)
			}
			if current {
				delete(r.pendingSince, ps.key)
			} else {
				res.Pending++
				if _, ok := r.pendingSince[ps.key]; !ok {
					r.pendingSince[ps.key] = now
				}
			}
			if !have {
				if msg, ok := r.failed[ps.key]; ok {
					res.Failures = append(res.Failures, fmt.Sprintf("%s source %q: %s", pl.name, ps.name, msg))
				} else {
					res.Waiting++
					waiting = append(waiting, fmt.Sprintf("%s source %q: %s", pl.name, ps.name, r.waitReason(wanted, &leader)))
				}
				fp.add(ps.name, string(ps.format), "missing")
				continue
			}
			f, fe, err := r.opts.Cache.Open(ps.key)
			if err != nil {
				res.Waiting++
				waiting = append(waiting, fmt.Sprintf("%s source %q: cache: %v", pl.name, ps.name, err))
				continue
			}
			if !current {
				notes = append(notes, fmt.Sprintf("%s source %q: serving an older body while the published one is pending", pl.name, ps.name))
			}
			spec.Sources = append(spec.Sources, opened.fileSource(ps.name, ps, f, fe.Size))
			ls.Digests = append(ls.Digests, fe.Digest)
			fp.add(ps.name, string(ps.format), fe.Digest)
		}
		if len(spec.Sources) > 0 {
			specs = append(specs, spec)
			statuses = append(statuses, ls)
		}
	}
	for _, f := range res.Failures {
		notes = append(notes, "failed: "+f)
	}
	r.m.pending.Set(float64(res.Pending))

	if !r.published || fp.String() != r.lastFP {
		snap, reports := blocking.Rebuild(r.snap, specs, r.opts.Compile)
		for _, rep := range reports {
			if rep.Err != nil {
				r.log.Warn("blocklist compile", "list", rep.Name, "keptOld", rep.KeptOld, "err", rep.Err)
			}
		}
		r.snap, r.lastFP, r.published = snap, fp.String(), true
		r.opts.Sink.SetLists(snap)
		r.m.rebuilds.Inc()
		res.Rebuilt = true
		r.log.Info("blocklists compiled", "lists", len(snap.Lists()), "entries", snap.Entries(), "generation", snap.Generation())
		r.mu.Lock()
		r.lists = statuses
		r.mu.Unlock()
	}
	r.mu.Lock()
	r.waiting, r.notes = waiting, notes
	r.mu.Unlock()

	if !offline && r.opts.Objects.Live() {
		for _, key := range r.opts.Cache.Keys() {
			if !referenced[key] {
				r.opts.Cache.Remove(key)
				delete(r.pendingSince, key)
				delete(r.selfNext, key)
				delete(r.failed, key)
			}
		}
	}
	return res
}

func (r *Resolver) waitReason(wanted string, leader *leaderLookup) string {
	switch {
	case wanted == "":
		return "waiting for the leader to publish a digest"
	case leader.done && leader.err != nil:
		return "leader unreachable: " + leader.err.Error()
	}
	return "waiting for the leader"
}

// obtain tries the leader by digest, then (after FallbackAfter) a direct
// download. It returns the entry now cached for the source.
func (r *Resolver) obtain(ctx context.Context, pl *plannedList, ps *plannedSource, wanted string, now time.Time, leader *leaderLookup) (Entry, bool, bool) {
	selfLeads := false
	if wanted != "" {
		if !leader.done {
			leader.peer, leader.err = r.opts.Membership.Leader(ctx)
			leader.done = true
		}
		switch {
		case leader.err != nil:
			r.leaderFailed(now)
		case leader.peer.Self:
			// This node's fetcher writes the cache; nothing to pull.
			selfLeads = true
		case r.opts.Client != nil:
			e, err := r.pull(ctx, ps, wanted, leader.peer, now)
			if err == nil {
				return e, true, true
			}
			if !errors.Is(err, internalapi.ErrListNotFound) && ctx.Err() == nil {
				// The leader is unreachable: do not dial it again for every
				// other source in this pass.
				leader.err = err
			}
		}
	}
	fallback := r.opts.Downloader != nil && r.fallbackDue(ps.key, now) && !now.Before(r.selfNext[ps.key])
	if fallback && !selfLeads {
		if !leader.done {
			leader.peer, leader.err = r.opts.Membership.Leader(ctx)
			leader.done = true
		}
		// On the leader the fetcher owns downloads, digest or not.
		selfLeads = leader.err == nil && leader.peer.Self
	}
	if fallback && !selfLeads {
		res, err := r.opts.Downloader.Fetch(ctx, ps.download(), OriginSelf)
		if ctx.Err() != nil {
			e, have := r.opts.Cache.Lookup(ps.key)
			return e, have, false
		}
		if err != nil {
			r.failed[ps.key] = truncate(err.Error(), maxStatusError)
			r.selfNext[ps.key] = now.Add(max(r.opts.RetryInterval*4, time.Minute))
			r.m.pulls.WithLabelValues(OriginSelf, "error").Inc()
			r.log.Warn("blocklist fallback download failed", "list", pl.name, "source", redact(ps.key), "err", err)
		} else {
			delete(r.failed, ps.key)
			r.selfNext[ps.key] = now.Add(pl.refresh)
			r.m.pulls.WithLabelValues(OriginSelf, "ok").Inc()
			r.log.Info("blocklist downloaded without the leader", "list", pl.name, "source", redact(ps.key), "digest", res.Entry.Digest)
		}
	} else if ps.cm != nil && wanted == "" && (r.opts.Downloader == nil || r.opts.Downloader.opts.ConfigMaps == nil) {
		r.failed[ps.key] = fmt.Sprintf("%v: configMapRef needs the kube store driver", ErrUnsupported)
	}
	e, have := r.opts.Cache.Lookup(ps.key)
	return e, have, have && (wanted == "" || e.Digest == wanted)
}

// pull downloads the body with digest wanted from the leader into the
// cache; the sha256 is verified before the body replaces anything.
func (r *Resolver) pull(ctx context.Context, ps *plannedSource, wanted string, leader cluster.Peer, now time.Time) (Entry, error) {
	key := ps.key
	// Keep the published ETag so this node can revalidate conditionally
	// should it become the leader.
	var etag string
	if ps.status != nil {
		etag = ps.status.ETag
	}
	rc, _, err := r.opts.Client.OpenList(ctx, leader.Address, wanted)
	if err == nil {
		var e Entry
		e, _, err = r.opts.Cache.Put(key, rc, PutOptions{
			MaxSize: r.opts.MaxListBytes, ExpectDigest: wanted, Origin: OriginLeader, Fetched: now, ETag: etag,
		})
		_ = rc.Close()
		if err == nil {
			r.leaderFailingSince = time.Time{}
			delete(r.failed, key)
			r.m.pulls.WithLabelValues(OriginLeader, "ok").Inc()
			return e, nil
		}
	}
	switch {
	case errors.Is(err, internalapi.ErrListNotFound):
		// The leader answered but no longer (or not yet) holds the body.
		r.leaderFailingSince = time.Time{}
		r.m.pulls.WithLabelValues(OriginLeader, "not_found").Inc()
	default:
		r.leaderFailed(now)
		r.m.pulls.WithLabelValues(OriginLeader, "error").Inc()
		r.log.Debug("blocklist pull from leader failed", "leader", leader.Name, "address", leader.Address, "digest", wanted, "err", err)
	}
	return Entry{}, err
}

func (r *Resolver) leaderFailed(now time.Time) {
	if r.leaderFailingSince.IsZero() {
		r.leaderFailingSince = now
	}
}

// fallbackDue: the leader has been unreachable for FallbackAfter, or this
// source has been pending that long.
func (r *Resolver) fallbackDue(key string, now time.Time) bool {
	if !r.leaderFailingSince.IsZero() && now.Sub(r.leaderFailingSince) >= r.opts.FallbackAfter {
		return true
	}
	since, ok := r.pendingSince[key]
	return ok && now.Sub(since) >= r.opts.FallbackAfter
}
