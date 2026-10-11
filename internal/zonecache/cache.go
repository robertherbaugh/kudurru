package zonecache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
)

// Defaults from docs/architecture.md §4.4.
const (
	DefaultDebounce        = 50 * time.Millisecond
	DefaultPersistInterval = 30 * time.Second
	DefaultRefreshInterval = 5 * time.Minute
	DefaultStartupTimeout  = 15 * time.Second
	DefaultMaxAge          = 24 * time.Hour
)

// Watch retry backoff bounds.
const (
	minRetry = 100 * time.Millisecond
	maxRetry = 5 * time.Second
)

// finalPersistTimeout bounds the local.db write on shutdown.
const finalPersistTimeout = 5 * time.Second

// Builder derives one view from the object set; see the package
// documentation for the full contract.
type Builder interface {
	// Name is the stable, unique name of the view.
	Name() string
	// Build returns the view for objects. On error the previous view is
	// kept and Degraded{BuildFailed} is raised.
	Build(objects ObjectSet) (any, error)
}

// NewBuilder adapts a function to a Builder.
func NewBuilder(name string, build func(ObjectSet) (any, error)) Builder {
	return funcBuilder{name: name, build: build}
}

type funcBuilder struct {
	name  string
	build func(ObjectSet) (any, error)
}

func (b funcBuilder) Name() string                         { return b.name }
func (b funcBuilder) Build(objects ObjectSet) (any, error) { return b.build(objects) }

// Options configures a Cache. The zero value watches every kudurru.io kind
// with the §4.4 defaults and neither persists nor cold-starts.
type Options struct {
	// Kinds to watch. Default DefaultKinds().
	Kinds []store.Kind
	// Debounce is the window after a change during which further changes
	// are folded into the same rebuild. Default 50 ms.
	Debounce time.Duration
	// LocalDB enables the local.db snapshot: the live set is written after
	// every sync and then at most every PersistInterval when it changed,
	// and it is loaded when the store has not synced within
	// StartupTimeout. The caller opens and closes it. Nil disables both
	// (Docker mode, where state.db is local; §4.5).
	LocalDB *localdb.SnapshotStore
	// PersistInterval is the minimum spacing of change-driven local.db
	// writes. Default 30 s.
	PersistInterval time.Duration
	// RefreshInterval: while live with no changes, the stored snapshot's
	// timestamp is refreshed this often so its age measures time since the
	// set was last known current rather than time since the last edit.
	// Default 5 min.
	RefreshInterval time.Duration
	// StartupTimeout is how long Start waits for the store to sync before
	// loading local.db (store.kube.startupTimeout). Default 15 s.
	StartupTimeout time.Duration
	// MaxAge is the oldest local.db snapshot that keeps the Cache ready
	// and primary zones answering (store.kube.snapshot.maxAge). Default
	// 24 h.
	MaxAge time.Duration
	// Registerer receives the zonecache metrics. Nil: not registered.
	Registerer prometheus.Registerer
	// Logger receives logs. Default slog.Default().
	Logger *slog.Logger
}

func (o *Options) setDefaults() {
	if len(o.Kinds) == 0 {
		o.Kinds = DefaultKinds()
	}
	if o.Debounce <= 0 {
		o.Debounce = DefaultDebounce
	}
	if o.PersistInterval <= 0 {
		o.PersistInterval = DefaultPersistInterval
	}
	if o.RefreshInterval <= 0 {
		o.RefreshInterval = DefaultRefreshInterval
	}
	if o.StartupTimeout <= 0 {
		o.StartupTimeout = DefaultStartupTimeout
	}
	if o.MaxAge <= 0 {
		o.MaxAge = DefaultMaxAge
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// DefaultKinds returns every kind registered in the kudurru.io/v1alpha1
// scheme that has a matching list type, sorted: a kind added to
// api/v1alpha1 is watched without touching zonecache.
func DefaultKinds() []store.Kind {
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		panic(fmt.Sprintf("zonecache: v1alpha1 scheme: %v", err))
	}
	known := s.KnownTypes(v1alpha1.GroupVersion)
	var kinds []store.Kind
	for name, typ := range known {
		listTyp, ok := known[name+"List"]
		if !ok {
			continue
		}
		obj := reflect.New(typ).Interface()
		if _, isList := obj.(metav1.ListInterface); isList {
			continue
		}
		if _, ok := obj.(store.Object); !ok {
			continue
		}
		if _, ok := reflect.New(listTyp).Interface().(metav1.ListInterface); !ok {
			continue
		}
		kinds = append(kinds, store.Kind(name))
	}
	slices.Sort(kinds)
	return kinds
}

// Cache materialises the store into serving snapshots. Create it with New,
// register builders, and run Start (an always-on runnable). Every method is
// safe for concurrent use.
type Cache struct {
	st    store.Store
	opts  Options
	kinds []store.Kind
	log   *slog.Logger
	m     *cacheMetrics

	current     atomic.Pointer[Snapshot]
	kick        chan struct{} // cap 1: a rebuild is wanted
	persistNow  chan struct{} // cap 1: a sync completed; write local.db now
	initialized chan struct{}
	initOnce    sync.Once
	started     atomic.Bool
	emptyIdx    *index

	mu           sync.Mutex
	objects      map[store.Kind]map[store.Key]store.Object // the live set
	rvs          map[store.Kind]string
	kindSynced   map[store.Kind]bool
	syncedAll    bool
	changed      map[store.Kind]bool // kinds changed since the last rebuild
	persistDirty bool
	lastConfirm  time.Time // last local.db write or refresh
	state        State
	snapIdx      *index    // the served local.db snapshot (FromSnapshot)
	snapWritten  time.Time // its WrittenAt
	noSnapshot   string    // why the fallback found nothing, for Ready
	lists        *blocking.Snapshot
	listsDirty   bool
	builders     []Builder
	builderNames map[string]bool
	conditions   map[string]Condition
	hooks        []func(*Snapshot)

	// Owned by the rebuild goroutine.
	liveIdx     *index
	lastState   State
	views       map[string]any
	expiryTick  bool
	firstBuilt  bool
	rebuildLock sync.Mutex // serialises rebuild (tests call it directly)
}

// New returns a Cache over st. It does no I/O; Start does.
func New(st store.Store, opts Options) (*Cache, error) {
	if st == nil {
		return nil, errors.New("zonecache: nil store")
	}
	opts.setDefaults()
	kinds := slices.Clone(opts.Kinds)
	slices.Sort(kinds)
	kinds = slices.Compact(kinds)
	c := &Cache{
		st:           st,
		opts:         opts,
		kinds:        kinds,
		log:          opts.Logger.With("component", "zonecache"),
		m:            newCacheMetrics(),
		kick:         make(chan struct{}, 1),
		persistNow:   make(chan struct{}, 1),
		initialized:  make(chan struct{}),
		objects:      make(map[store.Kind]map[store.Key]store.Object, len(kinds)),
		rvs:          map[store.Kind]string{},
		kindSynced:   map[store.Kind]bool{},
		changed:      map[store.Kind]bool{},
		builderNames: map[string]bool{},
		conditions:   map[string]Condition{},
		views:        map[string]any{},
	}
	for _, k := range kinds {
		c.objects[k] = map[store.Key]store.Object{}
	}
	c.emptyIdx = c.indexOf(map[store.Kind][]store.Object{}, nil, nil)
	c.current.Store(&Snapshot{State: Unsynced, Objects: ObjectSet{idx: c.emptyIdx}, BuiltAt: time.Now(), views: map[string]any{}})
	if opts.Registerer != nil {
		if err := c.m.register(opts.Registerer, kinds, c.snapshotAgeSeconds); err != nil {
			return nil, fmt.Errorf("zonecache: register metrics: %w", err)
		}
	}
	return c, nil
}

// Register adds a builder. Builders may be registered before or after
// Start; a registration triggers a rebuild. Names must be unique.
func (c *Cache) Register(b Builder) error {
	if b == nil || b.Name() == "" {
		return errors.New("zonecache: builder needs a name")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.builderNames[b.Name()] {
		return fmt.Errorf("zonecache: builder %q already registered", b.Name())
	}
	c.builderNames[b.Name()] = true
	c.builders = append(c.builders, b)
	c.trigger()
	return nil
}

// OnPublish registers fn to run on the rebuild goroutine after every
// published snapshot. fn must return quickly and must not block on the
// Cache.
func (c *Cache) OnPublish(fn func(*Snapshot)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hooks = append(c.hooks, fn)
}

// SetLists hands the Cache the current compiled blocking lists (published
// by the list refresher); they reach builders through ObjectSet.Lists in
// the next snapshot.
func (c *Cache) SetLists(lists *blocking.Snapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lists = lists
	c.listsDirty = true
	c.trigger()
}

// Snapshot returns the current snapshot; never nil. Load it once per query.
func (c *Cache) Snapshot() *Snapshot { return c.current.Load() }

// State returns where the served objects come from.
func (c *Cache) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Synced reports whether every watched kind has delivered its initial list
// and Bookmark at least once.
func (c *Cache) Synced() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.syncedAll
}

// Initialized is closed once the Cache serves something: the store synced
// or a local.db snapshot was loaded. The startup probe (§5.2) waits on it.
func (c *Cache) Initialized() <-chan struct{} { return c.initialized }

// Degraded returns the active Degraded conditions, sorted by reason and
// builder.
func (c *Cache) Degraded() []Condition {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conditionList()
}

// NeedLeaderElection reports false: zonecache runs on every replica (§3.1).
// It makes Cache a controller-runtime manager.LeaderElectionRunnable.
func (c *Cache) NeedLeaderElection() bool { return false }

// Ready implements health.ReadyFunc: ready when live, or when serving a
// local.db snapshot younger than MaxAge (§4.4, §5.2). When ready, the
// returned strings describe active Degraded conditions (shown by
// /healthz); when not ready, they say why.
func (c *Cache) Ready() (bool, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case Live:
		return true, c.conditionStrings()
	case FromSnapshot:
		if age := time.Since(c.snapWritten); age > c.opts.MaxAge {
			return false, []string{fmt.Sprintf("serving a local.db snapshot %s old, older than maxAge %s", age.Round(time.Second), c.opts.MaxAge)}
		}
		return true, c.conditionStrings()
	}
	var waiting []string
	for _, k := range c.kinds {
		if !c.kindSynced[k] {
			waiting = append(waiting, string(k))
		}
	}
	out := []string{"waiting for the initial sync of " + strings.Join(waiting, ", ")}
	if c.noSnapshot != "" {
		out = append(out, c.noSnapshot)
	}
	return false, out
}

// Start runs the Cache until ctx ends: one watch per kind, the rebuild
// loop, the startup fallback to local.db and the local.db writer. It
// returns nil after ctx ends (a final local.db write is attempted when
// the live set changed since the last one), or an error if called twice.
func (c *Cache) Start(ctx context.Context) error {
	if !c.started.CompareAndSwap(false, true) {
		return errors.New("zonecache: Start called twice")
	}
	var wg sync.WaitGroup
	for _, k := range c.kinds {
		wg.Go(func() { c.watchKind(ctx, k) })
	}
	wg.Go(func() { c.rebuildLoop(ctx) })
	wg.Go(func() { c.startupFallback(ctx) })
	if c.opts.LocalDB != nil {
		wg.Go(func() { c.persistLoop(ctx) })
	}
	wg.Wait()
	return nil
}

// trigger asks the rebuild loop for a rebuild. Callers may hold c.mu.
func (c *Cache) trigger() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

func (c *Cache) markInitialized() { c.initOnce.Do(func() { close(c.initialized) }) }

// --- watches ---------------------------------------------------------------

// watchKind keeps one watch on kind open until ctx ends, re-listing with a
// fresh Watch whenever the store closes the channel.
func (c *Cache) watchKind(ctx context.Context, kind store.Kind) {
	retry := minRetry
	established := false
	for {
		w, err := c.st.Watch(ctx, kind, store.WatchOptions{})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, store.ErrClosed) {
				c.log.Error("store closed; watch ends", "kind", kind)
				return
			}
			c.log.Warn("watch failed; retrying", "kind", kind, "err", err, "retry", retry)
			if !sleep(ctx, retry) {
				return
			}
			retry = min(retry*2, maxRetry)
			continue
		}
		if established {
			c.m.resyncs.WithLabelValues(string(kind)).Inc()
		}
		established = true
		listed := c.consume(ctx, kind, w)
		w.Stop()
		if ctx.Err() != nil {
			return
		}
		if listed {
			retry = minRetry
		} else {
			retry = min(retry*2, maxRetry)
		}
		c.log.Info("watch closed by the store; re-listing", "kind", kind, "retry", retry)
		if !sleep(ctx, retry) {
			return
		}
	}
}

// consume applies the events of one watch until its channel closes or ctx
// ends. It reports whether the watch delivered its Bookmark.
func (c *Cache) consume(ctx context.Context, kind store.Kind, w store.Watcher) bool {
	l := &relist{seen: map[store.Key]struct{}{}}
	ch := w.ResultChan()
	for {
		select {
		case <-ctx.Done():
			return l.done
		case ev, ok := <-ch:
			if !ok {
				return l.done
			}
			c.handle(kind, ev, l)
		}
	}
}

// relist tracks the initial list of one watch: the keys it delivered
// before its Bookmark.
type relist struct {
	seen map[store.Key]struct{}
	done bool
}

// handle applies one event to the live set.
func (c *Cache) handle(kind store.Kind, ev store.Event, l *relist) {
	c.mu.Lock()
	defer c.mu.Unlock()
	set := c.objects[kind]
	switch ev.Type {
	case store.Added, store.Modified:
		if ev.Object == nil {
			return
		}
		key := store.Key{Kind: kind, Namespace: ev.Object.GetNamespace(), Name: ev.Object.GetName()}
		set[key] = ev.Object
		if !l.done {
			l.seen[key] = struct{}{}
		}
		c.rvs[kind] = rvMax(c.rvs[kind], ev.Object.GetResourceVersion())
		c.markChanged(kind)
	case store.Deleted:
		if ev.Object == nil {
			return
		}
		key := store.Key{Kind: kind, Namespace: ev.Object.GetNamespace(), Name: ev.Object.GetName()}
		if _, ok := set[key]; ok {
			delete(set, key)
			c.markChanged(kind)
		}
		if !l.done {
			delete(l.seen, key)
		}
	case store.Bookmark:
		if ev.Object != nil {
			if rv := ev.Object.GetResourceVersion(); rv != "" {
				c.rvs[kind] = rvMax(c.rvs[kind], rv)
			}
		}
		if l.done {
			return
		}
		l.done = true
		// Objects absent from this list were deleted while the previous
		// watch was down.
		for key := range set {
			if _, ok := l.seen[key]; !ok {
				delete(set, key)
				c.markChanged(kind)
			}
		}
		l.seen = nil
		c.kindSynced[kind] = true
		if !c.syncedAll {
			for _, k := range c.kinds {
				if !c.kindSynced[k] {
					return
				}
			}
			c.goLive()
		}
		// Every successful (re)sync is written to local.db.
		c.persistDirty = true
		select {
		case c.persistNow <- struct{}{}:
		default:
		}
	}
}

// goLive switches to the live set once every kind has synced. c.mu held.
func (c *Cache) goLive() {
	c.syncedAll = true
	from := c.state
	c.state = Live
	c.snapIdx = nil
	c.noSnapshot = ""
	delete(c.conditions, ReasonServingFromSnapshot)
	delete(c.conditions, ReasonSnapshotStale)
	c.m.serving.Set(0)
	c.markInitialized()
	c.trigger()
	c.log.Info("store synced; serving live objects", "previous", from.String())
}

// markChanged records a change to kind. c.mu held.
func (c *Cache) markChanged(kind store.Kind) {
	c.changed[kind] = true
	c.persistDirty = true
	if c.syncedAll {
		c.trigger()
	}
}

// rvMax returns the larger of two resourceVersions compared as unsigned
// integers. A numeric value wins over a non-numeric one; of two
// non-numeric values the first is kept.
func rvMax(a, b string) string {
	if b == "" {
		return a
	}
	if a == "" {
		return b
	}
	na, errA := strconv.ParseUint(a, 10, 64)
	nb, errB := strconv.ParseUint(b, 10, 64)
	switch {
	case errA == nil && errB == nil:
		if nb > na {
			return b
		}
		return a
	case errB == nil:
		return b
	default:
		return a
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// --- rebuild ---------------------------------------------------------------

// rebuildLoop publishes a snapshot after each debounce window that follows
// a trigger, and when a timed event (record expiry, snapshot crossing
// MaxAge) is due.
func (c *Cache) rebuildLoop(ctx context.Context) {
	wake := time.NewTimer(time.Hour)
	wake.Stop()
	defer wake.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.kick:
			if !sleep(ctx, c.opts.Debounce) {
				return
			}
		case <-wake.C:
			c.expiryTick = true
		}
		// Drain before reading state: a change after this point triggers
		// the next rebuild instead of being lost.
		select {
		case <-c.kick:
		default:
		}
		next := c.rebuild()
		if next.IsZero() {
			wake.Stop()
		} else {
			wake.Reset(max(time.Until(next), time.Millisecond))
		}
	}
}

// rebuild indexes the served objects, runs every builder and publishes a
// snapshot. It returns when the next timed rebuild is due, or zero.
func (c *Cache) rebuild() time.Time {
	c.rebuildLock.Lock()
	defer c.rebuildLock.Unlock()
	start := time.Now()

	c.mu.Lock()
	state := c.state
	lists, listsChanged := c.lists, c.listsDirty
	c.listsDirty = false
	builders := slices.Clone(c.builders)
	hooks := slices.Clone(c.hooks)
	writtenAt := c.snapWritten
	var idx *index
	var changed map[store.Kind]bool
	switch state {
	case Live:
		changed = c.changed
		c.changed = map[store.Kind]bool{}
		collect := make(map[store.Kind][]store.Object, len(changed))
		for _, k := range c.kinds {
			if c.liveIdx != nil && !changed[k] {
				continue
			}
			objs := make([]store.Object, 0, len(c.objects[k]))
			for _, o := range c.objects[k] {
				objs = append(objs, o)
			}
			collect[k] = objs
		}
		rvs := maps.Clone(c.rvs)
		c.mu.Unlock()
		idx = c.indexOf(collect, c.liveIdx, rvs)
		c.liveIdx = idx
	case FromSnapshot:
		idx = c.snapIdx
		c.mu.Unlock()
	default:
		idx = c.emptyIdx
		c.mu.Unlock()
	}

	set := ObjectSet{
		idx:          idx,
		lists:        lists,
		now:          start,
		changed:      changed,
		allChanged:   !c.firstBuilt || state != c.lastState,
		listsChanged: listsChanged,
	}
	if c.expiryTick {
		c.expiryTick = false
		set.changed = maps.Clone(changed)
		if set.changed == nil {
			set.changed = map[store.Kind]bool{}
		}
		set.changed[KindDNSRecordSet] = true
	}
	c.firstBuilt, c.lastState = true, state

	views := maps.Clone(c.views)
	type outcome struct {
		name string
		err  error
	}
	outcomes := make([]outcome, 0, len(builders))
	for _, b := range builders {
		v, err := safeBuild(b, set)
		if err != nil {
			c.m.buildErrors.WithLabelValues(b.Name()).Inc()
			c.log.Error("builder failed; keeping its previous view", "builder", b.Name(), "err", err)
		} else {
			views[b.Name()] = v
		}
		outcomes = append(outcomes, outcome{b.Name(), err})
	}
	c.views = views

	stale := state == FromSnapshot && start.Sub(writtenAt) > c.opts.MaxAge
	c.mu.Lock()
	for _, o := range outcomes {
		key := ReasonBuildFailed + "/" + o.name
		if o.err == nil {
			delete(c.conditions, key)
		} else if _, ok := c.conditions[key]; !ok {
			c.conditions[key] = Condition{Reason: ReasonBuildFailed, Builder: o.name, Message: o.name + ": " + o.err.Error(), Since: start}
		} else {
			cond := c.conditions[key]
			cond.Message = o.name + ": " + o.err.Error()
			c.conditions[key] = cond
		}
	}
	if stale && c.state == FromSnapshot {
		if _, ok := c.conditions[ReasonSnapshotStale]; !ok {
			c.conditions[ReasonSnapshotStale] = Condition{Reason: ReasonSnapshotStale, Message: fmt.Sprintf("local.db snapshot written %s is older than maxAge %s; primary zones answer SERVFAIL", writtenAt.UTC().Format(time.RFC3339), c.opts.MaxAge), Since: start}
		}
	}
	degraded := c.conditionList()
	c.mu.Unlock()

	snap := &Snapshot{
		Generation: c.current.Load().Generation + 1,
		State:      state,
		Objects:    set,
		BuiltAt:    start,
		Stale:      stale,
		views:      views,
		degraded:   degraded,
	}
	if state == FromSnapshot {
		snap.SnapshotWrittenAt = writtenAt
	}
	c.current.Store(snap)
	c.m.rebuilds.Inc()
	c.m.rebuildDuration.Observe(time.Since(start).Seconds())
	for _, k := range c.kinds {
		c.m.objects.WithLabelValues(string(k)).Set(float64(set.Count(k)))
	}
	for _, h := range hooks {
		h(snap)
	}

	next := set.nextExpiry(start)
	if state == FromSnapshot && !stale {
		if due := writtenAt.Add(c.opts.MaxAge); next.IsZero() || due.Before(next) {
			next = due
		}
	}
	return next
}

// safeBuild runs one builder, turning a panic into an error.
func safeBuild(b Builder, set ObjectSet) (v any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	return b.Build(set)
}

// indexOf builds an index from per-kind object slices. Kinds missing from
// objs reuse prev's kind index (or are empty without prev).
func (c *Cache) indexOf(objs map[store.Kind][]store.Object, prev *index, rvs map[store.Kind]string) *index {
	idx := &index{kinds: c.kinds, byKind: make(map[store.Kind]*kindIndex, len(c.kinds)), rvs: rvs}
	for _, k := range c.kinds {
		if o, ok := objs[k]; ok {
			idx.byKind[k] = newKindIndex(k, o)
			continue
		}
		if prev != nil && prev.byKind[k] != nil {
			idx.byKind[k] = prev.byKind[k]
			continue
		}
		idx.byKind[k] = newKindIndex(k, nil)
	}
	return idx
}

// conditionList returns the conditions sorted by reason and builder. c.mu
// held.
func (c *Cache) conditionList() []Condition {
	out := make([]Condition, 0, len(c.conditions))
	for _, cond := range c.conditions {
		out = append(out, cond)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Reason != out[j].Reason {
			return out[i].Reason < out[j].Reason
		}
		return out[i].Builder < out[j].Builder
	})
	return out
}

// conditionStrings renders the conditions as "degraded: ..." lines. c.mu
// held.
func (c *Cache) conditionStrings() []string {
	conds := c.conditionList()
	if len(conds) == 0 {
		return nil
	}
	out := make([]string, len(conds))
	for i, cond := range conds {
		out[i] = "degraded: " + cond.String()
	}
	return out
}
