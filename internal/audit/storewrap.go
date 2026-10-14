package audit

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"

	internalkinds "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// WrapOptions configures WrapStore.
type WrapOptions struct {
	// Node is recorded as the node of every event.
	Node string
	// Scheme builds the empty objects the previous state is read into
	// (the store's scheme: public and internal kudurru.io kinds).
	Scheme *runtime.Scheme
	// Status records UpdateStatus writes as "<kind>.status". Off by
	// default: status is written by the leader's controllers and on every
	// token use (lastUsedAt), and the apiserver audit log covers it.
	Status bool
	// Logger receives the recorder's failures, which never fail a write.
	Logger *slog.Logger
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// WrapStore returns st with every successful Apply, Delete (and, with
// opts.Status, UpdateStatus) recorded to rec as "<kind>.create|update|
// delete|status" with the before and after documents, the way the sqlite
// driver audits its own writes in state.db (§4.3 item 5). It is for
// drivers that keep no audit rows (kube). A write that changes nothing
// (same resourceVersion) is not recorded. Internal kinds (local users,
// tokens, ...) omit documents except for safe token details. A recorder failure is
// logged and never fails the write: the object is already stored.
func WrapStore(st store.Store, rec Recorder, opts WrapOptions) store.Store {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	w := &recordingStore{Store: st, rec: rec, opts: opts}
	if md, ok := st.(store.MultiDeleter); ok {
		return &recordingMultiStore{recordingStore: w, md: md}
	}
	return w
}

type recordingStore struct {
	store.Store
	rec  Recorder
	opts WrapOptions
}

type recordingMultiStore struct {
	*recordingStore
	md store.MultiDeleter
}

// Unwrap returns the wrapped store.
func (w *recordingStore) Unwrap() store.Store { return w.Store }

func (w *recordingStore) Apply(ctx context.Context, obj store.Object, opts store.ApplyOptions) (store.Object, error) {
	key := store.KeyOf(obj)
	prev, found, err := w.current(ctx, key)
	out, aerr := w.Store.Apply(ctx, obj, opts)
	if aerr != nil {
		return out, aerr
	}
	// The kube driver reads through its informer cache, which can lag a
	// write just made: an object the cache does not have yet but whose
	// generation moved past 1 was updated, not created.
	verb := "update"
	switch {
	case err != nil:
		prev = nil // unknown previous state: recorded as an update without a before document
	case !found && out.GetGeneration() <= 1:
		verb = "create"
	case !found:
	case prev.GetResourceVersion() == out.GetResourceVersion():
		return out, nil
	}
	w.record(ctx, key, verb, opts.Manager, prev, out)
	return out, nil
}

func (w *recordingStore) Delete(ctx context.Context, key store.Key, opts store.DeleteOptions) error {
	prev, found, _ := w.current(ctx, key)
	if err := w.Store.Delete(ctx, key, opts); err != nil {
		return err
	}
	if !found {
		prev = nil
	}
	w.record(ctx, key, "delete", opts.Manager, prev, nil)
	return nil
}

func (w *recordingStore) UpdateStatus(ctx context.Context, obj store.Object) error {
	if !w.opts.Status {
		return w.Store.UpdateStatus(ctx, obj)
	}
	key := store.KeyOf(obj)
	prev, found, _ := w.current(ctx, key)
	if err := w.Store.UpdateStatus(ctx, obj); err != nil {
		return err
	}
	if !found {
		prev = nil
	}
	w.record(ctx, key, "status", store.ManagedBy(obj), prev, obj)
	return nil
}

// DeleteMany keeps the wrapped driver's transaction and records every
// request that deleted an object.
func (w *recordingMultiStore) DeleteMany(ctx context.Context, reqs []store.DeleteRequest) error {
	prevs := make([]store.Object, len(reqs))
	for i, r := range reqs {
		if p, found, _ := w.current(ctx, r.Key); found {
			prevs[i] = p
		}
	}
	if err := w.md.DeleteMany(ctx, reqs); err != nil {
		return err
	}
	for i, r := range reqs {
		if prevs[i] != nil {
			w.record(ctx, r.Key, "delete", r.Options.Manager, prevs[i], nil)
		}
	}
	return nil
}

// current reads the stored object of key into a fresh typed object.
func (w *recordingStore) current(ctx context.Context, key store.Key) (store.Object, bool, error) {
	if w.opts.Scheme == nil {
		return nil, false, errors.New("audit: no scheme")
	}
	ro, err := w.opts.Scheme.New(v1alpha1.GroupVersion.WithKind(string(key.Kind)))
	if err != nil {
		return nil, false, err
	}
	obj, ok := ro.(store.Object)
	if !ok {
		return nil, false, errors.New("audit: " + string(key.Kind) + " is not a store object")
	}
	switch err := w.Get(ctx, key, obj); {
	case errors.Is(err, store.ErrNotFound):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	return obj, true, nil
}

func (w *recordingStore) record(ctx context.Context, key store.Key, verb, manager string, before, after store.Object) {
	ev := Event{
		Time:     w.opts.Now(),
		Node:     w.opts.Node,
		Actor:    actorFromContext(ctx, manager),
		Action:   strings.ToLower(string(key.Kind)) + "." + verb,
		Resource: Resource{Kind: string(key.Kind), Namespace: key.Namespace, Name: key.Name},
		Result:   ResultSuccess,
		Manager:  manager,
	}
	if req, ok := ctx.Value(requestContextKey{}).(requestContext); ok {
		ev.Actor, ev.Request = req.actor, req.request
		if req.authz != nil {
			ev.Authz = req.authz()
		}
		// Forwarded auth routes authorize in the identity handler after the API
		// middleware. Their decision is on the write's context, not API state.
		if dec, ok := auth.DecisionFrom(ctx); ok && dec.Allowed {
			ev.Authz = dec.Path
		}
	}
	if after != nil {
		ev.ResourceVersion = after.GetResourceVersion()
	}
	ev.Before, ev.After = document(before), document(after)
	// The write is done: a cancelled request must not lose its record.
	if err := w.rec.Record(context.WithoutCancel(ctx), Sanitize(ev)); err != nil {
		w.opts.Logger.Warn("audit: recording a store write failed", "action", ev.Action, "object", key.String(), "err", err)
	}
}

// actorFromContext is the actor set with store.WithActor ("<kind>:<name>"),
// else the manager of the write (as the sqlite driver records it).
func actorFromContext(ctx context.Context, manager string) Actor {
	a := store.ActorFrom(ctx)
	if a == "" {
		return Actor{Name: manager}
	}
	if kind, name, ok := strings.Cut(a, ":"); ok && kind != "file" && kind != "integration" {
		return Actor{Kind: kind, Name: name}
	}
	return Actor{Name: a}
}

// document is the audit copy of obj: without managedFields, capped like
// Diff.
func document(obj store.Object) json.RawMessage {
	if obj == nil {
		return nil
	}
	if tok, ok := obj.(*internalkinds.APIToken); ok {
		return Diff(tok.AuditDocument())
	}
	if internalkinds.IsInternal(string(store.KeyOf(obj).Kind)) {
		return nil
	}
	cp, ok := obj.DeepCopyObject().(store.Object)
	if !ok {
		return nil
	}
	cp.SetManagedFields(nil)
	return Diff(cp)
}
