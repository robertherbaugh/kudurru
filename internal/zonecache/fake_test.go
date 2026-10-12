package zonecache

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// fakeStore is a Watch-only store for the tests that need control the
// memory driver does not give: unbounded watch buffers (the memory driver
// blocks Watch beyond 256 initial objects), held Bookmarks (a store that
// never syncs), forced watch closes (resync) and changes that bypass the
// watchers (events missed while a watch was down). Methods other than
// Watch and Info panic through the nil embedded interface.
type fakeStore struct {
	store.Store

	mu          sync.Mutex
	objs        map[store.Key]store.Object
	rv          uint64
	watchers    map[*fakeWatcher]store.Kind
	hold        chan struct{} // non-nil: Bookmarks wait until it is closed
	failWatches int           // the next n Watch calls fail
	watchCalls  map[store.Kind]int
}

func newFakeStore() *fakeStore {
	return &fakeStore{objs: map[store.Key]store.Object{}, watchers: map[*fakeWatcher]store.Kind{}, watchCalls: map[store.Kind]int{}}
}

func (f *fakeStore) Info() store.Info { return store.Info{Driver: "fake", ClusterID: "fake-cluster"} }

// put stores obj with a new resourceVersion; notify sends the watch event.
func (f *fakeStore) put(obj store.Object, notify bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rv++
	o := obj.DeepCopyObject().(store.Object)
	o.SetResourceVersion(strconv.FormatUint(f.rv, 10))
	key := store.KeyOf(o)
	_, exists := f.objs[key]
	f.objs[key] = o
	if !notify {
		return
	}
	typ := store.Added
	if exists {
		typ = store.Modified
	}
	f.broadcast(key.Kind, store.Event{Type: typ, Object: o})
}

// del removes key; notify sends the watch event.
func (f *fakeStore) del(key store.Key, notify bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objs[key]
	if !ok {
		return
	}
	f.rv++
	delete(f.objs, key)
	if notify {
		f.broadcast(key.Kind, store.Event{Type: store.Deleted, Object: o})
	}
}

func (f *fakeStore) broadcast(kind store.Kind, ev store.Event) {
	for w, k := range f.watchers {
		if k == kind {
			w.push(ev)
		}
	}
}

// holdBookmarks makes every later Watch withhold its Bookmark until
// releaseBookmarks.
func (f *fakeStore) holdBookmarks() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hold = make(chan struct{})
}

func (f *fakeStore) releaseBookmarks() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hold != nil {
		close(f.hold)
		f.hold = nil
	}
}

// breakWatches closes the result channel of every open watch of kind, as a
// driver does when its watch fails.
func (f *fakeStore) breakWatches(kind store.Kind) {
	f.mu.Lock()
	var ws []*fakeWatcher
	for w, k := range f.watchers {
		if k == kind {
			ws = append(ws, w)
		}
	}
	f.mu.Unlock()
	for _, w := range ws {
		w.Stop()
	}
}

func (f *fakeStore) calls(kind store.Kind) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.watchCalls[kind]
}

func (f *fakeStore) Watch(ctx context.Context, kind store.Kind, _ store.WatchOptions) (store.Watcher, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watchCalls[kind]++
	if f.failWatches > 0 {
		f.failWatches--
		return nil, errors.New("fake: apiserver unreachable")
	}
	w := newFakeWatcher()
	for key, o := range f.objs {
		if key.Kind == kind {
			w.push(store.Event{Type: store.Added, Object: o})
		}
	}
	w.untrack = func() {
		f.mu.Lock()
		delete(f.watchers, w)
		f.mu.Unlock()
	}
	f.watchers[w] = kind
	hold := f.hold
	bm := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{ResourceVersion: strconv.FormatUint(f.rv, 10)}}
	go func() {
		if hold != nil {
			select {
			case <-hold:
			case <-w.done:
				return
			}
		}
		w.push(store.Event{Type: store.Bookmark, Object: bm})
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

// fakeWatcher delivers queued events in order through an unbuffered
// channel; push never blocks.
type fakeWatcher struct {
	out     chan store.Event
	notify  chan struct{}
	done    chan struct{}
	untrack func()

	mu    sync.Mutex
	queue []store.Event
	once  sync.Once
}

func newFakeWatcher() *fakeWatcher {
	w := &fakeWatcher{out: make(chan store.Event), notify: make(chan struct{}, 1), done: make(chan struct{})}
	go w.pump()
	return w
}

func (w *fakeWatcher) push(ev store.Event) {
	w.mu.Lock()
	w.queue = append(w.queue, ev)
	w.mu.Unlock()
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

func (w *fakeWatcher) pump() {
	defer close(w.out)
	for {
		w.mu.Lock()
		q := w.queue
		w.queue = nil
		w.mu.Unlock()
		for _, ev := range q {
			select {
			case w.out <- ev:
			case <-w.done:
				return
			}
		}
		select {
		case <-w.notify:
		case <-w.done:
			return
		}
	}
}

func (w *fakeWatcher) ResultChan() <-chan store.Event { return w.out }

func (w *fakeWatcher) Stop() {
	w.once.Do(func() {
		close(w.done)
		if w.untrack != nil {
			w.untrack()
		}
	})
}

// --- shared helpers --------------------------------------------------------

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// startCache starts c and stops it at the end of the test; the returned
// cancel stops it early and waits for Start to return.
func startCache(t testing.TB, c *Cache) (cancel func()) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()
	var once sync.Once
	cancel = func() {
		once.Do(func() {
			stop()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Start: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("Start did not return after cancel")
			}
		})
	}
	t.Cleanup(cancel)
	return cancel
}

// eventually polls cond every 5 ms until it holds or timeout elapses.
func eventually(t testing.TB, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

func waitState(t testing.TB, c *Cache, want State) *Snapshot {
	t.Helper()
	eventually(t, 10*time.Second, "a "+want.String()+" snapshot", func() bool { return c.Snapshot().State == want })
	return c.Snapshot()
}

func zone(name, zoneName string) *v1alpha1.DNSZone {
	return &v1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.DNSZoneSpec{ZoneName: zoneName, Type: v1alpha1.ZoneTypePrimary},
	}
}

func rrset(ns, name, zoneRef string, labels map[string]string) *v1alpha1.DNSRecordSet {
	return &v1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Spec: v1alpha1.DNSRecordSetSpec{
			ZoneRef: v1alpha1.ObjectRef{Name: zoneRef},
			Endpoints: []v1alpha1.Endpoint{
				{DNSName: name + "." + zoneRef + ".example.", RecordType: "A", Targets: []string{"10.0.0.1"}, RecordTTL: 300},
			},
		},
	}
}

func policy(ns, name string, lists ...string) *v1alpha1.Policy {
	return &v1alpha1.Policy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.PolicySpec{BlockListRefs: lists},
	}
}

func clientgroup(ns, name string) *v1alpha1.ClientGroup {
	return &v1alpha1.ClientGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{CIDR: "10.0.10.0/24"}}},
	}
}

func blocklist(ns, name string) *v1alpha1.BlockList {
	return &v1alpha1.BlockList{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{Name: "inline", Inline: []string{"ads.example."}}}},
	}
}

// zoneSummary is the view of the test "zones" builder: record-set names per
// zone, as "<ns>/<name>".
type zoneSummary map[string][]string

func zonesBuilder() Builder {
	return NewBuilder("zones", func(s ObjectSet) (any, error) {
		out := zoneSummary{}
		for _, z := range s.Zones() {
			names := []string{}
			for _, rs := range s.RecordSetsForZone(z.Name) {
				names = append(names, rs.Namespace+"/"+rs.Name)
			}
			out[z.Name] = names
		}
		return out, nil
	})
}
