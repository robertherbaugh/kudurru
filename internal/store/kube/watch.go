package kube

import (
	"context"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/resourceversion"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// watchBuffer is the result channel capacity. Event delivery never drops:
// when the consumer is slower than the informer, the per-handler buffer of
// the shared informer absorbs the backlog.
const watchBuffer = 256

// Watch implements store.Store over the kind's shared informer:
//
//   - every object in the informer's initial list arrives as store.Added;
//   - then one store.Bookmark whose Object is an empty object of the kind
//     carrying the informer's last-synced resourceVersion (the high-water
//     mark zonecache records in local.db);
//   - then live Added/Modified/Deleted events, filtered by opts.Namespace.
//
// Before the handler is registered the informer is brought up to date with
// a consistent metadata list from the apiserver (see catchUp), so the
// initial Added events include every write that completed before Watch was
// called, as with the other drivers.
//
// opts.ResourceVersion is accepted but the informer always replays its
// current state as Added: consumers treat Added as an upsert (level-
// triggered, compaction-safe). Informer resyncs (same resourceVersion) are
// not forwarded. The watch ends on Stop, when ctx is done, or on Close.
func (s *Store) Watch(ctx context.Context, kind store.Kind, opts store.WatchOptions) (store.Watcher, error) {
	if s.isClosed() {
		return nil, store.ErrClosed
	}
	if ki, ok := s.internal.get(kind); ok {
		return s.watchInternal(ctx, ki, opts)
	}
	ki, err := s.kinds.get(kind)
	if err != nil {
		return nil, err
	}
	inf, err := s.cache.GetInformer(ctx, ki.newObject(), cache.BlockUntilSynced(false))
	if err != nil {
		return nil, err
	}
	s.catchUp(ctx, ki, inf, opts.Namespace)
	w := &watcher{ch: make(chan store.Event, watchBuffer), done: make(chan struct{}), ki: ki, ns: opts.Namespace}
	reg, err := inf.AddEventHandlerWithOptions(w, toolscache.HandlerOptions{ResyncPeriod: ptr.To(time.Duration(0))})
	if err != nil {
		return nil, err
	}
	// Both hooks are set before the watcher becomes visible to Close.
	w.remove = func() { _ = inf.RemoveEventHandler(reg) }
	w.untrack = func() {
		s.mu.Lock()
		delete(s.watchers, w)
		s.mu.Unlock()
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		w.Stop()
		return nil, store.ErrClosed
	}
	s.watchers[w] = struct{}{}
	s.mu.Unlock()

	go func() {
		select {
		case <-reg.HasSyncedChecker().Done():
		case <-w.done:
			return
		}
		bm := ki.newObject()
		if lsr, ok := inf.(interface{ LastSyncResourceVersion() string }); ok {
			bm.SetResourceVersion(lsr.LastSyncResourceVersion())
		}
		w.send(store.Event{Type: store.Bookmark, Object: bm})
	}()
	go func() {
		select {
		case <-ctx.Done():
			w.Stop()
		case <-w.done:
		}
	}()
	return w, nil
}

// catchUpTimeout bounds how long Watch waits for its informer to reach the
// apiserver's current state.
const catchUpTimeout = 10 * time.Second

// catchUp blocks until the kind's informer reflects at least the state of a
// consistent metadata list issued now: every listed object is present at
// its listed resourceVersion or newer, and every other cached object is
// newer than the list. The informer's last-synced resourceVersion reaching
// the list's resourceVersion short-circuits the check (it also covers
// objects deleted after the list). resourceVersions are compared as
// integers (k8s.io/apimachinery/pkg/util/resourceversion, KEP-5504).
//
// The barrier is best effort: if the apiserver is unreachable or the
// informer does not catch up within catchUpTimeout, the watch starts from
// whatever the informer has and converges as events arrive.
func (s *Store) catchUp(ctx context.Context, ki *kindInfo, inf cache.Informer, ns string) {
	ctx, cancel := context.WithTimeout(ctx, catchUpTimeout)
	defer cancel()
	var lo []client.ListOption
	if ns != "" {
		lo = append(lo, client.InNamespace(ns))
	}
	want := &metav1.PartialObjectMetadataList{}
	want.SetGroupVersionKind(ki.listGVK)
	if err := s.c.List(ctx, want, lo...); err != nil {
		s.log.Debug("watch: consistency list failed; starting from the informer as is", "kind", ki.kind, "err", err)
		return
	}
	wantRV := map[types.NamespacedName]string{}
	for i := range want.Items {
		it := &want.Items[i]
		wantRV[types.NamespacedName{Namespace: it.Namespace, Name: it.Name}] = it.ResourceVersion
	}
	lsr, _ := inf.(interface{ LastSyncResourceVersion() string })
	for {
		if lsr != nil && rvAtLeast(lsr.LastSyncResourceVersion(), want.ResourceVersion) {
			return
		}
		if s.cacheReflects(ctx, ki, lo, wantRV, want.ResourceVersion) {
			return
		}
		select {
		case <-ctx.Done():
			s.log.Debug("watch: informer did not catch up in time", "kind", ki.kind)
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// cacheReflects reports whether the cached objects of a kind include every
// wanted object at its wanted resourceVersion or newer, and nothing older
// than listRV that the list did not contain.
func (s *Store) cacheReflects(ctx context.Context, ki *kindInfo, lo []client.ListOption, want map[types.NamespacedName]string, listRV string) bool {
	have := ki.newList().(client.ObjectList)
	if err := s.cache.List(ctx, have, append(lo, client.UnsafeDisableDeepCopy)...); err != nil {
		return false
	}
	items, err := meta.ExtractList(have)
	if err != nil {
		return false
	}
	seen := 0
	for _, it := range items {
		o := it.(store.Object)
		rv, ok := want[types.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()}]
		if ok {
			if !rvAtLeast(o.GetResourceVersion(), rv) {
				return false
			}
			seen++
			continue
		}
		if !rvAtLeast(o.GetResourceVersion(), listRV) {
			return false // deleted before the list; the informer has not seen it yet
		}
	}
	return seen == len(want)
}

// rvAtLeast reports a >= b for well-formed resourceVersions; malformed
// values compare false.
func rvAtLeast(a, b string) bool {
	c, err := resourceversion.CompareResourceVersion(a, b)
	return err == nil && c >= 0
}

// watcher adapts informer callbacks to a store.Watcher. Senders hold mu for
// reading while they block on the channel; Stop closes done first (which
// releases blocked senders) and then takes mu for writing before closing
// the channel, so a send never races with close.
type watcher struct {
	ch      chan store.Event
	done    chan struct{}
	ki      *kindInfo
	ns      string
	remove  func()
	untrack func()
	// convert, when set, turns an informer object into the store object
	// (the Secrets of internal kinds); false skips it.
	convert func(obj any) (store.Object, bool)

	mu      sync.RWMutex
	stopped bool
	once    sync.Once
}

var _ toolscache.ResourceEventHandler = (*watcher)(nil)

// ResultChan implements store.Watcher.
func (w *watcher) ResultChan() <-chan store.Event { return w.ch }

// Stop implements store.Watcher; it is idempotent.
func (w *watcher) Stop() {
	w.once.Do(func() {
		close(w.done)
		if w.remove != nil {
			w.remove()
		}
		w.mu.Lock()
		w.stopped = true
		close(w.ch)
		w.mu.Unlock()
		if w.untrack != nil {
			w.untrack()
		}
	})
}

func (w *watcher) send(ev store.Event) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.stopped {
		return
	}
	select {
	case w.ch <- ev:
	case <-w.done:
	}
}

// emit copies an informer object (shared, read-only) and forwards it.
func (w *watcher) emit(t store.EventType, obj any) {
	if w.convert != nil {
		o, ok := w.convert(obj) // a fresh object, already normalized
		if ok && (w.ns == "" || o.GetNamespace() == w.ns) {
			w.send(store.Event{Type: t, Object: o})
		}
		return
	}
	o, ok := obj.(store.Object)
	if !ok {
		return
	}
	if w.ns != "" && o.GetNamespace() != w.ns {
		return
	}
	cp := o.DeepCopyObject().(store.Object)
	normalize(w.ki, cp)
	w.send(store.Event{Type: t, Object: cp})
}

// OnAdd implements toolscache.ResourceEventHandler.
func (w *watcher) OnAdd(obj any, _ bool) { w.emit(store.Added, obj) }

// OnUpdate implements toolscache.ResourceEventHandler; resyncs that carry
// an unchanged resourceVersion are dropped.
func (w *watcher) OnUpdate(oldObj, newObj any) {
	o, ok1 := oldObj.(store.Object)
	n, ok2 := newObj.(store.Object)
	if ok1 && ok2 && o.GetResourceVersion() == n.GetResourceVersion() {
		return
	}
	w.emit(store.Modified, newObj)
}

// OnDelete implements toolscache.ResourceEventHandler, unwrapping the
// tombstone the informer delivers when it missed the final state.
func (w *watcher) OnDelete(obj any) {
	if t, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
		obj = t.Obj
	}
	w.emit(store.Deleted, obj)
}
