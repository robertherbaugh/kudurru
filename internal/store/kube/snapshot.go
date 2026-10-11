package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube/localdb"
)

// ErrNotSynced is returned by MaterializedSnapshot before every informer
// has completed its initial list.
var ErrNotSynced = errors.New("store/kube: informers have not synced")

// snapshotPageSize is the page size Snapshot uses against the apiserver.
const snapshotPageSize = 500

// Snapshot implements store.Store: a `kind: List` YAML document of every
// kudurru.io object, read page by page from the apiserver (each kind is a
// consistent list) and sorted by key. It is the backup format and what
// `kudurru apply -f` and Restore accept. managedFields are omitted.
// opts.Safe has nothing to strip yet: the internal kinds (Secrets labelled
// kudurru.io/kind) are not served by this driver in v0.1.
func (s *Store) Snapshot(ctx context.Context, w io.Writer, _ store.SnapshotOptions) error {
	if s.isClosed() {
		return store.ErrClosed
	}
	var items []store.Object
	for _, k := range s.kinds.kinds {
		cont := ""
		for {
			page, err := s.List(ctx, k, store.ListOptions{Limit: snapshotPageSize, Continue: cont})
			if err != nil {
				return err
			}
			items = append(items, page.Items...)
			if cont = page.Continue; cont == "" {
				break
			}
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return store.KeyOf(items[i]).String() < store.KeyOf(items[j]).String() })
	if items == nil {
		items = []store.Object{}
	}
	b, err := yaml.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// Restore implements store.Store as apply: every object in r (a `kind:
// List` document, a single object, or a multi-document YAML/JSON stream) is
// applied with its recorded managed-by owner as Manager, or "bootstrap" if
// it has none. Status is not restored; the controller recomputes it. All
// objects are attempted; the errors are joined.
func (s *Store) Restore(ctx context.Context, r io.Reader) error {
	if s.isClosed() {
		return store.ErrClosed
	}
	objs, err := decodeObjects(s.opts.Scheme, r)
	if err != nil {
		return err
	}
	var errs []error
	for _, o := range objs {
		manager := store.ManagedBy(o)
		if manager == "" {
			manager = store.ManagerBootstrap
		}
		o.SetResourceVersion("")
		if _, err := s.Apply(ctx, o, store.ApplyOptions{Manager: manager}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// decodeObjects reads typed kudurru objects from a YAML or JSON stream,
// flattening `kind: List` documents.
func decodeObjects(scheme *runtime.Scheme, r io.Reader) ([]store.Object, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(r, 4096)
	var out []store.Object
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, fmt.Errorf("store/kube: decode: %w", err)
		}
		if len(doc) == 0 {
			continue
		}
		u := &unstructured.Unstructured{Object: doc}
		if u.IsList() {
			list, err := u.ToList()
			if err != nil {
				return nil, fmt.Errorf("store/kube: decode list: %w", err)
			}
			for i := range list.Items {
				o, err := typed(scheme, &list.Items[i])
				if err != nil {
					return nil, err
				}
				out = append(out, o)
			}
			continue
		}
		o, err := typed(scheme, u)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
}

func typed(scheme *runtime.Scheme, u *unstructured.Unstructured) (store.Object, error) {
	gvk := u.GroupVersionKind()
	ro, err := scheme.New(gvk)
	if err != nil {
		return nil, fmt.Errorf("store/kube: %s %s: %w", gvk, u.GetName(), err)
	}
	obj, ok := ro.(store.Object)
	if !ok {
		return nil, fmt.Errorf("store/kube: %s is not a kudurru object", gvk)
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, obj); err != nil {
		return nil, fmt.Errorf("store/kube: %s %s: %w", gvk, u.GetName(), err)
	}
	obj.GetObjectKind().SetGroupVersionKind(gvk)
	return obj, nil
}

// MaterializedSnapshot returns the informer cache's current object set and
// per-kind resourceVersions, ready for localdb.SnapshotStore.Write. Each
// kind's resourceVersion is read before its objects, so the objects are at
// least as fresh as the recorded high-water mark. It returns ErrNotSynced
// until every informer has synced.
func (s *Store) MaterializedSnapshot(ctx context.Context) (*localdb.Snapshot, error) {
	if s.isClosed() {
		return nil, store.ErrClosed
	}
	select {
	case <-s.informersReady:
	default:
		return nil, ErrNotSynced
	}
	snap := &localdb.Snapshot{ResourceVersions: map[store.Kind]string{}, WrittenAt: time.Now()}
	if id := s.clusterID.Load(); id != nil {
		snap.ClusterID = *id
	}
	for _, k := range s.kinds.kinds {
		ki := s.kinds.byKind[k]
		inf, err := s.cache.GetInformer(ctx, ki.newObject(), cache.BlockUntilSynced(false))
		if err != nil {
			return nil, err
		}
		if !inf.HasSynced() {
			return nil, ErrNotSynced
		}
		if lsr, ok := inf.(interface{ LastSyncResourceVersion() string }); ok {
			snap.ResourceVersions[k] = lsr.LastSyncResourceVersion()
		}
		list := ki.newList().(client.ObjectList)
		if err := s.cache.List(ctx, list); err != nil {
			return nil, err
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			o := it.(store.Object)
			normalize(ki, o)
			snap.Objects = append(snap.Objects, o)
		}
	}
	return snap, nil
}

// PersistSnapshots keeps local.db current (§4.4): it writes once every
// informer has synced and then, at most every interval, whenever an
// informer reported a change. It returns when ctx ends. Write failures are
// logged and retried on the next tick.
func (s *Store) PersistSnapshots(ctx context.Context, db *localdb.SnapshotStore, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("store/kube: snapshot interval must be positive, got %s", interval)
	}
	select {
	case <-s.informersReady:
	case <-ctx.Done():
		return nil
	}
	var dirty atomic.Bool
	mark := toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { dirty.Store(true) },
		UpdateFunc: func(any, any) { dirty.Store(true) },
		DeleteFunc: func(any) { dirty.Store(true) },
	}
	for _, k := range s.kinds.kinds {
		inf, err := s.cache.GetInformer(ctx, s.kinds.byKind[k].newObject(), cache.BlockUntilSynced(false))
		if err != nil {
			return err
		}
		reg, err := inf.AddEventHandlerWithOptions(mark, toolscache.HandlerOptions{ResyncPeriod: ptr.To(time.Duration(0))})
		if err != nil {
			return err
		}
		defer func() { _ = inf.RemoveEventHandler(reg) }()
	}
	if !s.cache.WaitForCacheSync(ctx) {
		return nil
	}
	dirty.Store(true)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if dirty.Swap(false) {
			if err := s.persistOnce(ctx, db); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				s.log.Warn("local.db snapshot write failed; retrying", "err", err)
				dirty.Store(true)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func (s *Store) persistOnce(ctx context.Context, db *localdb.SnapshotStore) error {
	snap, err := s.MaterializedSnapshot(ctx)
	if err != nil {
		return err
	}
	return db.Write(ctx, snap)
}
