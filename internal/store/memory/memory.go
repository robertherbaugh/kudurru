// Package memory is an in-process Store used by tests and as the reference
// implementation of the Apply contract. It is not a production driver.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// Store implements store.Store in memory.
type Store struct {
	mu       sync.RWMutex
	objects  map[store.Key]store.Object
	rv       uint64
	watchers map[store.Kind][]*watcher
	closed   bool
	now      func() time.Time
}

// New returns an empty store.
func New() *Store {
	return &Store{objects: map[store.Key]store.Object{}, watchers: map[store.Kind][]*watcher{}, now: time.Now}
}

func (s *Store) nextRV() string {
	s.rv++
	return strconv.FormatUint(s.rv, 10)
}

// Get implements store.Store.
func (s *Store) Get(_ context.Context, key store.Key, into store.Object) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return store.ErrClosed
	}
	obj, ok := s.objects[key]
	if !ok {
		return store.ErrNotFound
	}
	return copyInto(obj, into)
}

// List implements store.Store.
func (s *Store) List(_ context.Context, kind store.Kind, opts store.ListOptions) (store.ObjectList, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return store.ObjectList{}, store.ErrClosed
	}
	keys := make([]store.Key, 0)
	for k, obj := range s.objects {
		if k.Kind != kind || (opts.Namespace != "" && k.Namespace != opts.Namespace) {
			continue
		}
		if !matchLabels(obj.GetLabels(), opts.LabelSelector) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	start := 0
	if opts.Continue != "" {
		start, _ = strconv.Atoi(opts.Continue)
	}
	out := store.ObjectList{}
	for i := start; i < len(keys); i++ {
		if opts.Limit > 0 && len(out.Items) >= opts.Limit {
			out.Continue = strconv.Itoa(i)
			break
		}
		out.Items = append(out.Items, s.objects[keys[i]].DeepCopyObject().(store.Object))
	}
	return out, nil
}

// Apply implements store.Store with the Apply contract of §4.3: whole-object
// apply, managedBy single writer, optimistic resourceVersion.
func (s *Store) Apply(_ context.Context, obj store.Object, opts store.ApplyOptions) (store.Object, error) {
	if opts.Manager == "" {
		return nil, fmt.Errorf("store: ApplyOptions.Manager is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, store.ErrClosed
	}
	key := store.KeyOf(obj)
	prev, exists := s.objects[key]

	// Apply writes the main resource only: status comes from the stored
	// object (or stays empty on create), and generation moves only when the
	// spec changed, as the apiserver does (D70).
	nextMap, err := sections(obj)
	if err != nil {
		return nil, err
	}
	var prevMap map[string]any
	if exists {
		if prevMap, err = sections(prev); err != nil {
			return nil, err
		}
		nextMap["status"] = prevMap["status"]
	} else {
		delete(nextMap, "status")
	}
	next, err := rebuild(obj, nextMap)
	if err != nil {
		return nil, err
	}

	if exists {
		if opts.ExpectedResourceVersion != "" && opts.ExpectedResourceVersion != prev.GetResourceVersion() {
			return nil, store.ErrConflict
		}
		if owner := store.ManagedBy(prev); owner != "" && owner != opts.Manager && !opts.Force {
			return nil, &store.ErrManagedBy{Key: key, Owner: owner}
		}
		// Caller-stamped identity is kept from the stored object (D10).
		next.SetUID(prev.GetUID())
		next.SetCreationTimestamp(prev.GetCreationTimestamp())
		next.SetGeneration(prev.GetGeneration())
		if !reflect.DeepEqual(prevMap["spec"], nextMap["spec"]) {
			next.SetGeneration(prev.GetGeneration() + 1)
		}
	} else {
		if next.GetUID() == "" {
			next.SetUID(types.UID(fmt.Sprintf("mem-%d", s.rv+1)))
		}
		if ct := next.GetCreationTimestamp(); ct.IsZero() {
			next.SetCreationTimestamp(metav1.NewTime(s.now()))
		}
		next.SetGeneration(1)
	}
	store.SetManagedBy(next, opts.Manager)
	next.SetResourceVersion(s.nextRV())
	s.objects[key] = next

	typ := store.Modified
	if !exists {
		typ = store.Added
	}
	s.broadcast(key.Kind, store.Event{Type: typ, Object: next.DeepCopyObject().(store.Object)})
	return next.DeepCopyObject().(store.Object), nil
}

// Delete implements store.Store.
func (s *Store) Delete(_ context.Context, key store.Key, opts store.DeleteOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return store.ErrClosed
	}
	prev, ok := s.objects[key]
	if !ok {
		return store.ErrNotFound
	}
	if owner := store.ManagedBy(prev); owner != "" && opts.Manager != "" && owner != opts.Manager && !opts.Force {
		return &store.ErrManagedBy{Key: key, Owner: owner}
	}
	delete(s.objects, key)
	s.broadcast(key.Kind, store.Event{Type: store.Deleted, Object: prev})
	return nil
}

// UpdateStatus implements store.Store. Status is whatever the caller set on
// obj; metadata and spec are taken from the stored copy (the status
// subresource contract).
func (s *Store) UpdateStatus(_ context.Context, obj store.Object) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return store.ErrClosed
	}
	key := store.KeyOf(obj)
	prev, ok := s.objects[key]
	if !ok {
		return store.ErrNotFound
	}
	prevMap, err := sections(prev)
	if err != nil {
		return err
	}
	objMap, err := sections(obj)
	if err != nil {
		return err
	}
	prevMap["status"] = objMap["status"]
	next, err := rebuild(prev, prevMap)
	if err != nil {
		return err
	}
	next.SetResourceVersion(s.nextRV())
	s.objects[key] = next
	s.broadcast(key.Kind, store.Event{Type: store.Modified, Object: next.DeepCopyObject().(store.Object)})
	return nil
}

type watcher struct {
	ch   chan store.Event
	stop func()
	once sync.Once
}

func (w *watcher) ResultChan() <-chan store.Event { return w.ch }
func (w *watcher) Stop()                          { w.once.Do(w.stop) }

// Watch implements store.Store: current objects are delivered as Added,
// then live events follow.
func (s *Store) Watch(ctx context.Context, kind store.Kind, opts store.WatchOptions) (store.Watcher, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, store.ErrClosed
	}
	w := &watcher{ch: make(chan store.Event, 256)}
	w.stop = func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		ws := s.watchers[kind]
		for i, x := range ws {
			if x == w {
				s.watchers[kind] = append(ws[:i], ws[i+1:]...)
				break
			}
		}
		close(w.ch)
	}
	for k, obj := range s.objects {
		if k.Kind == kind && (opts.Namespace == "" || k.Namespace == opts.Namespace) {
			w.ch <- store.Event{Type: store.Added, Object: obj.DeepCopyObject().(store.Object)}
		}
	}
	w.ch <- store.Event{Type: store.Bookmark}
	s.watchers[kind] = append(s.watchers[kind], w)
	go func() { <-ctx.Done(); w.Stop() }()
	return w, nil
}

func (s *Store) broadcast(kind store.Kind, ev store.Event) {
	for _, w := range s.watchers[kind] {
		select {
		case w.ch <- ev:
		default: // slow watcher: drop; real drivers resync
		}
	}
}

type alwaysLeader struct{ name string }

// Run implements store.Elector: one term that lasts as long as ctx.
func (a alwaysLeader) Run(ctx context.Context, onStarted func(context.Context), onStopped func()) error {
	if onStarted != nil {
		onStarted(ctx)
	}
	<-ctx.Done()
	if onStopped != nil {
		onStopped()
	}
	return nil
}
func (a alwaysLeader) IsLeader() bool { return true }
func (a alwaysLeader) Leader() string { return a.name }

// Elector implements store.Store: a single process is always the leader.
func (s *Store) Elector(string) store.Elector { return alwaysLeader{name: "self"} }

// Snapshot implements store.Store as a kind: List YAML document.
func (s *Store) Snapshot(_ context.Context, w io.Writer, _ store.SnapshotOptions) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]store.Key, 0, len(s.objects))
	for k := range s.objects {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	items := make([]store.Object, 0, len(keys))
	for _, k := range keys {
		items = append(items, s.objects[k])
	}
	doc := map[string]any{"apiVersion": "v1", "kind": "List", "items": items}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// Restore is not implemented for the memory driver: decoding typed objects
// needs the scheme-aware codec that the real drivers share.
func (s *Store) Restore(context.Context, io.Reader) error {
	return fmt.Errorf("store/memory: Restore not implemented")
}

// Info implements store.Store.
func (s *Store) Info() store.Info {
	return store.Info{Driver: "memory", SchemaVersion: store.SchemaVersion, Leader: "self"}
}

// Close implements store.Store.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for kind, ws := range s.watchers {
		for _, w := range ws {
			close(w.ch)
		}
		delete(s.watchers, kind)
	}
	return nil
}

// sections renders an object as a JSON map so spec and status can be
// handled separately.
func sections(obj store.Object) (map[string]any, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// rebuild decodes a JSON map into a fresh object of like's type.
func rebuild(like store.Object, m map[string]any) (store.Object, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	t := reflect.TypeOf(like)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	out, ok := reflect.New(t).Interface().(store.Object)
	if !ok {
		return nil, fmt.Errorf("store/memory: %s is not a store.Object", t)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return nil, err
	}
	return out, nil
}

func copyInto(src, dst store.Object) error {
	b, err := yaml.Marshal(src)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, dst)
}

func matchLabels(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
