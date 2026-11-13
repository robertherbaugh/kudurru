// Package k8s is the Kubernetes source adapter of external zones (D402).
//
// A DNSZone of type external is filled from Kubernetes objects: Ingresses,
// Gateway API routes (HTTPRoute, GRPCRoute, TLSRoute) and Services of type
// LoadBalancer, as its spec.external selects. The adapter runs on the
// elected replica only (a leader-only runnable, never the query path). It
// reads the sources through the driver's guarded transport with bounded,
// complete lists, retains only the fields it needs, and maintains one
// DNSRecordSet per source object and zone in the source's namespace through
// the ordinary store ownership and compare-and-swap path, as the DNSEndpoint
// source does (D303). Every replica then serves those record sets through its
// zone cache; DNS answers never read the API server.
//
// Watches only wake a fresh bounded reconciliation; their payloads grant no
// write. Sources are never written: no status, finalizer or annotation. A
// record set is written only after its zone and its source are confirmed
// unchanged, and removed only after the source is confirmed absent or
// replaced. Kubernetes has no transaction across these objects, so the
// adapter converges eventually instead of claiming atomic updates.
package k8s

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	zonemodel "github.com/robertherbaugh/kudurru/internal/zone"
)

const (
	reconcileTimeout = 30 * time.Second
	// maxObjects and maxBytes bound one kind's complete list; a larger
	// list refuses that kind for the pass (its record sets are kept).
	maxObjects = 4096
	maxBytes   = 32 << 20
	// maxZonesPerSource bounds the record sets one source can own.
	maxZonesPerSource = 8
	// maxEndpoints is the DNSRecordSet schema's spec.endpoints bound.
	maxEndpoints  = 1000
	defaultResync = 10 * time.Second
)

// Options configures the adapter.
type Options struct {
	Store     store.Store
	Validator *validator.Validator
	// Admit rechecks the elected term before every write.
	Admit  func(context.Context) error
	Logger *slog.Logger
	// Resync bounds the wait between reconciliations when no watch event
	// arrives (default 10 s).
	Resync time.Duration
}

// Controller maintains the record sets of external zones.
type Controller struct {
	opts   Options
	client dynamic.Interface
}

// New requires the Kubernetes driver's guarded RESTConfig.
func New(cfg *rest.Config, opts Options) (*Controller, error) {
	if cfg == nil || opts.Store == nil || opts.Validator == nil || opts.Admit == nil {
		return nil, errors.New("external zones: incomplete source options")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Resync <= 0 {
		opts.Resync = defaultResync
	}
	config := rest.CopyConfig(cfg)
	// Bound one list page or watch response before it is decoded.
	config.Wrap(func(rt http.RoundTripper) http.RoundTripper { return limitTransport{rt} })
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &Controller{opts: opts, client: client}, nil
}

type limitTransport struct{ http.RoundTripper }
type limitBody struct {
	io.Reader
	io.Closer
}

func (t limitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := t.RoundTripper.RoundTrip(req)
	if err == nil && res != nil && res.Body != nil {
		res.Body = limitBody{Reader: io.LimitReader(res.Body, 4<<20), Closer: res.Body}
	}
	return res, err
}

// Run reconciles until ctx (the elected term) ends.
func (c *Controller) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		rctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
		watched, err := c.reconcile(rctx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.opts.Logger.Warn("external zone reconciliation incomplete", "error", err)
		}
		c.wait(ctx, watched)
	}
	return nil
}

// watchFrom is one resource and the list resourceVersion to watch from.
type watchFrom struct {
	gvr schema.GroupVersionResource
	rv  string
}

// wait returns on the first watch event of any watched list, or after the
// resync interval.
func (c *Controller) wait(ctx context.Context, watched []watchFrom) {
	wctx, stop := context.WithTimeout(ctx, c.opts.Resync)
	defer stop()
	woke := make(chan struct{}, 1)
	var wg sync.WaitGroup
	seconds := int64(c.opts.Resync / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	for _, w := range watched {
		wg.Add(1)
		go func(w watchFrom) {
			defer wg.Done()
			watch, err := c.client.Resource(w.gvr).Watch(wctx, metav1.ListOptions{ResourceVersion: w.rv, TimeoutSeconds: &seconds})
			if err != nil {
				return
			}
			defer watch.Stop()
			select {
			case <-wctx.Done():
			case _, ok := <-watch.ResultChan():
				if ok {
					select {
					case woke <- struct{}{}:
					default:
					}
				}
			}
		}(w)
	}
	select {
	case <-wctx.Done():
	case <-woke:
	}
	stop()
	wg.Wait()
}

// list reads one complete, bounded list of gvr in all namespaces.
func (c *Controller) list(ctx context.Context, gvr schema.GroupVersionResource) ([]unstructured.Unstructured, string, error) {
	var all []unstructured.Unstructured
	var rv, cont string
	size := 0
	for {
		page, err := c.client.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 256, Continue: cont})
		if err != nil {
			return nil, "", err
		}
		if page.GetResourceVersion() == "" || (rv != "" && rv != page.GetResourceVersion()) {
			return nil, "", store.ErrConflict
		}
		rv = page.GetResourceVersion()
		for _, item := range page.Items {
			data, err := item.MarshalJSON()
			if err != nil {
				return nil, "", err
			}
			size += len(data)
			if size > maxBytes || len(all) == maxObjects {
				return nil, "", store.ErrTooLarge
			}
			all = append(all, item)
		}
		if cont = page.GetContinue(); cont == "" {
			return all, rv, nil
		}
	}
}

// zones returns the external zones that are being served.
func (c *Controller) zones(ctx context.Context) ([]*v1alpha1.DNSZone, error) {
	var out []*v1alpha1.DNSZone
	cont, count := "", 0
	for {
		page, err := c.opts.Store.List(ctx, "DNSZone", store.ListOptions{Limit: 128, Continue: cont})
		if err != nil {
			return nil, err
		}
		for _, obj := range page.Items {
			if count++; count > maxObjects {
				return nil, store.ErrTooLarge
			}
			z, ok := obj.(*v1alpha1.DNSZone)
			if !ok {
				return nil, errors.New("external zones: invalid zone observation")
			}
			if z.Spec.Type == v1alpha1.ZoneTypeExternal && z.Spec.External != nil && z.DeletionTimestamp == nil && !v1alpha1.IsDisabled(z) {
				out = append(out, z)
			}
		}
		if cont = page.Continue; cont == "" {
			return out, nil
		}
	}
}

// children lists the record sets derived from sources of kind k.
func (c *Controller) children(ctx context.Context, k *sourceKind) ([]*v1alpha1.DNSRecordSet, error) {
	var out []*v1alpha1.DNSRecordSet
	cont := ""
	for {
		page, err := c.opts.Store.List(ctx, "DNSRecordSet", store.ListOptions{Limit: 128, Continue: cont, LabelSelector: map[string]string{zonemodel.ExternalSourceLabel: k.label}})
		if err != nil {
			return nil, err
		}
		for _, obj := range page.Items {
			rs, ok := obj.(*v1alpha1.DNSRecordSet)
			if !ok || len(out) == maxObjects*maxZonesPerSource {
				return nil, store.ErrTooLarge
			}
			out = append(out, rs)
		}
		if cont = page.Continue; cont == "" {
			return out, nil
		}
	}
}

func enabled(z *v1alpha1.DNSZone, f family) bool {
	switch f {
	case familyIngress:
		return z.Spec.External.Ingress
	case familyGateway:
		return z.Spec.External.Gateway
	case familyService:
		return z.Spec.External.LoadBalancerServices
	}
	return false
}

// reconcile is one bounded pass over every source kind. It returns the
// lists to watch for the next wake-up.
func (c *Controller) reconcile(ctx context.Context) ([]watchFrom, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("external zones: reconciliation must be bounded")
	}
	if err := c.opts.Admit(ctx); err != nil {
		return nil, err
	}
	zones, err := c.zones(ctx)
	if err != nil {
		return nil, err
	}
	var watched []watchFrom
	var firstError error
	keep := func(err error) {
		if err != nil && firstError == nil {
			firstError = err
		}
	}
	byFamily := map[family][]*v1alpha1.DNSZone{}
	for _, z := range zones {
		for _, f := range []family{familyIngress, familyGateway, familyService} {
			if enabled(z, f) {
				byFamily[f] = append(byFamily[f], z)
			}
		}
	}
	var gateways map[string]gateway
	gatewayErr := error(nil)
	if len(byFamily[familyGateway]) > 0 {
		items, rv, err := c.list(ctx, gatewayResource)
		if err != nil {
			gatewayErr = err
		} else {
			watched = append(watched, watchFrom{gatewayResource, rv})
			gateways = make(map[string]gateway, len(items))
			for i := range items {
				gateways[items[i].GetNamespace()+"/"+items[i].GetName()] = stripGateway(&items[i])
			}
		}
	}
	for _, k := range sourceKinds {
		current, err := c.children(ctx, k)
		if err != nil {
			keep(err)
			continue
		}
		familyZones := byFamily[k.family]
		if len(familyZones) == 0 {
			// No zone wants this kind any more: withdraw what it published.
			for _, rs := range current {
				if zonemodel.ExternalSourceOwned(rs) {
					keep(c.remove(ctx, rs))
				}
			}
			continue
		}
		if k.family == familyGateway && gatewayErr != nil {
			// Without the Gateways no route can be projected; keep the
			// record sets of the last complete pass.
			keep(c.listError(k, gatewayErr))
			continue
		}
		items, rv, err := c.list(ctx, k.gvr)
		if err != nil {
			// A partial or refused list is never a delete capability.
			keep(c.listError(k, err))
			continue
		}
		watched = append(watched, watchFrom{k.gvr, rv})
		keep(c.reconcileKind(ctx, k, items, current, familyZones, gateways))
	}
	return watched, firstError
}

func (c *Controller) listError(k *sourceKind, err error) error {
	if apierrors.IsNotFound(err) {
		// The kind is not installed (Gateway API CRDs are optional).
		c.opts.Logger.Debug("external zone source kind not served by the API server", "kind", k.kind)
		return nil
	}
	return fmt.Errorf("%s list: %w", k.kind, err)
}

func (c *Controller) reconcileKind(ctx context.Context, k *sourceKind, items []unstructured.Unstructured, current []*v1alpha1.DNSRecordSet, zones []*v1alpha1.DNSZone, gateways map[string]gateway) error {
	bySource := map[string][]*v1alpha1.DNSRecordSet{}
	for _, rs := range current {
		if zonemodel.ExternalSourceOwned(rs) {
			uid := rs.Annotations[zonemodel.ExternalSourceUIDAnnotation]
			bySource[uid] = append(bySource[uid], rs)
		}
	}
	var firstError error
	seen := map[string]bool{}
	for i := range items {
		s := strip(k, &items[i], gateways)
		if s.uid == "" || s.rv == "" {
			return errors.New("external zones: source identity missing")
		}
		seen[s.uid] = true
		desired, err := c.project(ctx, s, zones)
		if err != nil {
			// Invalid current data withdraws only this source's records.
			c.opts.Logger.Warn("external zone source omitted", "kind", k.kind, "namespace", s.namespace, "name", s.name, "error", err)
			desired = nil
		}
		if err := c.apply(ctx, s, desired, bySource[s.uid], zones); err != nil && firstError == nil {
			// One refused source cannot starve the others; it is not
			// replayed here but recomputed on the next pass.
			firstError = err
		}
	}
	for uid, rows := range bySource {
		if seen[uid] {
			continue
		}
		for _, rs := range rows {
			// A complete list alone is not a delete capability: confirm the
			// exact source UID is gone immediately before cleanup.
			now, err := c.client.Resource(k.gvr).Namespace(rs.Namespace).Get(ctx, rs.Annotations[zonemodel.ExternalSourceNameAnnotation], metav1.GetOptions{})
			if !apierrors.IsNotFound(err) && (err != nil || string(now.GetUID()) == uid) {
				if err == nil {
					err = store.ErrConflict
				}
				if firstError == nil {
					firstError = err
				}
				continue
			}
			if err := c.remove(ctx, rs); err != nil && firstError == nil {
				firstError = err
			}
		}
	}
	return firstError
}

// RecordSetName is the deterministic name of the record set derived from
// the source of kind label (zone.ExternalSourceLabel) with uid for zone.
func RecordSetName(label, uid, zone string) string {
	sum := sha256.Sum256([]byte(label + "/" + uid + "/" + zone))
	return "external-" + label + "-" + hex.EncodeToString(sum[:16])
}

// zoneFor returns the external zone with the longest apex containing name,
// or nil. Two zones with the same apex are ambiguous and select none.
func zoneFor(name string, zones []*v1alpha1.DNSZone) *v1alpha1.DNSZone {
	var best *v1alpha1.DNSZone
	bestLen, ambiguous := -1, false
	for _, z := range zones {
		apex := normHost(z.Spec.ZoneName)
		if name != apex && !strings.HasSuffix(name, "."+apex) && apex != "" {
			continue
		}
		switch {
		case len(apex) > bestLen:
			best, bestLen, ambiguous = z, len(apex), false
		case len(apex) == bestLen:
			ambiguous = true
		}
	}
	if ambiguous {
		return nil
	}
	return best
}

// project builds the desired record sets of one source.
func (c *Controller) project(ctx context.Context, s *source, zones []*v1alpha1.DNSZone) (map[string]*v1alpha1.DNSRecordSet, error) {
	out := map[string]*v1alpha1.DNSRecordSet{}
	if s.deleting {
		return out, nil
	}
	if s.invalid != nil {
		return nil, s.invalid
	}
	names := slices.Sorted(maps.Keys(s.targets))
	for _, name := range names {
		z := zoneFor(name, zones)
		if z == nil {
			continue
		}
		eps, err := endpoints(name, s.targets[name], s.ttl)
		if err != nil {
			continue // nothing to publish yet (no load balancer address)
		}
		child := RecordSetName(s.kind.label, s.uid, z.Name)
		rs := out[child]
		if rs == nil {
			labels := maps.Clone(s.labels)
			if labels == nil {
				labels = map[string]string{}
			}
			labels[zonemodel.ExternalSourceLabel] = s.kind.label
			rs = &v1alpha1.DNSRecordSet{
				TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "DNSRecordSet"},
				ObjectMeta: metav1.ObjectMeta{
					Name: child, Namespace: s.namespace, Labels: labels,
					Annotations: map[string]string{
						zonemodel.ExternalSourceUIDAnnotation:     s.uid,
						zonemodel.ExternalSourceNameAnnotation:    s.name,
						zonemodel.ExternalSourceVersionAnnotation: s.rv,
					},
					OwnerReferences: []metav1.OwnerReference{{APIVersion: s.kind.apiVersion(), Kind: s.kind.kind, Name: s.name, UID: types.UID(s.uid)}},
				},
				Spec: v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: z.Name}},
			}
			store.SetManagedBy(rs, zonemodel.ExternalSourceManager(s.uid))
			selector, err := zonemodel.NewRecordSetSelector(z)
			if err != nil || !selector.Admits(rs) {
				continue // the zone does not take this namespace or these labels
			}
			if len(out) == maxZonesPerSource {
				return nil, store.ErrTooLarge
			}
			out[child] = rs
		}
		rs.Spec.Endpoints = append(rs.Spec.Endpoints, eps...)
	}
	for _, rs := range out {
		if len(rs.Spec.Endpoints) > maxEndpoints {
			return nil, store.ErrTooLarge
		}
		if err := zonemodel.RecordSetError(rs); err != nil {
			return nil, err
		}
		if err := c.opts.Validator.Validate(ctx, rs, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// currentSource confirms the source is unchanged since the list.
func (c *Controller) currentSource(ctx context.Context, s *source) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now, err := c.client.Resource(s.kind.gvr).Namespace(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(now.GetUID()) != s.uid || now.GetResourceVersion() != s.rv {
		return store.ErrConflict
	}
	return c.opts.Admit(ctx)
}

func (c *Controller) apply(ctx context.Context, s *source, desired map[string]*v1alpha1.DNSRecordSet, current []*v1alpha1.DNSRecordSet, zones []*v1alpha1.DNSZone) error {
	manager := zonemodel.ExternalSourceManager(s.uid)
	names := make([]string, 0, len(desired))
	for name := range desired {
		names = append(names, name)
	}
	sort.Strings(names)
	var firstError error
	for _, name := range names {
		rs := desired[name]
		var old v1alpha1.DNSRecordSet
		err := c.opts.Store.Get(ctx, store.KeyOf(rs), &old)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		absent := errors.Is(err, store.ErrNotFound)
		if !absent && (!zonemodel.ExternalSourceOwned(&old) || store.ManagedBy(&old) != manager) {
			// A foreign record set with this name is never taken over.
			c.opts.Logger.Warn("external zone record set name held by another writer", "namespace", rs.Namespace, "name", rs.Name, "owner", store.ManagedBy(&old))
			if firstError == nil {
				firstError = &store.ErrManagedBy{Key: store.KeyOf(rs), Owner: store.ManagedBy(&old)}
			}
			continue
		}
		if !absent && reflect.DeepEqual(old.Spec, rs.Spec) && reflect.DeepEqual(old.Labels, rs.Labels) && reflect.DeepEqual(old.OwnerReferences, rs.OwnerReferences) {
			continue
		}
		if err := c.currentZone(ctx, zones, rs.Spec.ZoneRef.Name); err != nil {
			return err
		}
		if err := c.currentSource(ctx, s); err != nil {
			return err
		}
		opts := store.ApplyOptions{Manager: manager, CreateOnly: absent}
		if !absent {
			opts.ExpectedResourceVersion = old.ResourceVersion
		}
		if _, err := c.opts.Store.Apply(ctx, rs, opts); err != nil {
			return err
		}
	}
	for _, rs := range current {
		if desired[rs.Name] != nil {
			continue
		}
		if err := c.currentSource(ctx, s); err != nil {
			return err
		}
		if err := c.remove(ctx, rs); err != nil {
			return err
		}
	}
	return firstError
}

// currentZone confirms the zone is unchanged since the list.
func (c *Controller) currentZone(ctx context.Context, zones []*v1alpha1.DNSZone, name string) error {
	for _, original := range zones {
		if original.Name != name {
			continue
		}
		var now v1alpha1.DNSZone
		if err := c.opts.Store.Get(ctx, store.KeyOf(original), &now); err != nil {
			return err
		}
		if now.UID != original.UID || now.ResourceVersion != original.ResourceVersion {
			return store.ErrConflict
		}
		return nil
	}
	return store.ErrConflict
}

// remove deletes one derived record set by its observed identity.
func (c *Controller) remove(ctx context.Context, rs *v1alpha1.DNSRecordSet) error {
	if !zonemodel.ExternalSourceOwned(rs) {
		return errors.New("external zones: unowned cleanup candidate")
	}
	if err := c.opts.Admit(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	manager := store.ManagedBy(rs)
	err := c.opts.Store.Delete(ctx, store.KeyOf(rs), store.DeleteOptions{Manager: manager, ExpectedUID: string(rs.UID), ExpectedResourceVersion: rs.ResourceVersion})
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}
