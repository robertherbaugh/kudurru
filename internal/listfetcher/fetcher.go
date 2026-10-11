package listfetcher

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Fetcher defaults.
const (
	DefaultRefreshInterval    = 24 * time.Hour
	DefaultMinRefreshInterval = time.Minute
	DefaultConcurrency        = 4
	DefaultCooldown           = time.Minute
	DefaultMaxCooldown        = time.Hour
	// maxIdle bounds how long Run sleeps without a change.
	maxIdle = time.Hour
	// maxStatusError bounds status.sources[].error.
	maxStatusError = 512
	// statusWriteAttempts bounds conflict retries of one status write.
	statusWriteAttempts = 5
)

// Options configure the leader's Fetcher.
type Options struct {
	// Store receives status writes (UpdateStatus); required.
	Store store.Store
	// Objects supplies the BlockLists; required.
	Objects Objects
	// Cache is the shared list cache; required.
	Cache *DiskCache
	// Downloader fetches sources into Cache; required.
	Downloader *Downloader
	// RefreshInterval applies to lists without spec.refreshInterval
	// (blocking.refreshInterval); default DefaultRefreshInterval.
	RefreshInterval time.Duration
	// MinRefreshInterval is a floor for every list; default
	// DefaultMinRefreshInterval.
	MinRefreshInterval time.Duration
	// Concurrency bounds parallel downloads; default DefaultConcurrency.
	Concurrency int
	// Cooldown is the delay before retrying a failed source, doubled per
	// consecutive failure up to MaxCooldown.
	Cooldown, MaxCooldown time.Duration
	// Compile tunes the compile that counts entries for status.
	Compile blocking.CompileOptions
	// Registerer receives the kudurru_blocklist_* metrics; nil skips them.
	Registerer prometheus.Registerer
	Logger     *slog.Logger
	// Now is the clock; default time.Now.
	Now func() time.Time
}

type sourceState struct {
	failures  int
	nextTry   time.Time
	lastErr   string
	attempted bool // a download ran in this term
}

type listStats struct {
	fp                        string
	entries, ruleCount, inval int
	perSource                 map[string][2]int // name -> {rules, invalid}
}

// Fetcher is the leader-only runnable that downloads every BlockList
// source on its refresh interval and publishes digests, entry counts and
// fetch errors into BlockList.status (§4.6, §7.3, D15). It owns only
// status.sources, status.entries, status.ruleCount, status.invalid and
// status.lastFetched; observedGeneration and the conditions belong to the
// controller, which derives Degraded{ListFetchFailed} from
// status.sources[].error.
type Fetcher struct {
	opts Options
	log  *slog.Logger
	m    *fetcherMetrics

	mu      sync.Mutex
	state   map[string]*sourceState // by cache key
	stats   map[string]listStats    // by list name
	metered map[string]bool
}

// New returns a Fetcher. It does no I/O.
func New(opts Options) (*Fetcher, error) {
	switch {
	case opts.Store == nil:
		return nil, errors.New("listfetcher: Options.Store is required")
	case opts.Objects == nil:
		return nil, errors.New("listfetcher: Options.Objects is required")
	case opts.Cache == nil:
		return nil, errors.New("listfetcher: Options.Cache is required")
	case opts.Downloader == nil:
		return nil, errors.New("listfetcher: Options.Downloader is required")
	}
	if opts.RefreshInterval <= 0 {
		opts.RefreshInterval = DefaultRefreshInterval
	}
	if opts.MinRefreshInterval <= 0 {
		opts.MinRefreshInterval = DefaultMinRefreshInterval
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.Cooldown <= 0 {
		opts.Cooldown = DefaultCooldown
	}
	if opts.MaxCooldown < opts.Cooldown {
		opts.MaxCooldown = max(DefaultMaxCooldown, opts.Cooldown)
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	m, err := newFetcherMetrics(opts.Registerer)
	if err != nil {
		return nil, err
	}
	return &Fetcher{
		opts: opts, log: opts.Logger.With("component", "listfetcher"), m: m,
		state: map[string]*sourceState{}, stats: map[string]listStats{}, metered: map[string]bool{},
	}, nil
}

// NeedLeaderElection marks the Fetcher as leader-only.
func (f *Fetcher) NeedLeaderElection() bool { return true }

// Run reconciles until ctx ends: at start, after every BlockList change
// and when the next source is due. Each term starts with empty in-memory
// state; refresh timers resume from status.sources[].lastFetched.
func (f *Fetcher) Run(ctx context.Context) error {
	f.mu.Lock()
	f.state = map[string]*sourceState{}
	f.stats = map[string]listStats{}
	f.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil
	case <-f.opts.Objects.Initialized():
	}
	changes := f.opts.Objects.Subscribe()
	for {
		next := f.Reconcile(ctx)
		wait := max(next.Sub(f.opts.Now()), time.Second)
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

type keyPlan struct {
	src      Source
	refresh  time.Duration
	lists    []string
	statusAt time.Time // latest status lastFetched that matches the cached digest
	digest   string    // status digest seen with statusAt
}

// Reconcile runs one pass: download every due source (bounded
// concurrency), then publish the owned status fields of every list whose
// view changed. It returns when the next source is due.
func (f *Fetcher) Reconcile(ctx context.Context) time.Time {
	now := f.opts.Now()
	next := now.Add(maxIdle)
	var plans []plannedList
	keys := map[string]*keyPlan{}
	var order []string
	for _, bl := range f.opts.Objects.BlockLists() {
		pl := plan(bl, f.opts.RefreshInterval, f.opts.MinRefreshInterval)
		plans = append(plans, pl)
		for i := range pl.sources {
			ps := &pl.sources[i]
			if ps.key == "" {
				continue
			}
			kp := keys[ps.key]
			if kp == nil {
				kp = &keyPlan{src: ps.download(), refresh: pl.refresh}
				keys[ps.key] = kp
				order = append(order, ps.key)
			}
			kp.refresh = min(kp.refresh, pl.refresh)
			kp.lists = append(kp.lists, pl.name)
			if ps.status != nil && ps.status.LastFetched != nil && ps.status.LastFetched.After(kp.statusAt) {
				kp.statusAt, kp.digest = ps.status.LastFetched.Time, ps.status.Digest
			}
		}
	}

	var due []string
	for _, key := range order {
		kp := keys[key]
		at := f.dueAt(key, kp, now)
		if !at.After(now) {
			due = append(due, key)
		} else if at.Before(next) {
			next = at
		}
	}

	if len(due) > 0 {
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(f.opts.Concurrency)
		for _, key := range due {
			kp := keys[key]
			g.Go(func() error {
				f.fetch(gctx, kp)
				return nil
			})
		}
		_ = g.Wait()
		if ctx.Err() != nil {
			return next
		}
		for _, key := range due {
			if at := f.dueAt(key, keys[key], f.opts.Now()); at.Before(next) {
				next = at
			}
		}
	}

	seen := map[string]bool{}
	for i := range plans {
		seen[plans[i].name] = true
		if err := f.publish(ctx, &plans[i]); err != nil && ctx.Err() == nil {
			f.log.Warn("listfetcher: status write failed", "list", plans[i].name, "err", err)
			if retry := f.opts.Now().Add(f.opts.Cooldown); retry.Before(next) {
				next = retry
			}
		}
	}
	f.mu.Lock()
	for name := range f.metered {
		if !seen[name] {
			f.m.forget(name)
			delete(f.metered, name)
			delete(f.stats, name)
		}
	}
	f.mu.Unlock()
	return next
}

// dueAt is when key should be downloaded next: immediately without a
// cached body, else one refresh interval (less a per-URL jitter of up to
// 5 %) after the last success, and never before a failure's cooldown.
func (f *Fetcher) dueAt(key string, kp *keyPlan, now time.Time) time.Time {
	at := now
	if e, ok := f.opts.Cache.Lookup(key); ok {
		last := e.Fetched
		if kp.digest == e.Digest && kp.statusAt.After(last) {
			last = kp.statusAt
		}
		at = last.Add(kp.refresh - jitter(key, kp.refresh))
	}
	f.mu.Lock()
	st := f.state[key]
	f.mu.Unlock()
	if st != nil && st.nextTry.After(at) {
		at = st.nextTry
	}
	return at
}

// jitter spreads refreshes of different URLs deterministically over the
// last 5 % of the interval.
func jitter(key string, refresh time.Duration) time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return refresh / 20 * time.Duration(h.Sum32()%1000) / 1000
}

func (f *Fetcher) fetch(ctx context.Context, kp *keyPlan) {
	start := f.opts.Now()
	res, err := f.opts.Downloader.Fetch(ctx, kp.src, OriginFetch)
	if ctx.Err() != nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.state[kp.src.Key]
	if st == nil {
		st = &sourceState{}
		f.state[kp.src.Key] = st
	}
	st.attempted = true
	if err != nil {
		st.failures++
		st.lastErr = truncate(err.Error(), maxStatusError)
		backoff := f.opts.Cooldown << min(st.failures-1, 16)
		st.nextTry = start.Add(min(backoff, f.opts.MaxCooldown))
		for _, l := range kp.lists {
			f.m.failures.WithLabelValues(l).Inc()
		}
		_, cached := f.opts.Cache.Lookup(kp.src.Key)
		f.log.Warn("listfetcher: download failed", "source", redact(kp.src.Key), "lists", kp.lists, "failures", st.failures, "keptOld", cached, "retryAt", st.nextTry, "err", err)
		return
	}
	st.failures, st.lastErr, st.nextTry = 0, "", time.Time{}
	f.log.Info("listfetcher: source fetched", "source", redact(kp.src.Key), "lists", kp.lists,
		"changed", res.Changed, "notModified", res.NotModified, "digest", res.Entry.Digest, "bytes", res.Entry.Size)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// ownedStatus is the subset of BlockListStatus the fetcher writes.
type ownedStatus struct {
	Sources     []v1alpha1.SourceStatus
	Entries     int
	RuleCount   int
	Invalid     int
	LastFetched *metav1.Time
}

func ownedOf(s *v1alpha1.BlockListStatus) ownedStatus {
	return ownedStatus{Sources: s.Sources, Entries: s.Entries, RuleCount: s.RuleCount, Invalid: s.Invalid, LastFetched: s.LastFetched}
}

func (o ownedStatus) applyTo(s *v1alpha1.BlockListStatus) {
	s.Sources, s.Entries, s.RuleCount, s.Invalid, s.LastFetched = o.Sources, o.Entries, o.RuleCount, o.Invalid, o.LastFetched
}

func (o ownedStatus) equal(p ownedStatus) bool {
	norm := func(x ownedStatus) ownedStatus {
		if len(x.Sources) == 0 {
			x.Sources = nil
		}
		return x
	}
	return equality.Semantic.DeepEqual(norm(o), norm(p))
}

func timeOf(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	mt := metav1.NewTime(t.UTC().Truncate(time.Second))
	return &mt
}

// publish computes the owned status of one list and writes it when it
// differs from what the object carries.
func (f *Fetcher) publish(ctx context.Context, pl *plannedList) error {
	var owned ownedStatus
	fp := &fpBuilder{}
	fp.add(string(pl.mode))
	spec := blocking.Spec{Name: pl.name, Mode: pl.mode}
	var opened openedSources
	defer opened.close()
	stale := false
	f.mu.Lock()
	for i := range pl.sources {
		ps := &pl.sources[i]
		ss := v1alpha1.SourceStatus{Name: ps.name}
		if ps.key == "" {
			ss.Digest = inlineDigest(ps.inline)
			spec.Sources = append(spec.Sources, blocking.LinesSource(ps.name, ps.format, ps.inline))
		} else {
			st := f.state[ps.key]
			switch {
			case st != nil && st.attempted:
				ss.Error = st.lastErr
			case ps.status != nil:
				// Not attempted in this term yet: keep the previous verdict so
				// Degraded does not flap on a leader change.
				ss.Error = ps.status.Error
			}
			if file, e, err := f.opts.Cache.Open(ps.key); err == nil {
				spec.Sources = append(spec.Sources, opened.fileSource(ps.name, ps, file, e.Size))
				ss.Digest, ss.ETag = e.Digest, e.ETag
				ss.LastFetched = timeOf(e.Fetched)
				if ps.status != nil && ps.status.Digest == e.Digest && ps.status.LastFetched != nil &&
					(ss.LastFetched == nil || ps.status.LastFetched.After(ss.LastFetched.Time)) {
					ss.LastFetched = ps.status.LastFetched.DeepCopy()
				}
			}
			if ss.Error != "" || ss.Digest == "" {
				stale = true
			}
		}
		fp.add(ps.name, string(ps.format), ss.Digest)
		owned.Sources = append(owned.Sources, ss)
		if ss.LastFetched != nil && (owned.LastFetched == nil || ss.LastFetched.After(owned.LastFetched.Time)) {
			owned.LastFetched = ss.LastFetched.DeepCopy()
		}
	}
	cached, haveCached := f.stats[pl.name]
	f.mu.Unlock()

	stats, err := f.listStats(pl, fp.String(), spec, cached, haveCached)
	if err != nil {
		f.log.Warn("listfetcher: compile for status", "list", pl.name, "err", err)
	}
	owned.Entries, owned.RuleCount, owned.Invalid = stats.entries, stats.ruleCount, stats.inval
	for i := range owned.Sources {
		if c, ok := stats.perSource[owned.Sources[i].Name]; ok {
			owned.Sources[i].Entries, owned.Sources[i].Invalid = c[0], c[1]
		}
	}

	f.mu.Lock()
	f.stats[pl.name] = stats
	f.metered[pl.name] = true
	f.mu.Unlock()
	f.m.entries.WithLabelValues(pl.name).Set(float64(owned.Entries))
	if owned.LastFetched != nil {
		f.m.lastUpdate.WithLabelValues(pl.name).Set(float64(owned.LastFetched.Unix()))
	}
	f.m.stale.WithLabelValues(pl.name).Set(boolGauge(stale))

	if owned.equal(ownedOf(&pl.obj.Status)) {
		return nil
	}
	return f.writeStatus(ctx, pl, owned)
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// listStats returns the counts for status: from this term's cache when the
// inputs are unchanged, from the object's own status when its digests
// already match (a new leader does not recompile everything), else by
// compiling the list once.
func (f *Fetcher) listStats(pl *plannedList, fp string, spec blocking.Spec, cached listStats, haveCached bool) (listStats, error) {
	if haveCached && cached.fp == fp {
		return cached, nil
	}
	if s, ok := statsFromStatus(pl, fp); ok {
		return s, nil
	}
	out := listStats{fp: fp, perSource: map[string][2]int{}}
	if len(spec.Sources) == 0 {
		return out, nil
	}
	l, err := blocking.Compile(spec, nil, f.opts.Compile)
	if l == nil {
		return out, err
	}
	st := l.Stats()
	out.entries, out.ruleCount, out.inval = st.Entries, st.RuleCount, st.Invalid
	for _, s := range l.Sources() {
		out.perSource[s.Name] = [2]int{s.Rules, s.Invalid}
	}
	return out, err
}

// statsFromStatus reuses the published counts when every source digest in
// the status equals the current one.
func statsFromStatus(pl *plannedList, fp string) (listStats, bool) {
	st := &pl.obj.Status
	if len(st.Sources) != len(pl.sources) || st.RuleCount == 0 {
		return listStats{}, false
	}
	check := &fpBuilder{}
	check.add(string(pl.mode))
	out := listStats{fp: fp, entries: st.Entries, ruleCount: st.RuleCount, inval: st.Invalid, perSource: map[string][2]int{}}
	for i, ps := range pl.sources {
		ss := st.Sources[i]
		if ss.Name != ps.name {
			return listStats{}, false
		}
		check.add(ps.name, string(ps.format), ss.Digest)
		out.perSource[ss.Name] = [2]int{ss.Entries, ss.Invalid}
	}
	if check.String() != fp {
		return listStats{}, false
	}
	return out, true
}

// writeStatus re-reads the object and replaces only the owned fields,
// retrying on resourceVersion conflicts with the controller's writes.
func (f *Fetcher) writeStatus(ctx context.Context, pl *plannedList, owned ownedStatus) error {
	key := store.Key{Kind: store.KindOf(pl.obj), Namespace: pl.obj.Namespace, Name: pl.obj.Name}
	for range statusWriteAttempts {
		cur := &v1alpha1.BlockList{}
		if err := f.opts.Store.Get(ctx, key, cur); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			return err
		}
		if cur.Generation != pl.obj.Generation || !slices.EqualFunc(cur.Spec.Sources, pl.obj.Spec.Sources, sameSource) {
			return nil // the spec moved on; the next pass sees the new object
		}
		if owned.equal(ownedOf(&cur.Status)) {
			return nil
		}
		owned.applyTo(&cur.Status)
		err := f.opts.Store.UpdateStatus(ctx, cur)
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		return err
	}
	return fmt.Errorf("listfetcher: status of %s: %w after %d attempts", key, store.ErrConflict, statusWriteAttempts)
}

func sameSource(a, b v1alpha1.ListSource) bool { return equality.Semantic.DeepEqual(a, b) }
