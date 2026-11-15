package kube

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	internalkinds "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func resumedEvent(t *testing.T, w store.Watcher) store.Event {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-w.ResultChan():
			if !ok {
				t.Fatal("resumed stream closed before expected event")
			}
			if ev.Type != store.Bookmark {
				return ev
			}
		case <-timer.C:
			t.Fatal("resumed event deadline exceeded")
		}
	}
}

func TestWatchResumeActualHistory(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := newTestStore(t, Options{})
	ctx := t.Context()
	if _, err := s.Apply(ctx, zone("untouched", "untouched.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	original, err := s.Apply(ctx, zone("history", "history.example."), store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	changed := zone("history", "history.example.")
	changed.Labels = map[string]string{"revision": "second"}
	updated, err := s.Apply(ctx, changed, store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, store.Key{Kind: "DNSZone", Name: "history"}, store.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	// This deleted object no longer exists in the informer. Only server history
	// can produce both exact events, while replay would emit untouched as Added.
	w, err := s.Watch(ctx, "DNSZone", store.WatchOptions{ResourceVersion: original.GetResourceVersion()})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	ev := resumedEvent(t, w)
	if ev.Type != store.Modified || ev.Object.GetName() != "history" || ev.Object.GetResourceVersion() != updated.GetResourceVersion() || ev.Object.GetLabels()["revision"] != "second" || len(ev.Object.GetManagedFields()) != 0 {
		t.Fatalf("expected exact historical Modified, got type=%s name=%s version=%s", ev.Type, ev.Object.GetName(), ev.Object.GetResourceVersion())
	}
	ev = resumedEvent(t, w)
	if ev.Type != store.Deleted || ev.Object.GetName() != "history" || !rvAtLeast(ev.Object.GetResourceVersion(), updated.GetResourceVersion()) {
		t.Fatal("missing historical deletion")
	}
	later, err := s.Apply(ctx, zone("later", "later.example."), store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	ev = resumedEvent(t, w)
	if ev.Type != store.Added || ev.Object.GetResourceVersion() != later.GetResourceVersion() {
		t.Fatal("missing live event following history")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	w.Stop()
	for range w.ResultChan() {
	}
	if err := w.(interface{ Err() error }).Err(); err != nil {
		t.Fatal(err)
	}
}

func TestWatchResumeActualNamespaceAndInternalKinds(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := newTestStore(t, Options{Scheme: internalScheme(t), InternalKinds: internalKindList()})
	ctx := t.Context()
	t.Cleanup(func() {
		_ = shared.admin.DeleteAllOf(context.Background(), &corev1.Secret{}, client.InNamespace(s.opts.Namespace), client.HasLabels{InternalKindLabel})
	})
	for _, internal := range []bool{false, true} {
		t.Run(map[bool]string{false: "CRD", true: "Secret"}[internal], func(t *testing.T) {
			makeObj := func(ns, name string) store.Object {
				if !internal {
					return rrset(ns, name, nil)
				}
				return &internalkinds.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: internalkinds.RoleBindingSpec{Role: "operator", Subjects: []internalkinds.BindingSubject{{Kind: "User", Name: "fixture"}}}}
			}
			kind := store.Kind("DNSRecordSet")
			if internal {
				kind = internalkinds.KindRoleBinding
			}
			original, err := s.Apply(ctx, makeObj("apps", "resume"), store.ApplyOptions{Manager: store.ManagerAPI})
			if err != nil {
				t.Fatal(err)
			}
			// Another namespace (and for private kinds another backing kind) must stay out.
			if _, err := s.Apply(ctx, makeObj("dns", "outside"), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
				t.Fatal(err)
			}
			if internal {
				secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unrelated-resume", Namespace: s.opts.Namespace, Labels: map[string]string{InternalKindLabel: "APIToken"}}, Type: InternalSecretType}
				if err := shared.admin.Create(ctx, secret); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Delete(ctx, store.Key{Kind: kind, Namespace: "apps", Name: "resume"}, store.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
			w, err := s.Watch(ctx, kind, store.WatchOptions{Namespace: "apps", ResourceVersion: original.GetResourceVersion()})
			if err != nil {
				t.Fatal(err)
			}
			defer w.Stop()
			ev := resumedEvent(t, w)
			if ev.Type != store.Deleted || ev.Object.GetNamespace() != "apps" || ev.Object.GetName() != "resume" || store.KindOf(ev.Object) != kind {
				t.Fatal("wrong resumed scope or decoded kind")
			}
			// Close must terminate the underlying stream even when it is idle.
			w.Stop()
			for range w.ResultChan() {
			}
		})
	}
}

type resumeStreamClient struct {
	client.WithWatch
	fn func(context.Context, client.ObjectList, ...client.ListOption) (watch.Interface, error)
}

func (c resumeStreamClient) Watch(ctx context.Context, l client.ObjectList, o ...client.ListOption) (watch.Interface, error) {
	return c.fn(ctx, l, o...)
}

type resumeTestStream struct {
	ch   chan watch.Event
	stop chan struct{}
	once sync.Once
}

func (r *resumeTestStream) ResultChan() <-chan watch.Event { return r.ch }
func (r *resumeTestStream) Stop()                          { r.once.Do(func() { close(r.stop) }) }

func resumeTestStore(t *testing.T, remote *resumeTestStream) (*Store, *kindInfo) {
	t.Helper()
	registry, err := newRegistry(testScheme())
	if err != nil {
		t.Fatal(err)
	}
	ki, err := registry.get("DNSZone")
	if err != nil {
		t.Fatal(err)
	}
	bg, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &Store{bgCtx: bg, watchers: map[*watcher]struct{}{}}
	s.directWatch = resumeStreamClient{fn: func(_ context.Context, _ client.ObjectList, options ...client.ListOption) (watch.Interface, error) {
		opts := (&client.ListOptions{}).ApplyOptions(options)
		if opts.Raw.ResourceVersion != "42" || !opts.Raw.AllowWatchBookmarks {
			t.Error("original resume options lost")
		}
		return remote, nil
	}}
	return s, ki
}

func TestWatchResumeExpiredNeverRelists(t *testing.T) {
	remote := &resumeTestStream{ch: make(chan watch.Event, 1), stop: make(chan struct{})}
	s, ki := resumeTestStore(t, remote)
	remote.ch <- watch.Event{Type: watch.Error, Object: &metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonExpired, Code: 410, Message: "fixture cursor compacted"}}
	w, err := s.resumeWatch(t.Context(), ki, store.WatchOptions{ResourceVersion: "42"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	select {
	case _, ok := <-w.ResultChan():
		if ok {
			t.Fatal("expired stream emitted replacement event")
		}
	case <-time.After(time.Second):
		t.Fatal("expired stream did not close")
	}
	if !apierrors.IsResourceExpired(w.(interface{ Err() error }).Err()) {
		t.Fatal("compaction error lost")
	}
	select {
	case <-remote.stop:
	default:
		t.Fatal("upstream was not stopped")
	}
}

func TestWatchResumeCancellationReleasesBackpressure(t *testing.T) {
	remote := &resumeTestStream{ch: make(chan watch.Event, watchBuffer+1), stop: make(chan struct{})}
	s, ki := resumeTestStore(t, remote)
	for range watchBuffer + 1 {
		obj := zone("bounded", "bounded.example.")
		obj.ResourceVersion = "43"
		remote.ch <- watch.Event{Type: watch.Modified, Object: obj}
	}
	ctx, cancel := context.WithCancel(t.Context())
	w, err := s.resumeWatch(ctx, ki, store.WatchOptions{ResourceVersion: "42"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	deadline := time.Now().Add(time.Second)
	for len(w.(*watcher).ch) < watchBuffer && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(w.(*watcher).ch) != watchBuffer {
		t.Fatal("fixture did not reach bounded backpressure")
	}
	cancel()
	select {
	case <-remote.stop:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not stop blocked stream")
	}
	for range w.ResultChan() {
	}
	if err := w.(interface{ Err() error }).Err(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.watchers) != 0 {
		t.Fatal("closed watcher remains registered")
	}
}

func TestWatchResumeInvalidEventsAndZeroCursor(t *testing.T) {
	remote := &resumeTestStream{ch: make(chan watch.Event, 1), stop: make(chan struct{})}
	s, ki := resumeTestStore(t, remote)
	if _, err := s.resumeWatch(t.Context(), ki, store.WatchOptions{ResourceVersion: "0"}, false); err == nil {
		t.Fatal("zero cursor silently accepted")
	}
	for _, obj := range []runtime.Object{&metav1.Status{}, &corev1.Secret{}, zone("no-version", "example.")} {
		if _, err := resumeObject(ki, watch.Event{Type: watch.Modified, Object: obj}, false); err == nil {
			t.Fatal("malformed resumed object accepted")
		}
	}
	remote.ch <- watch.Event{Type: watch.Modified, Object: &metav1.Status{}}
	w, err := s.resumeWatch(t.Context(), ki, store.WatchOptions{ResourceVersion: "42"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	for range w.ResultChan() {
	}
	if w.(interface{ Err() error }).Err() == nil {
		t.Fatal("invalid event closed without error")
	}
	// Synchronous open failures are returned; they never register a watcher.
	s.directWatch = resumeStreamClient{fn: func(context.Context, client.ObjectList, ...client.ListOption) (watch.Interface, error) {
		return nil, apierrors.NewResourceExpired("fixture")
	}}
	if _, err := s.resumeWatch(t.Context(), ki, store.WatchOptions{ResourceVersion: "42"}, false); !apierrors.IsResourceExpired(err) {
		t.Fatal("open error lost")
	}
	if err := w.(interface{ Err() error }).Err(); errors.Is(err, context.Canceled) {
		t.Fatal("decode failure mislabeled cancellation")
	}
}

func TestWatchResumeCloseInterruptsOpen(t *testing.T) {
	remote := &resumeTestStream{ch: make(chan watch.Event), stop: make(chan struct{})}
	s, ki := resumeTestStore(t, remote)
	bg, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.bgCtx = bg
	started := make(chan struct{})
	s.directWatch = resumeStreamClient{fn: func(ctx context.Context, _ client.ObjectList, _ ...client.ListOption) (watch.Interface, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	done := make(chan error, 1)
	go func() {
		_, err := s.resumeWatch(t.Context(), ki, store.WatchOptions{ResourceVersion: "42"}, false)
		done <- err
	}()
	<-started
	cancel() // Store.Close cancels this same driver-owned context.
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("initial watch did not keep driver cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("driver close did not interrupt watch open")
	}
}
