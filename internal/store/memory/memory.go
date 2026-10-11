// Package memory is an in-process Store used by tests and as the reference
// implementation of the Apply contract. It is not a production driver.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"
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
	// Optimistic concurrency like the sqlite and kube drivers: a caller
	// that names a resourceVersion writes only on top of it.
	if rv := obj.GetResourceVersion(); rv != "" && rv != prev.GetResourceVersion() {
		return fmt.Errorf("%w: %s is at %s, expected %s", store.ErrConflict, key, prev.GetResourceVersion(), rv)
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

// WatchBuffer is how many live events a watcher queues while its consumer
// is not reading. A watcher that falls further behind is closed, which
// tells the consumer to watch again and re-list (the slow-watcher
// contract of the conformance suite); events are never dropped silently.
const WatchBuffer = 256

// watcher is a store.Watcher. Its run goroutine is the only sender on out
// and closes it on exit.
type watcher struct {
	s        *Store
	kind     store.Kind
	ns       string
	out      chan store.Event
	in       chan store.Event // live events, filled by broadcast
	done     chan struct{}    // closed by Stop or Close
	overflow chan struct{}    // closed when in was full
	exited   chan struct{}    // closed when run returns

	stopOnce, overflowOnce sync.Once
}

func (w *watcher) ResultChan() <-chan store.Event { return w.out }

// Stop implements store.Watcher. It is idempotent and returns once the
// result channel is closed.
func (w *watcher) Stop() {
	w.stopOnce.Do(func() { close(w.done) })
	w.s.mu.Lock()
	w.s.unregister(w)
	w.s.mu.Unlock()
	<-w.exited
}

// run delivers the initial events, a Bookmark, then live events, until the
// watch is stopped or overflows.
func (w *watcher) run(initial []store.Event) {
	defer close(w.exited)
	defer close(w.out)
	send := func(ev store.Event) bool {
		select {
		case w.out <- ev:
			return true
		case <-w.done:
		case <-w.overflow:
		}
		return false
	}
	for _, ev := range initial {
		if !send(ev) {
			return
		}
	}
	if !send(store.Event{Type: store.Bookmark}) {
		return
	}
	for {
		select {
		case ev := <-w.in:
			if !send(ev) {
				return
			}
		case <-w.done:
			return
		case <-w.overflow:
			return
		}
	}
}

// Watch implements store.Store: current objects (filtered by
// opts.Namespace) are delivered as Added from a goroutine, then a Bookmark,
// then live events. The object set is read and the watcher registered
// under one lock, so no write falls between the list and the live stream.
// A consumer that lets more than WatchBuffer live events queue up has its
// channel closed and must watch again. The watch also ends on Stop, when
// ctx is done, or on Close.
func (s *Store) Watch(ctx context.Context, kind store.Kind, opts store.WatchOptions) (store.Watcher, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, store.ErrClosed
	}
	keys := make([]store.Key, 0)
	for k := range s.objects {
		if k.Kind == kind && (opts.Namespace == "" || k.Namespace == opts.Namespace) {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	initial := make([]store.Event, len(keys))
	for i, k := range keys {
		initial[i] = store.Event{Type: store.Added, Object: s.objects[k].DeepCopyObject().(store.Object)}
	}
	w := &watcher{
		s: s, kind: kind, ns: opts.Namespace,
		out:      make(chan store.Event),
		in:       make(chan store.Event, WatchBuffer),
		done:     make(chan struct{}),
		overflow: make(chan struct{}),
		exited:   make(chan struct{}),
	}
	s.watchers[kind] = append(s.watchers[kind], w)
	go w.run(initial)
	go func() {
		select {
		case <-ctx.Done():
			w.Stop()
		case <-w.exited:
		}
	}()
	return w, nil
}

// unregister removes w from the watcher list; s.mu is held.
func (s *Store) unregister(w *watcher) {
	ws := s.watchers[w.kind]
	for i, x := range ws {
		if x == w {
			s.watchers[w.kind] = append(ws[:i], ws[i+1:]...)
			return
		}
	}
}

// broadcast offers ev to every watcher of kind without blocking; s.mu is
// held. A watcher whose buffer is full is unregistered and closed.
func (s *Store) broadcast(kind store.Kind, ev store.Event) {
	for _, w := range slices.Clone(s.watchers[kind]) {
		if w.ns != "" && ev.Object != nil && ev.Object.GetNamespace() != w.ns {
			continue
		}
		select {
		case w.in <- ev:
		default:
			s.unregister(w)
			w.overflowOnce.Do(func() { close(w.overflow) })
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
			w.stopOnce.Do(func() { close(w.done) })
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
