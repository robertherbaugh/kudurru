// Package controller is the leader-only reconciler set (docs/architecture.md
// §3.1, §3.4, §4.2, §4.6, §5.1): one reconciler per kind (DNSZone,
// DNSRecordSet, BlockList, ClientGroup, Policy), never a mega-controller.
//
// Every reconciler is idempotent and level-triggered. Store.Watch feeds an
// in-memory cache and per-kind rate-limited workqueues; a reconcile reads
// only the cache, computes the object's whole desired status from current
// state, and calls Store.UpdateStatus only when that status differs from
// the stored one. A periodic resync (10 min) re-lists every kind through a
// fresh watch and re-queues every object, which repairs anything a dropped
// event or a failed write left behind. Nothing depends on event history,
// so a new leader resumes by simply starting (§4.6).
//
// Status written here: observedGeneration and the Ready/Degraded
// conditions on every kind; recordCount, contentHash and serial on
// DNSZone (serial = max(prev+1, unix_now), stamped only when contentHash
// changes, D12); names on DNSRecordSet; members on ClientGroup. Operational
// transitions are reported through an EventSink (SerialBumped, Duplicated,
// OutsideZone, Invalid).
//
// Run the Controller as a leader-only runnable of internal/cluster.Runner.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/util/workqueue"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	zonemodel "github.com/robertherbaugh/kudurru/internal/zone"
)

// Kinds reconciled by the controller.
const (
	KindDNSZone      store.Kind = "DNSZone"
	KindDNSRecordSet store.Kind = "DNSRecordSet"
	KindBlockList    store.Kind = "BlockList"
	KindClientGroup  store.Kind = "ClientGroup"
	KindPolicy       store.Kind = "Policy"
)

// Kinds lists the reconciled kinds in watch order.
var Kinds = []store.Kind{KindDNSZone, KindDNSRecordSet, KindBlockList, KindClientGroup, KindPolicy}

// Defaults for Options.
const (
	DefaultResyncPeriod = 10 * time.Minute
	DefaultWorkers      = 2
	DefaultZoneCoalesce = 250 * time.Millisecond
	maxWatchBackoff     = 30 * time.Second
)

// Options configures a Controller.
type Options struct {
	// Events receives operational transitions. Default LogSink.
	Events EventSink
	// ResyncPeriod re-lists every kind and re-queues every object.
	// Default DefaultResyncPeriod.
	ResyncPeriod time.Duration
	// Workers per kind. Default DefaultWorkers.
	Workers int
	// ZoneCoalesce delays a zone's reconcile after a record-set change so
	// a burst (an import, a Helm release, external-dns sync) is stamped as
	// one serial instead of one per object: at most one zone status write
	// per window per zone. Zone spec changes, resyncs and retries are not
	// delayed. Default DefaultZoneCoalesce; negative disables.
	ZoneCoalesce time.Duration
	// Validate checks endpoints for the record-set conditions; default
	// zone.ValidateEndpoint, the zone model's parser (the dataplane builds
	// zones with the same one, so Invalid means "not served"). The
	// miekg-free ValidateEndpoint remains for callers that want it.
	Validate EndpointValidator
	// RateLimiter overrides the per-kind retry limiter (default: client-
	// go's controller limiter, 5 ms doubling to 1000 s per item, 10 qps
	// overall).
	RateLimiter func() workqueue.TypedRateLimiter[store.Key]
	// Now is the clock for serials and condition transition times.
	// Default time.Now.
	Now func() time.Time
	// Registerer receives the controller metrics; nil leaves them
	// unregistered.
	Registerer prometheus.Registerer
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// KnownService reports whether the blocked-services catalogue in
	// service has a service ID (catalogue.Holder.HasService); Policy
	// blockedServices IDs it does not know make the Policy
	// Degraded{UnknownService}. Nil skips the check.
	KnownService func(id string) bool
	// KnownCategory is KnownService for blockedCategories
	// (catalogue.Holder.HasCategory): unknown IDs make the Policy
	// Degraded{UnknownCategory}. Nil skips the check.
	KnownCategory func(id string) bool
	// ServicesChanged re-queues every Policy when the catalogue changes at
	// runtime (catalogue.Holder.Subscribe); nil leaves it to the resync.
	ServicesChanged <-chan struct{}
	// ReconcileHook, if set, is called after every reconcile with its
	// duration and outcome (tracing and measurements).
	ReconcileHook func(key store.Key, took time.Duration, wroteStatus bool, err error)
}

// Controller runs the five reconcilers against one Store.
type Controller struct {
	st      store.Store
	opts    Options
	log     *slog.Logger
	metrics *ctrlMetrics

	running atomic.Bool
	// Per-run state, replaced at the start of every Run (every term). mu
	// guards the swap for Synced; the run's goroutines start after it.
	mu     sync.Mutex
	cache  *cache
	queues map[store.Kind]workqueue.TypedRateLimitingInterface[store.Key]
	synced map[store.Kind]chan struct{}
}

// reconcileFunc reconciles one key and reports whether it wrote status.
type reconcileFunc func(ctx context.Context, key store.Key) (bool, error)

// New returns a Controller. It does no I/O; call Run under a leader term.
func New(st store.Store, opts Options) (*Controller, error) {
	if st == nil {
		return nil, errors.New("controller: nil store")
	}
	if opts.Events == nil {
		opts.Events = LogSink{Logger: opts.Logger}
	}
	if opts.ResyncPeriod <= 0 {
		opts.ResyncPeriod = DefaultResyncPeriod
	}
	if opts.Workers <= 0 {
		opts.Workers = DefaultWorkers
	}
	switch {
	case opts.ZoneCoalesce == 0:
		opts.ZoneCoalesce = DefaultZoneCoalesce
	case opts.ZoneCoalesce < 0:
		opts.ZoneCoalesce = 0
	}
	if opts.Validate == nil {
		opts.Validate = zonemodel.ValidateEndpoint
	}
	if opts.RateLimiter == nil {
		opts.RateLimiter = workqueue.DefaultTypedControllerRateLimiter[store.Key]
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	m, err := newCtrlMetrics(opts.Registerer)
	if err != nil {
		return nil, err
	}
	return &Controller{st: st, opts: opts, log: opts.Logger.With("component", "controller"), metrics: m}, nil
}

// Run watches every kind and reconciles until ctx is cancelled (the end of
// the leader term), then stops its watches and workers and returns nil.
// Workers start once every kind's initial list has been delivered, so no
// reconcile acts on a partial view. Run may be called again after it
// returns (the next term); each run starts from a fresh cache.
func (c *Controller) Run(ctx context.Context) error {
	if !c.running.CompareAndSwap(false, true) {
		return errors.New("controller: already running")
	}
	defer c.running.Store(false)

	queues := map[store.Kind]workqueue.TypedRateLimitingInterface[store.Key]{}
	synced := map[store.Kind]chan struct{}{}
	for _, k := range Kinds {
		queues[k] = workqueue.NewTypedRateLimitingQueueWithConfig(c.opts.RateLimiter(), workqueue.TypedRateLimitingQueueConfig[store.Key]{Name: "kudurru_controller_" + string(k)})
		synced[k] = make(chan struct{})
	}
	c.mu.Lock()
	c.cache, c.queues, c.synced = newCache(), queues, synced
	c.mu.Unlock()
	reconcilers := map[store.Kind]reconcileFunc{
		KindDNSZone:      c.reconcileZone,
		KindDNSRecordSet: c.reconcileRecordSet,
		KindBlockList:    c.reconcileBlockList,
		KindClientGroup:  c.reconcileClientGroup,
		KindPolicy:       c.reconcilePolicy,
	}

	var wg sync.WaitGroup
	for _, k := range Kinds {
		wg.Add(1)
		go func() { defer wg.Done(); c.watchLoop(ctx, k) }()
	}
	t0 := time.Now()
	allSynced := true
	for _, k := range Kinds {
		select {
		case <-c.synced[k]:
		case <-ctx.Done():
			allSynced = false
		}
		if !allSynced {
			break
		}
	}
	if allSynced {
		c.log.Info("caches synced; starting reconcilers", "took", time.Since(t0).Round(time.Millisecond))
		for _, k := range Kinds {
			for range c.opts.Workers {
				wg.Add(1)
				go func() { defer wg.Done(); c.worker(ctx, k, reconcilers[k]) }()
			}
		}
		if ch := c.opts.ServicesChanged; ch != nil {
			wg.Add(1)
			go func() { defer wg.Done(); c.requeuePolicies(ctx, ch) }()
		}
	}
	<-ctx.Done()
	for _, q := range c.queues {
		q.ShutDown()
	}
	wg.Wait()
	c.log.Info("controller stopped")
	return nil
}

// requeuePolicies queues every Policy on each signal of ch, so their
// blockedServices are re-validated against a new catalogue.
func (c *Controller) requeuePolicies(ctx context.Context, ch <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			for _, o := range c.cache.list(KindPolicy, "") {
				c.queues[KindPolicy].Add(store.KeyOf(o))
			}
		}
	}
}

// Synced reports whether every kind's initial list has been delivered in
// the current run.
func (c *Controller) Synced() bool {
	c.mu.Lock()
	synced := c.synced
	c.mu.Unlock()
	if !c.running.Load() || synced == nil {
		return false
	}
	for _, ch := range synced {
		select {
		case <-ch:
		default:
			return false
		}
	}
	return true
}

// watchLoop keeps a watch open on one kind, re-opening it after errors and
// every ResyncPeriod. The initial events of each watch session are a full
// list: when its bookmark arrives, cached objects the list did not contain
// are removed and every object of the kind is queued.
func (c *Controller) watchLoop(ctx context.Context, kind store.Kind) {
	backoff := 100 * time.Millisecond
	firstSync := true
	for ctx.Err() == nil {
		w, err := c.st.Watch(ctx, kind, store.WatchOptions{})
		if err != nil {
			c.log.Warn("watch failed; retrying", "kind", kind, "err", err, "after", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, maxWatchBackoff)
			continue
		}
		backoff = 100 * time.Millisecond
		resync := time.NewTimer(c.opts.ResyncPeriod)
		seen := map[store.Key]struct{}{}
		listing := true
	session:
		for {
			select {
			case <-ctx.Done():
				resync.Stop()
				w.Stop()
				return
			case <-resync.C:
				c.metrics.resyncs.WithLabelValues(string(kind)).Inc()
				break session
			case ev, ok := <-w.ResultChan():
				if !ok {
					c.log.Debug("watch closed; re-opening", "kind", kind)
					break session
				}
				switch ev.Type {
				case store.Bookmark:
					if listing {
						listing = false
						c.endList(kind, seen)
						seen = nil
						if firstSync {
							firstSync = false
							close(c.synced[kind])
						}
					}
				case store.Added, store.Modified:
					if ev.Object == nil || store.KindOf(ev.Object) != kind {
						continue
					}
					if listing {
						seen[store.KeyOf(ev.Object)] = struct{}{}
					}
					if prev, changed := c.cache.upsert(ev.Object); changed {
						c.fanOut(prev, ev.Object)
					}
				case store.Deleted:
					if ev.Object == nil {
						continue
					}
					if prev := c.cache.remove(store.KeyOf(ev.Object)); prev != nil {
						c.fanOut(prev, nil)
					}
				}
			}
		}
		resync.Stop()
		w.Stop()
	}
}

// endList finishes a list: objects not seen were deleted while no watch was
// open, and every object of the kind is queued (initial sync and resync).
func (c *Controller) endList(kind store.Kind, seen map[store.Key]struct{}) {
	for _, k := range c.cache.keys(kind) {
		if _, ok := seen[k]; ok {
			c.queues[kind].Add(k)
			continue
		}
		if prev := c.cache.remove(k); prev != nil {
			c.fanOut(prev, nil)
		}
	}
}

// fanOut queues the reconciles a change affects (prev or next may be nil).
func (c *Controller) fanOut(prev, next store.Object) {
	obj := firstNonNil(next, prev)
	key := store.KeyOf(obj)
	specChanged := prev == nil || next == nil || prev.GetGeneration() != next.GetGeneration() ||
		!equalStringMaps(prev.GetLabels(), next.GetLabels())
	if key.Kind == KindDNSZone && !specChanged {
		// A status-only change, usually the echo of this controller's
		// own write: re-check within the coalescing window, or a burst
		// would be stamped once per echo instead of once per window.
		c.queues[key.Kind].AddAfter(key, c.opts.ZoneCoalesce)
	} else {
		c.queues[key.Kind].Add(key)
	}
	switch obj.(type) {
	case *v1alpha1.DNSZone:
		if specChanged {
			for _, k := range c.cache.rrsetsOf(key.Name) {
				c.queues[KindDNSRecordSet].Add(k)
			}
		}
	case *v1alpha1.DNSRecordSet:
		if !specChanged {
			return
		}
		p, _ := prev.(*v1alpha1.DNSRecordSet)
		n, _ := next.(*v1alpha1.DNSRecordSet)
		for _, rs := range []*v1alpha1.DNSRecordSet{p, n} {
			if rs == nil {
				continue
			}
			zone := rs.Spec.ZoneRef.Name
			// AddAfter keeps the earliest pending time, so the window
			// opens with the first change of a burst.
			c.queues[KindDNSZone].AddAfter(store.Key{Kind: KindDNSZone, Name: zone}, c.opts.ZoneCoalesce)
			for _, k := range c.cache.identityOwners(zone, p, n) {
				c.queues[KindDNSRecordSet].Add(k)
			}
		}
	case *v1alpha1.BlockList, *v1alpha1.ClientGroup:
		if prev != nil && next != nil && v1alpha1.IsDisabled(prev) == v1alpha1.IsDisabled(next) {
			return // existence and kudurru.io/enabled are all a Policy checks
		}
		for _, o := range c.cache.list(KindPolicy, key.Namespace) {
			if p, ok := o.(*v1alpha1.Policy); ok && policyRefers(p, key) {
				c.queues[KindPolicy].Add(store.KeyOf(p))
			}
		}
	}
}

// worker processes one kind's queue until it shuts down.
func (c *Controller) worker(ctx context.Context, kind store.Kind, fn reconcileFunc) {
	q := c.queues[kind]
	for {
		key, shutdown := q.Get()
		if shutdown {
			return
		}
		t0 := time.Now()
		wrote, err := c.safeReconcile(ctx, fn, key)
		took := time.Since(t0)
		c.metrics.duration.WithLabelValues(string(kind)).Observe(took.Seconds())
		if c.opts.ReconcileHook != nil {
			c.opts.ReconcileHook(key, took, wrote, err)
		}
		switch {
		case err == nil:
			c.metrics.reconciles.WithLabelValues(string(kind), "success").Inc()
			q.Forget(key)
		case ctx.Err() != nil:
			q.Forget(key)
		case errors.Is(err, store.ErrConflict):
			// The cache is behind the store; the watch will catch up.
			c.metrics.reconciles.WithLabelValues(string(kind), "conflict").Inc()
			c.log.Debug("status conflict; retrying", "key", key.String())
			q.AddRateLimited(key)
		default:
			c.metrics.reconciles.WithLabelValues(string(kind), "error").Inc()
			c.log.Warn("reconcile failed; retrying", "key", key.String(), "err", err, "retries", q.NumRequeues(key))
			q.AddRateLimited(key)
		}
		q.Done(key)
	}
}

// safeReconcile turns a panic into an error so one bad object cannot stop
// the controller.
func (c *Controller) safeReconcile(ctx context.Context, fn reconcileFunc, key store.Key) (wrote bool, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic reconciling %s: %v", key, p)
		}
	}()
	return fn(ctx, key)
}

// writeStatus persists next's status when it differs from cur's (equal is
// the caller's comparison). It stamps the cached resourceVersion so the
// kube driver applies optimistic concurrency, and records the write so
// reconciles before the watch echo compare against it.
func (c *Controller) writeStatus(ctx context.Context, next store.Object) error {
	key := store.KeyOf(next)
	baseRV := c.cache.cachedRV(key)
	next.SetResourceVersion(baseRV)
	if err := c.st.UpdateStatus(ctx, next); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil // deleted meanwhile; the Deleted event follows
		}
		return err
	}
	c.metrics.statusWrites.WithLabelValues(string(key.Kind)).Inc()
	c.cache.wrote(key, baseRV, next)
	return nil
}

func equalStringMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// ctrlMetrics are the controller's instruments.
type ctrlMetrics struct {
	reconciles   *prometheus.CounterVec
	duration     *prometheus.HistogramVec
	statusWrites *prometheus.CounterVec
	resyncs      *prometheus.CounterVec
}

func newCtrlMetrics(reg prometheus.Registerer) (*ctrlMetrics, error) {
	m := &ctrlMetrics{
		reconciles: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_controller_reconciles_total",
			Help: "Reconciles by kind and result (success, conflict, error).",
		}, []string{"kind", "result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kudurru_controller_reconcile_duration_seconds",
			Help:    "Time spent in one reconcile, including the status write.",
			Buckets: []float64{.00005, .0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1, .5, 1},
		}, []string{"kind"}),
		statusWrites: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_controller_status_writes_total",
			Help: "Store.UpdateStatus calls by kind; unchanged statuses are never written.",
		}, []string{"kind"}),
		resyncs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_controller_resyncs_total",
			Help: "Periodic resyncs by kind.",
		}, []string{"kind"}),
	}
	if reg != nil {
		for _, col := range []prometheus.Collector{m.reconciles, m.duration, m.statusWrites, m.resyncs} {
			if err := reg.Register(col); err != nil {
				return nil, fmt.Errorf("controller: register metrics: %w", err)
			}
		}
	}
	return m, nil
}
