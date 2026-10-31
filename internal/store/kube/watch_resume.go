package kube

import (
	"context"
	"errors"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// resumeWatch preserves the caller's event cursor. A compacted/failed stream
// closes with Err; it never manufactures Added events from a replacement list.
func (s *Store) resumeWatch(ctx context.Context, ki *kindInfo, opts store.WatchOptions, internal bool) (store.Watcher, error) {
	if opts.ResourceVersion == "0" {
		return nil, errors.New("store/kube: resourceVersion 0 is not an exact resume cursor; use empty for an initial list")
	}
	list := ki.newList().(client.ObjectList)
	namespace := opts.Namespace
	lo := []client.ListOption{}
	if internal {
		list = &corev1.SecretList{}
		namespace = s.opts.Namespace
		lo = append(lo, client.MatchingLabels{InternalKindLabel: string(ki.kind)})
	}
	if namespace != "" {
		lo = append(lo, client.InNamespace(namespace))
	}
	lo = append(lo, &client.ListOptions{Raw: &metav1.ListOptions{
		ResourceVersion: opts.ResourceVersion, AllowWatchBookmarks: true,
	}})
	// The child context also owns the initial HTTP call, so Close can interrupt
	// it before a remote watch is returned or registered.
	watchCtx, cancel := context.WithCancel(ctx)
	stopClose := context.AfterFunc(s.bgCtx, cancel)
	remote, err := s.directWatch.Watch(watchCtx, list, lo...)
	if err != nil {
		stopClose()
		cancel()
		return nil, err
	}
	w := &watcher{ch: make(chan store.Event, watchBuffer), done: make(chan struct{}), ki: ki, ns: opts.Namespace}
	w.remove = func() { stopClose(); cancel(); remote.Stop() }
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
		defer w.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case ev, ok := <-remote.ResultChan():
				if !ok {
					return
				}
				if ev.Type == watch.Error {
					w.fail(apierrors.FromObject(ev.Object))
					return
				}
				obj, err := resumeObject(ki, ev, internal)
				if err != nil {
					w.fail(err)
					return
				}
				if obj == nil || (ev.Type != watch.Bookmark && w.ns != "" && obj.GetNamespace() != w.ns) {
					continue
				}
				w.send(store.Event{Type: store.EventType(ev.Type), Object: obj})
			}
		}
	}()
	// A blocked result send must be released by cancellation, independent of the
	// stream pump that is currently doing that send.
	go func() {
		select {
		case <-watchCtx.Done():
			w.Stop()
		case <-w.done:
		}
	}()
	return w, nil
}

func resumeObject(ki *kindInfo, ev watch.Event, internal bool) (store.Object, error) {
	switch ev.Type {
	case watch.Bookmark:
		metadata, err := meta.Accessor(ev.Object)
		if err != nil || metadata.GetResourceVersion() == "" {
			return nil, errors.New("store/kube: resumed watch received an invalid bookmark")
		}
		obj := ki.newObject()
		obj.SetResourceVersion(metadata.GetResourceVersion())
		return obj, nil
	case watch.Added, watch.Modified, watch.Deleted:
	default:
		return nil, errors.New("store/kube: resumed watch received an unsupported event")
	}
	if internal {
		sec, ok := ev.Object.(*corev1.Secret)
		if !ok || sec.Type != InternalSecretType || sec.Labels[InternalKindLabel] != string(ki.kind) {
			return nil, errors.New("store/kube: resumed internal watch received an invalid backing object")
		}
		obj, err := decodeSecret(ki, sec)
		if err != nil || obj.GetResourceVersion() == "" {
			// Decoding errors can contain private JSON fragments. Do not attach them.
			return nil, errors.New("store/kube: resumed internal watch could not decode its backing object")
		}
		return obj, nil
	}
	obj, ok := ev.Object.(store.Object)
	if !ok || obj.GetResourceVersion() == "" || !sameResumeType(obj, ki.newObject()) {
		return nil, errors.New("store/kube: resumed watch received an invalid object")
	}
	cp := obj.DeepCopyObject().(store.Object)
	cp.SetManagedFields(nil)
	normalize(ki, cp)
	return cp, nil
}

func sameResumeType(a, b runtime.Object) bool { return reflect.TypeOf(a) == reflect.TypeOf(b) }
