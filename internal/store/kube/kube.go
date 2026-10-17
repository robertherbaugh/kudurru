// Package kube is the Kubernetes Store driver (dev-docs/design-record.md §4.3,
// decisions D2, D9, D10, D44): CRDs in the apiserver are the only source of
// truth for configuration objects.
//
// The driver splits its I/O in two:
//
//   - Get, List, Apply, Delete, UpdateStatus, Snapshot and Restore go to the
//     apiserver through an uncached controller-runtime client. API callers
//     need read-your-writes and keyset pagination (Continue tokens), and an
//     informer cache offers neither.
//   - Watch is served from a controller-runtime informer cache (one
//     informer per kudurru.io kind, managedFields stripped). This is the
//     high-volume path: zonecache and the reconcilers consume it, and it
//     keeps working from memory while the apiserver is away (§4.4).
//
// Info().LastContact reports the last successful apiserver round trip so
// consumers can tell a quiet cluster from an outage.
//
// Apply is server-side apply with FieldOwner(opts.Manager) and
// ForceOwnership only when opts.Force; the single writer is mirrored into the
// kudurru.io/managed-by annotation and enforced both by a pre-check and by
// SSA field ownership of that annotation, so concurrent writers cannot both
// win. Elector returns a coordination.k8s.io Lease elector whose lifetime is
// decoupled from the process (D44): losing the lease cancels only the leader
// term, never the caller.
//
// The local.db snapshot used for cold start without the apiserver (D7,
// D33) lives in internal/store/localdb; internal/zonecache writes it (D79).
package kube

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	coordinationv1client "k8s.io/client-go/kubernetes/typed/coordination/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// DriverName is reported in store.Info.Driver.
const DriverName = "kube"

// Defaults from dev-docs/design-record.md §4.4 and §5.3.
const (
	DefaultLeaseDuration  = 15 * time.Second
	DefaultRenewDeadline  = 10 * time.Second
	DefaultRetryPeriod    = 2 * time.Second
	DefaultStartupTimeout = 15 * time.Second
	DefaultLeasePrefix    = "kudurru-"
	// DefaultQPS and DefaultBurst replace client-go's 5/10 client-side
	// rate limit when the rest.Config leaves QPS unset.
	DefaultQPS   = 50
	DefaultBurst = 100
)

// serviceAccountNamespaceFile is where a pod finds its own namespace.
const serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// applyRetries bounds the check-then-write loops of Apply and Delete when the
// object changes between the managedBy check and the write.
const applyRetries = 5

// Options configures the driver. The zero value is usable in a pod.
type Options struct {
	// Namespace holds the driver's Leases and the Secrets of internal
	// kinds. Default: the pod's namespace (PodNamespace), else "default".
	Namespace string
	// Identity is this replica's Lease holder identity. Default: the
	// hostname, which is the pod name in Kubernetes (§5.3). Outside a pod
	// every process must set a unique identity.
	Identity string
	// LeasePrefix is prepended to Elector names to form Lease names, so
	// Elector("controller") uses Lease "kudurru-controller".
	// Default DefaultLeasePrefix.
	LeasePrefix string
	// LeaseDuration, RenewDeadline and RetryPeriod are the election
	// timings; default 15 s / 10 s / 2 s.
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
	// StartupTimeout bounds WaitForSync when the caller's context has no
	// deadline (store.kube.startupTimeout, §4.4). Default 15 s.
	StartupTimeout time.Duration
	// Scheme overrides the scheme; it must contain kudurru.io/v1alpha1 and
	// core/v1. Default: a fresh scheme with both.
	Scheme *runtime.Scheme
	// Logger receives driver logs. Default slog.Default().
	Logger *slog.Logger
	// InternalKinds are kinds served as Secrets labelled
	// kudurru.io/kind=<Kind> in Namespace instead of CRDs (internal.go):
	// the identity layer's LocalUser, APIToken, ServiceAccount,
	// RoleBinding and IdentityProvider. They must be registered in Scheme
	// (internalkinds.AddToScheme); core/v1 is added to Scheme when it is
	// missing. Snapshot omits them when Safe.
	InternalKinds []store.Kind
}

func (o *Options) setDefaults() error {
	if o.Namespace == "" {
		o.Namespace = podNamespace()
	}
	if o.Identity == "" {
		h, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("store/kube: no Identity and no hostname: %w", err)
		}
		o.Identity = h
	}
	if o.LeasePrefix == "" {
		o.LeasePrefix = DefaultLeasePrefix
	}
	if o.LeaseDuration == 0 {
		o.LeaseDuration = DefaultLeaseDuration
	}
	if o.RenewDeadline == 0 {
		o.RenewDeadline = DefaultRenewDeadline
	}
	if o.RetryPeriod == 0 {
		o.RetryPeriod = DefaultRetryPeriod
	}
	if o.StartupTimeout == 0 {
		o.StartupTimeout = DefaultStartupTimeout
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Scheme == nil {
		s := runtime.NewScheme()
		if err := v1alpha1.AddToScheme(s); err != nil {
			return err
		}
		if err := corev1.AddToScheme(s); err != nil {
			return err
		}
		o.Scheme = s
	}
	return nil
}

// PodNamespace returns the namespace of the pod the process runs in (the
// release namespace), or "default" outside a pod: the default of
// Options.Namespace, and where the identity layer keeps kudurru's own
// service accounts and reads clientSecretRef Secrets.
func PodNamespace() string { return podNamespace() }

func podNamespace() string {
	if b, err := os.ReadFile(serviceAccountNamespaceFile); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	return "default"
}

// Store implements store.Store over the Kubernetes apiserver.
type Store struct {
	opts   Options
	kinds  *registry
	mapper meta.RESTMapper
	c      client.Client // uncached: reads and writes go to the apiserver
	cache  cache.Cache   // informers for Watch
	leases coordinationv1client.LeasesGetter
	log    *slog.Logger

	internal *internalKinds  // Options.InternalKinds, served as Secrets
	coreREST rest.Interface  // core/v1 client for the internal-kind informer
	bgCtx    context.Context // cancelled by Close
	contact  *contact        // last successful apiserver round trip

	cancel         context.CancelFunc // stops the cache and background work
	cacheDone      chan struct{}      // closed when cache.Start returns
	informersReady chan struct{}      // closed when every kind has an informer

	clusterID atomic.Pointer[string]

	mu       sync.Mutex
	closed   bool
	electors map[string]*LeaseElector
	watchers map[*watcher]struct{}
}

var _ store.Store = (*Store)(nil)

// New builds the driver from a rest.Config and starts its informer cache in
// the background. It does no blocking I/O: when the apiserver is
// unreachable New still succeeds, informers keep retrying, and WaitForSync
// times out so the caller can fall back to local.db (§4.4). cfg is copied;
// an unset QPS becomes DefaultQPS/DefaultBurst.
func New(cfg *rest.Config, opts Options) (*Store, error) {
	if cfg == nil {
		return nil, errors.New("store/kube: nil rest.Config")
	}
	if err := opts.setDefaults(); err != nil {
		return nil, err
	}
	cfg = rest.CopyConfig(cfg)
	if cfg.QPS == 0 && cfg.RateLimiter == nil {
		// client-go would default to 5 QPS / burst 10, which turns a
		// Restore or bulk import of 1,000 objects into minutes. The
		// apiserver's priority and fairness is the real limiter.
		cfg.QPS, cfg.Burst = DefaultQPS, DefaultBurst
	}
	ct := &contact{now: time.Now}
	cfg.Wrap(ct.wrap)
	kinds, err := newRegistry(opts.Scheme)
	if err != nil {
		return nil, err
	}
	internal, err := splitInternal(kinds, opts.InternalKinds)
	if err != nil {
		return nil, err
	}
	if len(opts.InternalKinds) > 0 && !opts.Scheme.Recognizes(corev1.SchemeGroupVersion.WithKind("SecretList")) {
		// Internal kinds are Secrets: the client needs core/v1 in its
		// scheme (internalkinds.NewScheme carries only kudurru.io).
		if err := corev1.AddToScheme(opts.Scheme); err != nil {
			return nil, fmt.Errorf("store/kube: core/v1 scheme: %w", err)
		}
	}
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("store/kube: http client: %w", err)
	}
	// RESTMapping has no caller context. Bound discovery independently so a
	// black-holed apiserver cannot hold startup before local.db is served.
	// Keep the watch client's timeout unchanged.
	discoveryCfg := rest.CopyConfig(cfg)
	discoveryCfg.Timeout = opts.StartupTimeout
	discoveryHTTP, err := rest.HTTPClientFor(discoveryCfg)
	if err != nil {
		return nil, fmt.Errorf("store/kube: discovery http client: %w", err)
	}
	mapper, err := apiutil.NewDynamicRESTMapper(discoveryCfg, discoveryHTTP)
	if err != nil {
		return nil, fmt.Errorf("store/kube: rest mapper: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: opts.Scheme, Mapper: mapper, HTTPClient: httpClient})
	if err != nil {
		return nil, fmt.Errorf("store/kube: client: %w", err)
	}
	ca, err := cache.New(cfg, cache.Options{
		Scheme:                      opts.Scheme,
		Mapper:                      mapper,
		HTTPClient:                  httpClient,
		ReaderFailOnMissingInformer: true,
		DefaultTransform:            cache.TransformStripManagedFields(),
	})
	if err != nil {
		return nil, fmt.Errorf("store/kube: cache: %w", err)
	}
	leases, err := coordinationv1client.NewForConfigAndClient(cfg, httpClient)
	if err != nil {
		return nil, fmt.Errorf("store/kube: coordination client: %w", err)
	}
	core, err := corev1client.NewForConfigAndClient(cfg, httpClient)
	if err != nil {
		return nil, fmt.Errorf("store/kube: core client: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Store{
		opts:           opts,
		kinds:          kinds,
		mapper:         mapper,
		c:              c,
		cache:          ca,
		leases:         leases,
		internal:       internal,
		coreREST:       core.RESTClient(),
		bgCtx:          ctx,
		log:            opts.Logger.With("component", "store/kube"),
		contact:        ct,
		cancel:         cancel,
		cacheDone:      make(chan struct{}),
		informersReady: make(chan struct{}),
		electors:       map[string]*LeaseElector{},
		watchers:       map[*watcher]struct{}{},
	}
	go func() {
		defer close(s.cacheDone)
		if err := ca.Start(ctx); err != nil {
			s.log.Error("informer cache stopped", "err", err)
		}
	}()
	go s.startInformers(ctx)
	go s.discoverClusterID(ctx)
	return s, nil
}

// startInformers creates one informer per kind, retrying while the
// apiserver (or its discovery) is unreachable, then closes informersReady.
func (s *Store) startInformers(ctx context.Context) {
	for _, k := range s.kinds.kinds {
		ki := s.kinds.byKind[k]
		for {
			_, err := s.cache.GetInformer(ctx, ki.newObject(), cache.BlockUntilSynced(false))
			if err == nil {
				break
			}
			s.log.Warn("informer not started yet; retrying", "kind", k, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
	close(s.informersReady)
}

// discoverClusterID records the kube-system namespace UID, the conventional
// stable identity of a cluster, retrying until it succeeds or Close.
func (s *Store) discoverClusterID(ctx context.Context) {
	for {
		var ns corev1.Namespace
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := s.c.Get(rctx, client.ObjectKey{Name: "kube-system"}, &ns)
		cancel()
		if err == nil {
			id := string(ns.UID)
			s.clusterID.Store(&id)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// WaitForSync blocks until every kind's informer has listed the apiserver
// once, or ctx ends. Without a deadline on ctx it waits at most
// Options.StartupTimeout. A false return is the §4.4 signal to serve from
// local.db.
func (s *Store) WaitForSync(ctx context.Context) bool {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.opts.StartupTimeout)
		defer cancel()
	}
	select {
	case <-s.informersReady:
	case <-ctx.Done():
		return false
	}
	return s.cache.WaitForCacheSync(ctx)
}

// Info implements store.Store. LastContact is the time of the last
// successful apiserver round trip (any request of the driver, including
// informer watch traffic and Lease renewals; see contact.go), zero before
// the first one.
func (s *Store) Info() store.Info {
	info := store.Info{Driver: DriverName, SchemaVersion: store.SchemaVersion, LastContact: s.contact.Last()}
	if id := s.clusterID.Load(); id != nil {
		info.ClusterID = *id
	}
	s.mu.Lock()
	e := s.electors["controller"]
	s.mu.Unlock()
	if e != nil {
		info.Leader = e.Leader()
	}
	return info
}

// Close stops the informer cache and every open Watch. Electors are owned by
// their callers' contexts and are not stopped by Close.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	ws := make([]*watcher, 0, len(s.watchers))
	for w := range s.watchers {
		ws = append(ws, w)
	}
	s.mu.Unlock()
	for _, w := range ws {
		w.Stop()
	}
	s.cancel()
	<-s.cacheDone
	return nil
}

func (s *Store) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// isNamespaced reports whether the kind is namespaced according to the
// apiserver's discovery data.
func (s *Store) isNamespaced(ki *kindInfo) (bool, error) {
	m, err := s.mapper.RESTMapping(ki.gvk.GroupKind(), ki.gvk.Version)
	if err != nil {
		return false, fmt.Errorf("store/kube: %s: %w", ki.kind, err)
	}
	return m.Scope.Name() == meta.RESTScopeNameNamespace, nil
}
