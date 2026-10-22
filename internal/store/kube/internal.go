package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// Internal kinds (Options.InternalKinds: LocalUser, APIToken, PrincipalState
// from api/internalkinds/v1alpha1) are not
// CRDs (D26: password and token hashes stay out of `kubectl get`). The
// driver keeps each object as one Secret in Options.Namespace:
//
//	metadata.name   kudurru-<kind lower>.<name>             (cluster-scoped kinds)
//	                kudurru-<kind lower>.<namespace>.<name> (namespaced kinds)
//	labels          kudurru.io/kind=<Kind>, kudurru.io/namespace=<namespace>,
//	                app.kubernetes.io/managed-by=kudurru
//	type            kudurru.io/internal-object
//	data.object     the object as JSON (apiVersion, kind, metadata, spec, status)
//
// The object's resourceVersion, uid and creationTimestamp are the Secret's;
// generation and the managed-by annotation live in the JSON and follow the
// same Apply contract as the other drivers (D70): whole-object apply that
// keeps status, generation moves only on spec change, an identical re-apply
// is a no-op, a different manager is *store.ErrManagedBy unless Force, and
// ExpectedResourceVersion is checked against the Secret. Writes carry the
// Secret's resourceVersion as a precondition, so the managed-by check and the
// write are atomic. Objects are validated by their own Validate method; the
// error is a *validator.Error like a CRD schema rejection.
//
// Watch is served by one shared informer over the labelled Secrets of the
// namespace (the kudurru ServiceAccount only needs a namespaced Role on
// secrets, §5.3), started on the first Watch of an internal kind.

// Secret layout constants.
const (
	// InternalKindLabel marks the Secrets that carry internal kinds.
	InternalKindLabel = "kudurru.io/kind"
	// InternalNamespaceLabel records the object's namespace (namespaces are
	// DNS labels, so they fit a label value).
	InternalNamespaceLabel = "kudurru.io/namespace"
	// InternalNameAnnotation records the object's name for humans.
	InternalNameAnnotation = "kudurru.io/name"
	// InternalSecretType is the Secret type of internal objects.
	InternalSecretType corev1.SecretType = "kudurru.io/internal-object" //nolint:gosec // G101: a Secret type, not a credential
	// InternalObjectKey is the data key holding the object JSON.
	InternalObjectKey = "object"
)

// internalKinds is the set of kinds served as Secrets.
type internalKinds struct {
	byKind map[store.Kind]*kindInfo
	kinds  []store.Kind // sorted

	mu       sync.Mutex
	informer toolscache.SharedIndexInformer // lazily started by Watch
}

// splitInternal moves the internal kinds out of the CRD registry: they get
// no CRD informer and are dispatched to the Secret implementation.
func splitInternal(r *registry, kinds []store.Kind) (*internalKinds, error) {
	out := &internalKinds{byKind: map[store.Kind]*kindInfo{}}
	for _, k := range kinds {
		ki, ok := r.byKind[k]
		if !ok {
			return nil, fmt.Errorf("store/kube: internal kind %q is not registered in the scheme", k)
		}
		out.byKind[k] = ki
		out.kinds = append(out.kinds, k)
		delete(r.byKind, k)
		for i, x := range r.kinds {
			if x == k {
				r.kinds = append(r.kinds[:i], r.kinds[i+1:]...)
				break
			}
		}
	}
	sort.Slice(out.kinds, func(i, j int) bool { return out.kinds[i] < out.kinds[j] })
	if len(r.kinds) == 0 {
		return nil, errors.New("store/kube: every registered kind is internal; the scheme needs the kudurru.io CRD kinds")
	}
	return out, nil
}

func (ik *internalKinds) get(k store.Kind) (*kindInfo, bool) {
	if ik == nil {
		return nil, false
	}
	ki, ok := ik.byKind[k]
	return ki, ok
}

// InternalSecretName returns the name of the Secret that holds an internal
// object.
func InternalSecretName(kind store.Kind, namespace, name string) string {
	n := "kudurru-" + strings.ToLower(string(kind)) + "."
	if namespace != "" {
		n += namespace + "."
	}
	return n + name
}

type selfValidating interface{ Validate() error }

// fieldErrorer is implemented by the validation error of the internal kinds
// (internalkinds.ValidationError), matched structurally so the driver does
// not depend on the identity layer's API package.
type fieldErrorer interface {
	error
	FieldCauses() []metav1.StatusCause
}

// validateInternal runs obj's own validation and the Secret-name length
// check, returning a *validator.Error for rejections.
func validateInternal(ki *kindInfo, obj store.Object) error {
	key := store.KeyOf(obj)
	verr := &validator.Error{Group: v1alpha1.GroupVersion.Group, Kind: string(ki.kind), Namespace: key.Namespace, Name: key.Name}
	if v, ok := obj.(selfValidating); ok {
		if err := v.Validate(); err != nil {
			var fe fieldErrorer
			if !errors.As(err, &fe) {
				return err
			}
			for _, c := range fe.FieldCauses() {
				verr.Causes = append(verr.Causes, validator.Cause{Field: c.Field, Type: c.Type, Message: c.Message})
			}
		}
	}
	if key.Name == "" && len(verr.Causes) == 0 {
		verr.Causes = append(verr.Causes, validator.Cause{Field: "metadata.name", Type: metav1.CauseTypeFieldValueRequired, Message: "required"})
	}
	if n := InternalSecretName(ki.kind, key.Namespace, key.Name); key.Name != "" && len(n) > validation.DNS1123SubdomainMaxLength {
		verr.Causes = append(verr.Causes, validator.Cause{Field: "metadata.name", Type: metav1.CauseTypeFieldValueInvalid,
			Message: fmt.Sprintf("too long: the backing Secret name has %d characters, the limit is %d", len(n), validation.DNS1123SubdomainMaxLength)})
	}
	if len(verr.Causes) > 0 {
		return verr
	}
	return nil
}

// decodeSecret returns the object a Secret carries, with the Secret's
// resourceVersion, uid and creationTimestamp.
func decodeSecret(ki *kindInfo, sec *corev1.Secret) (store.Object, error) {
	raw, ok := sec.Data[InternalObjectKey]
	if !ok {
		return nil, fmt.Errorf("store/kube: secret %s/%s has no %q key", sec.Namespace, sec.Name, InternalObjectKey)
	}
	obj := ki.newObject()
	if err := json.Unmarshal(raw, obj); err != nil {
		return nil, fmt.Errorf("store/kube: secret %s/%s: decode %s: %w", sec.Namespace, sec.Name, ki.kind, err)
	}
	obj.SetResourceVersion(sec.ResourceVersion)
	obj.SetUID(sec.UID)
	obj.SetCreationTimestamp(sec.CreationTimestamp)
	normalize(ki, obj)
	return obj, nil
}

// encodeObject renders the stored JSON: no resourceVersion, uid or
// creationTimestamp, which the Secret carries.
func encodeObject(ki *kindInfo, obj store.Object) ([]byte, error) {
	cp := obj.DeepCopyObject().(store.Object)
	normalize(ki, cp)
	cp.SetResourceVersion("")
	cp.SetUID("")
	cp.SetCreationTimestamp(metav1.Time{})
	return json.Marshal(cp)
}

func (s *Store) internalSecret(ki *kindInfo, obj store.Object, data []byte) *corev1.Secret {
	key := store.KeyOf(obj)
	lbl := map[string]string{InternalKindLabel: string(ki.kind), "app.kubernetes.io/managed-by": "kudurru"}
	if key.Namespace != "" {
		lbl[InternalNamespaceLabel] = key.Namespace
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        InternalSecretName(ki.kind, key.Namespace, key.Name),
			Namespace:   s.opts.Namespace,
			Labels:      lbl,
			Annotations: map[string]string{InternalNameAnnotation: key.Name},
		},
		Type: InternalSecretType,
		Data: map[string][]byte{InternalObjectKey: data},
	}
}

// getSecret returns the Secret of key, or nil when it does not exist.
func (s *Store) getSecret(ctx context.Context, ki *kindInfo, key store.Key) (*corev1.Secret, error) {
	var sec corev1.Secret
	err := s.c.Get(ctx, client.ObjectKey{Namespace: s.opts.Namespace, Name: InternalSecretName(ki.kind, key.Namespace, key.Name)}, &sec)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store/kube: get %s: %w", key, err)
	}
	if sec.Labels[InternalKindLabel] != string(ki.kind) || sec.Type != InternalSecretType {
		return nil, nil // a foreign Secret that happens to share the name
	}
	return &sec, nil
}

func (s *Store) getInternal(ctx context.Context, ki *kindInfo, key store.Key, into store.Object) error {
	if got := store.KindOf(into); got != key.Kind {
		return fmt.Errorf("store/kube: Get %s into a %s", key, got)
	}
	sec, err := s.getSecret(ctx, ki, key)
	if err != nil {
		return err
	}
	if sec == nil {
		return fmt.Errorf("%w: %s", store.ErrNotFound, key)
	}
	obj, err := decodeSecret(ki, sec)
	if err != nil {
		return err
	}
	if store.KeyOf(obj) != key {
		return fmt.Errorf("%w: %s", store.ErrNotFound, key)
	}
	reflect.ValueOf(into).Elem().Set(reflect.ValueOf(obj).Elem())
	return nil
}

func (s *Store) listInternal(ctx context.Context, ki *kindInfo, opts store.ListOptions) (store.ObjectList, error) {
	sel := client.MatchingLabels{InternalKindLabel: string(ki.kind)}
	if opts.Namespace != "" {
		sel[InternalNamespaceLabel] = opts.Namespace
	}
	lo := []client.ListOption{client.InNamespace(s.opts.Namespace), sel}
	if opts.Limit > 0 {
		lo = append(lo, client.Limit(int64(opts.Limit)))
	}
	if opts.Continue != "" {
		lo = append(lo, client.Continue(opts.Continue))
	}
	var list corev1.SecretList
	if err := s.c.List(ctx, &list, lo...); err != nil {
		return store.ObjectList{}, fmt.Errorf("store/kube: list %s: %w", ki.kind, err)
	}
	// The object labels live in the JSON, so LabelSelector filters each
	// page after decoding: pages may come back shorter than Limit, and
	// Continue stays valid.
	selector := labels.SelectorFromSet(opts.LabelSelector)
	out := store.ObjectList{Items: make([]store.Object, 0, len(list.Items)), Continue: list.Continue}
	for i := range list.Items {
		if list.Items[i].Type != InternalSecretType {
			continue
		}
		obj, err := decodeSecret(ki, &list.Items[i])
		if err != nil {
			s.log.Warn("skipping undecodable internal object", "secret", list.Items[i].Name, "err", err)
			continue
		}
		if !selector.Matches(labels.Set(obj.GetLabels())) {
			continue
		}
		out.Items = append(out.Items, obj)
	}
	return out, nil
}

// sectionsOf returns obj as a JSON map.
func sectionsOf(obj any) (map[string]any, error) {
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

// withSections decodes m into a fresh object of ki.
func withSections(ki *kindInfo, m map[string]any) (store.Object, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	obj := ki.newObject()
	if err := json.Unmarshal(b, obj); err != nil {
		return nil, err
	}
	return obj, nil
}

func (s *Store) applyInternal(ctx context.Context, ki *kindInfo, obj store.Object, opts store.ApplyOptions) (store.Object, error) {
	if err := validateInternal(ki, obj); err != nil {
		return nil, err
	}
	key := store.KeyOf(obj)
	in, err := sectionsOf(obj)
	if err != nil {
		return nil, err
	}
	for attempt := 1; ; attempt++ {
		sec, err := s.getSecret(ctx, ki, key)
		if err != nil {
			return nil, err
		}
		next := make(map[string]any, len(in))
		for k, v := range in {
			next[k] = v
		}
		var prev store.Object
		var prevMap map[string]any
		if sec == nil {
			if opts.ExpectedResourceVersion != "" {
				return nil, fmt.Errorf("%w: %s does not exist, expected %s", store.ErrConflict, key, opts.ExpectedResourceVersion)
			}
			delete(next, "status") // Apply never writes status (D70)
		} else {
			if opts.CreateOnly {
				return nil, fmt.Errorf("%w: %s already exists", store.ErrConflict, key)
			}
			if prev, err = decodeSecret(ki, sec); err != nil {
				return nil, err
			}
			if opts.ExpectedResourceVersion != "" && opts.ExpectedResourceVersion != sec.ResourceVersion {
				return nil, fmt.Errorf("%w: %s is at %s, expected %s", store.ErrConflict, key, sec.ResourceVersion, opts.ExpectedResourceVersion)
			}
			if owner := store.ManagedBy(prev); owner != "" && owner != opts.Manager && !opts.Force {
				return nil, &store.ErrManagedBy{Key: key, Owner: owner}
			}
			if prevMap, err = sectionsOf(prev); err != nil {
				return nil, err
			}
			if st, ok := prevMap["status"]; ok {
				next["status"] = st
			} else {
				delete(next, "status")
			}
		}
		nobj, err := withSections(ki, next)
		if err != nil {
			return nil, err
		}
		nobj.SetManagedFields(nil)
		store.SetManagedBy(nobj, opts.Manager)
		if prev == nil {
			nobj.SetGeneration(1)
		} else {
			nobj.SetGeneration(prev.GetGeneration())
			if !reflect.DeepEqual(in["spec"], prevMap["spec"]) {
				nobj.SetGeneration(prev.GetGeneration() + 1)
			}
		}
		data, err := encodeObject(ki, nobj)
		if err != nil {
			return nil, err
		}
		if sec == nil {
			created := s.internalSecret(ki, nobj, data)
			err = s.c.Create(ctx, created)
			if err == nil {
				return decodeSecret(ki, created)
			}
			if !apierrors.IsAlreadyExists(err) {
				return nil, writeError("create", key, err)
			}
		} else {
			if old, err := encodeObject(ki, prev); err == nil && bytes.Equal(old, data) {
				return prev, nil // identical re-apply: no write, no event
			}
			updated := s.internalSecret(ki, nobj, data)
			updated.ResourceVersion = sec.ResourceVersion // precondition
			err = s.c.Update(ctx, updated)
			if err == nil {
				return decodeSecret(ki, updated)
			}
			if !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
				return nil, writeError("update", key, err)
			}
		}
		if opts.CreateOnly || opts.ExpectedResourceVersion != "" || attempt >= applyRetries {
			return nil, fmt.Errorf("%w: %s changed during apply", store.ErrConflict, key)
		}
	}
}

func (s *Store) deleteInternal(ctx context.Context, ki *kindInfo, key store.Key, opts store.DeleteOptions) error {
	for attempt := 1; ; attempt++ {
		sec, err := s.getSecret(ctx, ki, key)
		if err != nil {
			return err
		}
		if sec == nil {
			return fmt.Errorf("%w: %s", store.ErrNotFound, key)
		}
		prev, err := decodeSecret(ki, sec)
		if err != nil {
			return err
		}
		if owner := store.ManagedBy(prev); owner != "" && opts.Manager != "" && owner != opts.Manager && !opts.Force {
			return &store.ErrManagedBy{Key: key, Owner: owner}
		}
		err = s.c.Delete(ctx, sec, client.Preconditions{UID: ptr.To(sec.UID), ResourceVersion: ptr.To(sec.ResourceVersion)})
		switch {
		case err == nil:
			return nil
		case apierrors.IsNotFound(err):
			return fmt.Errorf("%w: %s", store.ErrNotFound, key)
		case !apierrors.IsConflict(err):
			return fmt.Errorf("store/kube: delete %s: %w", key, err)
		}
		if attempt >= applyRetries {
			return fmt.Errorf("%w: %s changed during delete", store.ErrConflict, key)
		}
	}
}

func (s *Store) updateStatusInternal(ctx context.Context, ki *kindInfo, obj store.Object) error {
	key := store.KeyOf(obj)
	om, err := sectionsOf(obj)
	if err != nil {
		return err
	}
	for attempt := 1; ; attempt++ {
		sec, err := s.getSecret(ctx, ki, key)
		if err != nil {
			return err
		}
		if sec == nil {
			return fmt.Errorf("%w: %s", store.ErrNotFound, key)
		}
		prev, err := decodeSecret(ki, sec)
		if err != nil {
			return err
		}
		pm, err := sectionsOf(prev)
		if err != nil {
			return err
		}
		if st, ok := om["status"]; ok {
			pm["status"] = st
		} else {
			delete(pm, "status")
		}
		next, err := withSections(ki, pm)
		if err != nil {
			return err
		}
		if err := validateInternal(ki, next); err != nil {
			return err
		}
		data, err := encodeObject(ki, next)
		if err != nil {
			return err
		}
		if old, err := encodeObject(ki, prev); err == nil && bytes.Equal(old, data) {
			obj.SetResourceVersion(sec.ResourceVersion)
			return nil
		}
		updated := s.internalSecret(ki, next, data)
		updated.ResourceVersion = sec.ResourceVersion
		err = s.c.Update(ctx, updated)
		switch {
		case err == nil:
			obj.SetResourceVersion(updated.ResourceVersion)
			return nil
		case apierrors.IsNotFound(err):
			return fmt.Errorf("%w: %s", store.ErrNotFound, key)
		case !apierrors.IsConflict(err):
			return writeError("update status", key, err)
		}
		if attempt >= applyRetries {
			return fmt.Errorf("%w: %s changed during status update", store.ErrConflict, key)
		}
	}
}

// secretInformer returns the shared informer over internal-object Secrets,
// starting it on first use.
func (s *Store) secretInformer() (toolscache.SharedIndexInformer, error) {
	ik := s.internal
	ik.mu.Lock()
	defer ik.mu.Unlock()
	if ik.informer != nil {
		return ik.informer, nil
	}
	lw := toolscache.NewFilteredListWatchFromClient(s.coreREST, "secrets", s.opts.Namespace, func(o *metav1.ListOptions) {
		o.LabelSelector = InternalKindLabel
		o.FieldSelector = fields.OneTermEqualSelector("type", string(InternalSecretType)).String()
	})
	inf := toolscache.NewSharedIndexInformer(lw, &corev1.Secret{}, 0, toolscache.Indexers{})
	if err := inf.SetTransform(func(o any) (any, error) {
		if sec, ok := o.(*corev1.Secret); ok {
			sec.ManagedFields = nil
		}
		return o, nil
	}); err != nil {
		return nil, err
	}
	go inf.Run(s.bgCtx.Done())
	ik.informer = inf
	return inf, nil
}

func (s *Store) watchInternal(ctx context.Context, ki *kindInfo, opts store.WatchOptions) (store.Watcher, error) {
	inf, err := s.secretInformer()
	if err != nil {
		return nil, err
	}
	s.catchUpInternal(ctx, ki, inf)
	w := &watcher{ch: make(chan store.Event, watchBuffer), done: make(chan struct{}), ki: ki, ns: opts.Namespace}
	w.convert = func(obj any) (store.Object, bool) {
		sec, ok := obj.(*corev1.Secret)
		if !ok || sec.Labels[InternalKindLabel] != string(ki.kind) {
			return nil, false
		}
		o, err := decodeSecret(ki, sec)
		if err != nil {
			s.log.Warn("skipping undecodable internal object", "secret", sec.Name, "err", err)
			return nil, false
		}
		return o, true
	}
	reg, err := inf.AddEventHandlerWithOptions(w, toolscache.HandlerOptions{ResyncPeriod: ptr.To(time.Duration(0))})
	if err != nil {
		return nil, err
	}
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
		bm.SetResourceVersion(inf.LastSyncResourceVersion())
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

// catchUpInternal is catchUp for the Secret informer: it waits (best
// effort, catchUpTimeout) until the informer holds every Secret of the kind
// that a consistent list returns now, at its listed resourceVersion or
// newer, and nothing older that the list no longer contains.
func (s *Store) catchUpInternal(ctx context.Context, ki *kindInfo, inf toolscache.SharedIndexInformer) {
	ctx, cancel := context.WithTimeout(ctx, catchUpTimeout)
	defer cancel()
	var list corev1.SecretList
	if err := s.c.List(ctx, &list, client.InNamespace(s.opts.Namespace), client.MatchingLabels{InternalKindLabel: string(ki.kind)}); err != nil {
		s.log.Debug("watch: consistency list failed; starting from the informer as is", "kind", ki.kind, "err", err)
		return
	}
	want := map[string]string{}
	for i := range list.Items {
		if list.Items[i].Type == InternalSecretType {
			want[list.Items[i].Name] = list.Items[i].ResourceVersion
		}
	}
	for {
		if inf.HasSynced() && informerReflects(ki, inf, want, list.ResourceVersion) {
			return
		}
		select {
		case <-ctx.Done():
			s.log.Debug("watch: secret informer did not catch up in time", "kind", ki.kind)
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func informerReflects(ki *kindInfo, inf toolscache.SharedIndexInformer, want map[string]string, listRV string) bool {
	if rvAtLeast(inf.LastSyncResourceVersion(), listRV) {
		return true
	}
	seen := 0
	for _, it := range inf.GetStore().List() {
		sec, ok := it.(*corev1.Secret)
		if !ok || sec.Labels[InternalKindLabel] != string(ki.kind) {
			continue
		}
		if rv, ok := want[sec.Name]; ok {
			if !rvAtLeast(sec.ResourceVersion, rv) {
				return false
			}
			seen++
			continue
		}
		if !rvAtLeast(sec.ResourceVersion, listRV) {
			return false
		}
	}
	return seen == len(want)
}

// snapshotInternal lists every internal object for Snapshot.
func (s *Store) snapshotInternal(ctx context.Context) ([]store.Object, error) {
	if s.internal == nil {
		return nil, nil
	}
	var items []store.Object
	for _, k := range s.internal.kinds {
		ki := s.internal.byKind[k]
		cont := ""
		for {
			page, err := s.listInternal(ctx, ki, store.ListOptions{Limit: snapshotPageSize, Continue: cont})
			if err != nil {
				return nil, err
			}
			items = append(items, page.Items...)
			if cont = page.Continue; cont == "" {
				break
			}
		}
	}
	return items, nil
}
