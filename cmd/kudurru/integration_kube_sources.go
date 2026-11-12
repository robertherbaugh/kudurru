package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/inbound"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// kubeMACGrantsInput is the zonecache external input that carries the MAC
// grants of the Kubernetes source runtime into the policy builder (D384).
const kubeMACGrantsInput = "integration.macGrants"

// kubeSourceCache is the part of the replica's zonecache the source runtime
// uses: the snapshot it checks eligibility against, the input it installs
// grants through and the publish hook that tells it a snapshot changed.
type kubeSourceCache interface {
	Snapshot() *zonecache.Snapshot
	Initialized() <-chan struct{}
	OnPublish(func(*zonecache.Snapshot))
	SetInput(string, any)
}

// kubeSourceRuntime is the replica-side consumer of committed integration
// source heads on Kubernetes (F09b-K5, D384). Only the Lease holder of an
// Integration observes its provider, but every replica answers DNS, so every
// replica runs this runtime. It lists every committed IntegrationSourceHead
// with the chunks it references and every name allocation (a bounded relist
// woken by a watch of the heads and by this replica's own commits, with a
// timer as the backstop), decodes them strictly, and
// on every relist or published object snapshot decides which sources are
// eligible against this replica's own snapshot: the Integration has the
// head's owner UID, is not suspended or deleting, runs on this store, and
// its spec digest is the head's ConfigDigest; every target zone and every
// evidenced child exists here with the same UID and intent digest. The
// eligible sources are rebuilt into one inventory in a canonical order
// (accepted time, then source ID), so every replica that reads the same heads
// arbitrates addresses the same way; cross-source removal watermarks are not
// durable on this store. Their MAC contributions become policy grants with
// the original exclusive expiry. A failed relist keeps the last listing:
// clients and grants then end at their original absolute expiry, and
// nothing renews them.
type kubeSourceRuntime struct {
	store     *kube.Store
	cache     kubeSourceCache
	namespace string
	now       func() time.Time
	logger    *slog.Logger
	interval  time.Duration

	wake     chan struct{}
	evaluate chan struct{}
	current  atomic.Pointer[inventory.Snapshot]

	mu      sync.Mutex
	listing *inbound.DecodedImage
	chunks  map[string]*kinds.IntegrationSourceChunk
	roots   map[string]string
	grants  []policy.MACGrant
	changed chan struct{}
}

func newKubeSourceRuntime(st *kube.Store, cache kubeSourceCache, namespace string, now func() time.Time, logger *slog.Logger) *kubeSourceRuntime {
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &kubeSourceRuntime{store: st, cache: cache, namespace: namespace, now: now, logger: logger, interval: 10 * time.Second, wake: make(chan struct{}, 1), evaluate: make(chan struct{}, 1), chunks: map[string]*kinds.IntegrationSourceChunk{}, roots: map[string]string{}, changed: make(chan struct{})}
}

// Snapshot implements the identify stage's inventory source.
func (k *kubeSourceRuntime) Snapshot() *inventory.Snapshot { return k.current.Load() }

// Wake asks for a relist, for example after this replica committed a head.
func (k *kubeSourceRuntime) Wake() {
	select {
	case k.wake <- struct{}{}:
	default:
	}
}

// Installed returns the root digest this replica serves for the head name,
// and a channel closed at the next change of the installed roots.
func (k *kubeSourceRuntime) Installed(head string) (string, <-chan struct{}) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.roots[head], k.changed
}

// Run relists and re-evaluates until ctx ends.
func (k *kubeSourceRuntime) Run(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case <-k.cache.Initialized():
	}
	k.cache.OnPublish(func(s *zonecache.Snapshot) {
		// Only objects eligibility reads; the runtime's own grant input
		// changes none of them, so installing grants cannot loop.
		for _, kind := range []store.Kind{"Integration", "ClientGroup", "DNSZone", "DNSRecordSet"} {
			if s.Objects.Changed(kind) {
				select {
				case k.evaluate <- struct{}{}:
				default:
				}
				return
			}
		}
	})
	go k.watchHeads(ctx)
	ticker := time.NewTicker(k.interval)
	defer ticker.Stop()
	k.relist(ctx)
	for {
		k.install()
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			k.relist(ctx)
		case <-k.wake:
			k.relist(ctx)
		case <-k.evaluate:
		}
	}
}

// watchHeads wakes a relist on every change of a committed head, so a
// replica that is not the holder installs a successor head within one watch
// delivery instead of one relist interval: an observation's expiry is three
// polls, and the shortest poll is one second. The timer relist stays as the
// backstop for a watch that is down; a failed watch is opened again after a
// second.
func (k *kubeSourceRuntime) watchHeads(ctx context.Context) {
	for ctx.Err() == nil {
		w, err := k.store.Watch(ctx, kinds.KindIntegrationSourceHead, store.WatchOptions{})
		if err == nil {
			for open := true; open; {
				select {
				case <-ctx.Done():
					open = false
				case ev, ok := <-w.ResultChan():
					open = ok
					if ok && ev.Type != store.Bookmark {
						k.Wake()
					}
				}
			}
			w.Stop()
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// relist reads every head, the chunks they reference and every allocation,
// and decodes them as one listing. Chunks are immutable and content
// addressed, so a chunk read once is kept while a head references it. A
// chunk that is gone is the ordinary race with a holder retiring the
// previous head's chunks right after its commit, so that relist is repeated
// at once, up to twice. Any other failure keeps the previous listing.
func (k *kubeSourceRuntime) relist(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	listing, err := k.read(ctx)
	for retry := 0; retry < 2 && errors.Is(err, store.ErrNotFound) && ctx.Err() == nil; retry++ {
		listing, err = k.read(ctx)
	}
	if err != nil {
		if ctx.Err() == nil {
			k.logger.Warn("integration sources: relist failed; keeping the last listing until its observations expire", "error", err)
		}
		return
	}
	k.mu.Lock()
	k.listing = listing
	k.mu.Unlock()
}

func (k *kubeSourceRuntime) read(ctx context.Context) (*inbound.DecodedImage, error) {
	clusterID := k.store.Info().ClusterID
	if clusterID == "" {
		return nil, ih.ErrUnavailable
	}
	backend, err := k.store.IntegrationSourceBackend()
	if err != nil {
		return nil, err
	}
	heads, err := backend.Heads(ctx)
	if err != nil {
		return nil, err
	}
	allocations, err := backend.Allocations(ctx)
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	known := k.chunks
	k.mu.Unlock()
	chunks := map[string]*kinds.IntegrationSourceChunk{}
	var listed []*kinds.IntegrationSourceChunk
	for _, head := range heads {
		for _, ref := range head.Spec.Chunks {
			c := known[ref.Name]
			if c == nil {
				var read kinds.IntegrationSourceChunk
				if err := k.store.Get(ctx, store.Key{Kind: kinds.KindIntegrationSourceChunk, Name: ref.Name}, &read); err != nil {
					return nil, err
				}
				c = &read
			}
			if chunks[ref.Name] == nil {
				chunks[ref.Name] = c
				listed = append(listed, c)
			}
		}
	}
	listing, err := inbound.DecodeCarriers(inbound.CarrierListing{ClusterID: clusterID, Heads: heads, Chunks: listed, Allocations: allocations})
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	k.chunks = chunks
	k.mu.Unlock()
	return listing, nil
}

// install evaluates the last listing against the current snapshot and
// installs the inventory and grants it yields.
func (k *kubeSourceRuntime) install() {
	k.mu.Lock()
	listing := k.listing
	k.mu.Unlock()
	if listing == nil {
		return
	}
	snap := k.cache.Snapshot()
	if snap == nil {
		return
	}
	at := k.now().UTC()
	inv, roots, grants, err := kubeSourceImage(listing, snap.Objects, k.store.Info().ClusterID, k.namespace, at)
	if err != nil {
		k.logger.Warn("integration sources: committed heads could not be assembled; keeping the installed sources until their observations expire", "error", err)
		return
	}
	k.current.Store(inv)
	k.mu.Lock()
	if !reflect.DeepEqual(k.roots, roots) {
		k.roots = roots
		close(k.changed)
		k.changed = make(chan struct{})
	}
	same := reflect.DeepEqual(k.grants, grants)
	k.grants = grants
	k.mu.Unlock()
	if !same {
		k.cache.SetInput(kubeMACGrantsInput, grants)
	}
}

// kubeSourceImage is the pure assembly of one replica's integration sources
// from a decoded listing and the replica's object snapshot (see
// kubeSourceRuntime).
func kubeSourceImage(listing *inbound.DecodedImage, objects zonecache.ObjectSet, clusterID, namespace string, at time.Time) (*inventory.Snapshot, map[string]string, []policy.MACGrant, error) {
	type candidate struct {
		source   inbound.SourceImage
		document inbound.Document
	}
	var eligible []candidate
	var admissions []inbound.SourceAdmission
	var sources []inbound.SourceImage
	for _, source := range listing.Sources {
		h := source.Head.Spec
		// A head another replica accepted at a time this replica's clock has
		// not reached yet is left out until it has, never refused as a whole.
		if h.AcceptedAt.After(at) || h.WithdrawnAt.After(at) {
			continue
		}
		sources = append(sources, source)
		obj, ok := objects.Get(store.Key{Kind: "Integration", Namespace: h.Owner.Namespace, Name: h.Owner.Name})
		d, typed := obj.(*public.Integration)
		if !ok || !typed || string(d.UID) != h.Owner.UID || d.Spec.Suspend || !d.DeletionTimestamp.IsZero() || kubeIntegrationRefusal(namespace, d) != "" || h.State != "active" {
			continue
		}
		raw, err := json.Marshal(d.Spec)
		if err != nil || kinds.Digest(raw) != h.ConfigDigest {
			continue
		}
		document, err := inbound.DecodeSource(source)
		if err != nil {
			return nil, nil, nil, err
		}
		if !kubeSourceEvidenced(document, objects) {
			continue
		}
		eligible = append(eligible, candidate{source, document})
		admissions = append(admissions, inbound.SourceAdmission{Owner: h.Owner, ConfigDigest: h.ConfigDigest, RootDigest: h.RootDigest, Ready: true, Targets: document.Targets})
	}
	// One canonical order, so every replica reading the same heads derives
	// the same arbitration.
	slices.SortFunc(eligible, func(a, b candidate) int {
		if c := a.source.Head.Spec.AcceptedAt.Compare(b.source.Head.Spec.AcceptedAt); c != 0 {
			return c
		}
		return strings.Compare(ih.SourceID(a.source.Head.Spec.Owner), ih.SourceID(b.source.Head.Spec.Owner))
	})
	state := inventory.State{FormatVersion: inventory.StateFormatVersion}
	for _, c := range eligible {
		h := c.source.Head.Spec
		next, err := inventory.PrepareReplacement(state, ih.SourceID(h.Owner), h.Sequence, c.document.Snapshot.Clients, h.AcceptedAt)
		if err != nil {
			return nil, nil, nil, err
		}
		state = next
	}
	prepared, err := inbound.Prepare(inbound.ImageInput{ClusterID: clusterID, Sources: sources, Admissions: admissions, Inventory: state, Groups: objects.ClientGroups(), At: at})
	if err != nil {
		return nil, nil, nil, err
	}
	holder, err := inventory.RestoreState(prepared.Inventory, func() time.Time { return at })
	if err != nil {
		return nil, nil, nil, err
	}
	snapshot := holder.Snapshot()
	roots, grants, err := integrationSourceGrants(prepared, snapshot, func(key store.Key) (*public.ClientGroup, bool) {
		obj, ok := objects.Get(key)
		group, typed := obj.(*public.ClientGroup)
		return group, ok && typed
	}, at)
	if err != nil {
		return nil, nil, nil, err
	}
	return snapshot, roots, grants, nil
}

// kubeSourceEvidenced reports whether every target zone and every child the
// document evidences exists in the snapshot as that exact incarnation with
// the same intent, and every child is still owned by the source.
func kubeSourceEvidenced(document inbound.Document, objects zonecache.ObjectSet) bool {
	for _, target := range document.Targets {
		z, ok := objects.Zone(target.Object.Name)
		if !ok || string(z.UID) != target.Object.UID || public.IsDisabled(z) || !z.DeletionTimestamp.IsZero() {
			return false
		}
		if digest, err := inbound.IntentDigest(z.Spec); err != nil || digest != target.Object.IntentDigest {
			return false
		}
	}
	for _, proof := range document.Objects {
		obj, ok := objects.Get(store.Key{Kind: store.Kind(proof.Kind), Namespace: proof.Namespace, Name: proof.Name})
		if !ok || string(obj.GetUID()) != proof.UID || !sourceOwned(obj, document.Owner) {
			return false
		}
		var spec any
		switch o := obj.(type) {
		case *public.DNSRecordSet:
			spec = o.Spec
		case *public.DNSZone:
			spec = o.Spec
		default:
			return false
		}
		if digest, err := inbound.IntentDigest(spec); err != nil || digest != proof.IntentDigest {
			return false
		}
	}
	return true
}

// await waits until this replica serves root for the head name.
func (k *kubeSourceRuntime) await(ctx context.Context, head, root string) error {
	for {
		installed, changed := k.Installed(head)
		if installed == root {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
