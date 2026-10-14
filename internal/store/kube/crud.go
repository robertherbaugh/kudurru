package kube

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// writeError wraps a failed apiserver write. A refusal for size (413, or
// etcd's "request is too large" passed through by the apiserver) wraps
// store.ErrTooLarge, as the sqlite driver reports past MaxObjectBytes.
func writeError(op string, key store.Key, err error) error {
	if apierrors.IsRequestEntityTooLargeError(err) || strings.Contains(err.Error(), "request is too large") {
		return fmt.Errorf("%w: store/kube: %s %s: %w", store.ErrTooLarge, op, key, err)
	}
	return fmt.Errorf("store/kube: %s %s: %w", op, key, err)
}

// statusFieldManager owns status writes (leader-only callers, §4.3).
const statusFieldManager = "kudurru-controller"

// serverManagedMetadata are metadata fields the apiserver owns or treats as
// preconditions; they are never sent in an apply body (D10: the apiserver
// necessarily overrides uid/resourceVersion/creationTimestamp).
var serverManagedMetadata = []string{
	"uid", "resourceVersion", "generation", "creationTimestamp",
	"deletionTimestamp", "deletionGracePeriodSeconds", "managedFields", "selfLink",
}

// Get implements store.Store with a consistent read from the apiserver.
func (s *Store) Get(ctx context.Context, key store.Key, into store.Object) error {
	if s.isClosed() {
		return store.ErrClosed
	}
	if ki, ok := s.internal.get(key.Kind); ok {
		return s.getInternal(ctx, ki, key, into)
	}
	ki, err := s.kinds.get(key.Kind)
	if err != nil {
		return err
	}
	if got := store.KindOf(into); got != key.Kind {
		return fmt.Errorf("store/kube: Get %s into a %s", key, got)
	}
	if ok, err := s.scopeMatches(ki, key.Namespace); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%w: %s", store.ErrNotFound, key)
	}
	if err := s.c.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: key.Name}, into); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: %s", store.ErrNotFound, key)
		}
		return fmt.Errorf("store/kube: get %s: %w", key, err)
	}
	normalize(ki, into)
	return nil
}

// List implements store.Store with apiserver keyset pagination: Limit and
// Continue map to the list call's limit and continue token. An expired
// token (HTTP 410) is returned as an error; callers restart the listing.
func (s *Store) List(ctx context.Context, kind store.Kind, opts store.ListOptions) (store.ObjectList, error) {
	if s.isClosed() {
		return store.ObjectList{}, store.ErrClosed
	}
	if ki, ok := s.internal.get(kind); ok {
		return s.listInternal(ctx, ki, opts)
	}
	ki, err := s.kinds.get(kind)
	if err != nil {
		return store.ObjectList{}, err
	}
	namespaced, err := s.isNamespaced(ki)
	if err != nil {
		return store.ObjectList{}, err
	}
	if !namespaced && opts.Namespace != "" {
		return store.ObjectList{}, nil // cluster-scoped objects live in no namespace
	}
	var lo []client.ListOption
	if opts.Namespace != "" {
		lo = append(lo, client.InNamespace(opts.Namespace))
	}
	if len(opts.LabelSelector) > 0 {
		lo = append(lo, client.MatchingLabels(opts.LabelSelector))
	}
	if opts.Limit > 0 {
		lo = append(lo, client.Limit(int64(opts.Limit)))
	}
	if opts.Continue != "" {
		lo = append(lo, client.Continue(opts.Continue))
	}
	list := ki.newList().(client.ObjectList)
	if err := s.c.List(ctx, list, lo...); err != nil {
		return store.ObjectList{}, fmt.Errorf("store/kube: list %s: %w", kind, err)
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return store.ObjectList{}, err
	}
	out := store.ObjectList{Items: make([]store.Object, 0, len(items)), Continue: list.GetContinue()}
	for _, it := range items {
		o := it.(store.Object)
		normalize(ki, o)
		out.Items = append(out.Items, o)
	}
	return out, nil
}

// Apply implements store.Store with server-side apply (the Apply contract,
// §4.3, D9):
//
//   - Manager is required and becomes both the SSA field manager and the
//     kudurru.io/managed-by annotation.
//   - An existing object whose annotation names another manager is
//     rejected with *store.ErrManagedBy unless Force. An SSA field-manager
//     conflict (for example fields owned by kubectl) is also reported as
//     *store.ErrManagedBy.
//   - Force is a whole-object takeover: other managers' field ownership is
//     reassigned to Manager (see takeOver), then the object is applied
//     with ForceOwnership, so fields the new manager omits are removed.
//   - ExpectedResourceVersion that does not match (or names an object that
//     does not exist) is store.ErrConflict.
//   - The managedBy check and the write are made atomic by sending the
//     observed resourceVersion as an SSA precondition; if the object changed
//     in between, the check is repeated.
func (s *Store) Apply(ctx context.Context, obj store.Object, opts store.ApplyOptions) (store.Object, error) {
	if opts.Manager == "" {
		return nil, errors.New("store: ApplyOptions.Manager is required")
	}
	if obj == nil {
		return nil, errors.New("store/kube: Apply of a nil object")
	}
	if s.isClosed() {
		return nil, store.ErrClosed
	}
	if ki, ok := s.internal.get(store.KindOf(obj)); ok {
		return s.applyInternal(ctx, ki, obj, opts)
	}
	ki, err := s.kinds.forObject(obj)
	if err != nil {
		return nil, err
	}
	key := store.KeyOf(obj)
	if key.Name == "" {
		return nil, fmt.Errorf("store/kube: Apply %s: metadata.name is required", key.Kind)
	}
	if err := s.checkScope(ki, key); err != nil {
		return nil, err
	}
	applyOpts := []client.ApplyOption{client.FieldOwner(fieldManagerFor(opts.Manager))}
	if opts.Force {
		applyOpts = append(applyOpts, client.ForceOwnership)
	}
	for attempt := 1; ; attempt++ {
		cur, err := s.getMeta(ctx, ki, key)
		if err != nil {
			return nil, err
		}
		precondition := ""
		switch {
		case cur != nil:
			if opts.CreateOnly {
				return nil, fmt.Errorf("%w: %s already exists", store.ErrConflict, key)
			}
			if opts.ExpectedResourceVersion != "" && opts.ExpectedResourceVersion != cur.ResourceVersion {
				return nil, fmt.Errorf("%w: %s is at %s, expected %s", store.ErrConflict, key, cur.ResourceVersion, opts.ExpectedResourceVersion)
			}
			if owner := store.ManagedBy(cur); owner != "" && owner != opts.Manager && !opts.Force {
				return nil, &store.ErrManagedBy{Key: key, Owner: owner}
			}
			precondition = cur.ResourceVersion
			if opts.Force {
				rv, err := s.takeOver(ctx, ki, key, cur, fieldManagerFor(opts.Manager))
				if err != nil {
					if !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
						return nil, fmt.Errorf("store/kube: take over %s: %w", key, err)
					}
					if opts.ExpectedResourceVersion != "" || attempt >= applyRetries {
						return nil, fmt.Errorf("%w: %s changed during apply", store.ErrConflict, key)
					}
					continue
				}
				precondition = rv
			}
		case opts.ExpectedResourceVersion != "":
			return nil, fmt.Errorf("%w: %s does not exist, expected %s", store.ErrConflict, key, opts.ExpectedResourceVersion)
		}
		u, err := applyPayload(ki, obj, opts.Manager, precondition)
		if err != nil {
			return nil, err
		}
		if opts.CreateOnly {
			// SSA can update an object created after the GET. POST is the
			// apiserver's atomic absence precondition, including equal specs.
			if err := s.c.Create(ctx, u, client.FieldOwner(fieldManagerFor(opts.Manager))); err != nil {
				if apierrors.IsAlreadyExists(err) {
					return nil, fmt.Errorf("%w: %s already exists", store.ErrConflict, key)
				}
				return nil, writeError("create", key, err)
			}
			return fromUnstructured(ki, u)
		}
		err = s.c.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), applyOpts...)
		if err == nil {
			return fromUnstructured(ki, u)
		}
		if managers := conflictingManagers(err); len(managers) > 0 {
			return nil, s.managedByError(ctx, ki, key, opts.Manager, managers)
		}
		raced := apierrors.IsConflict(err) || (precondition != "" && apierrors.IsNotFound(err))
		if !raced {
			return nil, writeError("apply", key, err)
		}
		if opts.ExpectedResourceVersion != "" || attempt >= applyRetries {
			return nil, fmt.Errorf("%w: %s changed during apply", store.ErrConflict, key)
		}
	}
}

// Delete implements store.Store. A non-empty Manager must match the
// recorded managedBy unless Force (an empty Manager is unchecked, as in the
// reference driver). The check and the delete are atomic through UID and
// resourceVersion preconditions. Finalizers are honoured by the apiserver:
// an object with finalizers stays visible with a deletionTimestamp until
// they are removed.
func (s *Store) Delete(ctx context.Context, key store.Key, opts store.DeleteOptions) error {
	if s.isClosed() {
		return store.ErrClosed
	}
	if ki, ok := s.internal.get(key.Kind); ok {
		return s.deleteInternal(ctx, ki, key, opts)
	}
	ki, err := s.kinds.get(key.Kind)
	if err != nil {
		return err
	}
	if ok, err := s.scopeMatches(ki, key.Namespace); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%w: %s", store.ErrNotFound, key)
	}
	for attempt := 1; ; attempt++ {
		cur, err := s.getMeta(ctx, ki, key)
		if err != nil {
			return err
		}
		if cur == nil {
			return fmt.Errorf("%w: %s", store.ErrNotFound, key)
		}
		if owner := store.ManagedBy(cur); owner != "" && opts.Manager != "" && owner != opts.Manager && !opts.Force {
			return &store.ErrManagedBy{Key: key, Owner: owner}
		}
		target := ki.newObject()
		target.SetNamespace(key.Namespace)
		target.SetName(key.Name)
		uid, rv := cur.UID, cur.ResourceVersion
		err = s.c.Delete(ctx, target, client.Preconditions{UID: &uid, ResourceVersion: &rv})
		switch {
		case err == nil:
			return nil
		case apierrors.IsNotFound(err):
			return fmt.Errorf("%w: %s", store.ErrNotFound, key)
		case apierrors.IsConflict(err) && attempt < applyRetries:
			continue
		case apierrors.IsConflict(err):
			return fmt.Errorf("%w: %s changed during delete", store.ErrConflict, key)
		default:
			return fmt.Errorf("store/kube: delete %s: %w", key, err)
		}
	}
}

// UpdateStatus implements store.Store through the status subresource, which
// ignores every change outside .status (spec, labels, annotations and
// generation are untouched). If obj carries a resourceVersion it is an
// optimistic-concurrency precondition (mismatch: store.ErrConflict);
// without one the current version is read and the write retried on
// conflict.
func (s *Store) UpdateStatus(ctx context.Context, obj store.Object) error {
	if s.isClosed() {
		return store.ErrClosed
	}
	if ki, ok := s.internal.get(store.KindOf(obj)); ok {
		return s.updateStatusInternal(ctx, ki, obj)
	}
	ki, err := s.kinds.forObject(obj)
	if err != nil {
		return err
	}
	key := store.KeyOf(obj)
	if err := s.checkScope(ki, key); err != nil {
		return err
	}
	next := obj.DeepCopyObject().(store.Object)
	next.GetObjectKind().SetGroupVersionKind(ki.gvk)
	next.SetManagedFields(nil)
	next.SetUID("") // the caller's uid is not a precondition here (D10)
	callerRV := obj.GetResourceVersion()
	for attempt := 1; ; attempt++ {
		if callerRV == "" {
			cur, err := s.getMeta(ctx, ki, key)
			if err != nil {
				return err
			}
			if cur == nil {
				return fmt.Errorf("%w: %s", store.ErrNotFound, key)
			}
			next.SetResourceVersion(cur.ResourceVersion)
		}
		err := s.c.Status().Update(ctx, next, client.FieldOwner(statusFieldManager))
		switch {
		case err == nil:
			return nil
		case apierrors.IsNotFound(err):
			return fmt.Errorf("%w: %s", store.ErrNotFound, key)
		case apierrors.IsConflict(err) && callerRV == "" && attempt < applyRetries:
			continue
		case apierrors.IsConflict(err):
			return fmt.Errorf("%w: %s", store.ErrConflict, key)
		default:
			return writeError("update status", key, err)
		}
	}
}

// takeOver prepares a forced apply that replaces the whole object, as in
// the reference driver. Plain SSA with ForceOwnership only takes the fields
// the new manager sends; fields that only previous owners set would linger
// under their old owners. So every main-resource field owned by any other
// manager (at the kind's apiVersion) is first reassigned to fm with a merge
// patch on managedFields, guarded by the observed resourceVersion; the
// forced apply that follows then removes whatever fm does not specify.
// Status (subresource) ownership is untouched. It returns the
// resourceVersion to use as the apply's precondition.
func (s *Store) takeOver(ctx context.Context, ki *kindInfo, key store.Key, cur *metav1.PartialObjectMetadata, fm string) (string, error) {
	gv := ki.gvk.GroupVersion().String()
	keep := make([]metav1.ManagedFieldsEntry, 0, len(cur.ManagedFields)+1)
	owned := fieldpath.NewSet()
	others := false
	for _, e := range cur.ManagedFields {
		if e.Subresource != "" || e.APIVersion != gv || e.FieldsType != "FieldsV1" || e.FieldsV1 == nil {
			keep = append(keep, e)
			continue
		}
		if e.Manager != fm || e.Operation != metav1.ManagedFieldsOperationApply {
			others = true
		}
		set := fieldpath.NewSet()
		if err := set.FromJSON(bytes.NewReader(e.FieldsV1.GetRawBytes())); err != nil {
			return "", fmt.Errorf("decode managedFields of %q: %w", e.Manager, err)
		}
		owned = owned.Union(set)
	}
	if !others {
		return cur.ResourceVersion, nil
	}
	raw, err := owned.ToJSON()
	if err != nil {
		return "", err
	}
	now := metav1.Now()
	keep = append(keep, metav1.ManagedFieldsEntry{
		Manager: fm, Operation: metav1.ManagedFieldsOperationApply, APIVersion: gv, Time: &now,
		FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(string(raw)),
	})
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"resourceVersion": cur.ResourceVersion, "managedFields": keep,
	}})
	if err != nil {
		return "", err
	}
	target := &metav1.PartialObjectMetadata{}
	target.SetGroupVersionKind(ki.gvk)
	target.SetNamespace(key.Namespace)
	target.SetName(key.Name)
	if err := s.c.Patch(ctx, target, client.RawPatch(types.MergePatchType, patch), client.FieldOwner(fm)); err != nil {
		return "", err
	}
	return target.ResourceVersion, nil
}

// getMeta reads the object's metadata only, returning (nil, nil) when it
// does not exist.
func (s *Store) getMeta(ctx context.Context, ki *kindInfo, key store.Key) (*metav1.PartialObjectMetadata, error) {
	pom := &metav1.PartialObjectMetadata{}
	pom.SetGroupVersionKind(ki.gvk)
	if err := s.c.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: key.Name}, pom); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store/kube: get %s: %w", key, err)
	}
	return pom, nil
}

// managedByError builds the ErrManagedBy for an SSA field-manager conflict.
// The recorded managed-by annotation names the owner when it is someone
// else; otherwise the first conflicting field manager (e.g. "kubectl")
// does.
func (s *Store) managedByError(ctx context.Context, ki *kindInfo, key store.Key, manager string, fieldManagers []string) error {
	owner := fieldManagers[0]
	if cur, err := s.getMeta(ctx, ki, key); err == nil && cur != nil {
		if o := store.ManagedBy(cur); o != "" && o != manager {
			owner = o
		}
	}
	return &store.ErrManagedBy{Key: key, Owner: owner}
}

// scopeMatches reports whether a key's namespace is consistent with the
// kind's scope; a mismatched key names no object (reference-driver parity).
func (s *Store) scopeMatches(ki *kindInfo, namespace string) (bool, error) {
	namespaced, err := s.isNamespaced(ki)
	if err != nil {
		return false, err
	}
	return namespaced == (namespace != ""), nil
}

// checkScope rejects writes whose namespace contradicts the kind's scope.
func (s *Store) checkScope(ki *kindInfo, key store.Key) error {
	ok, err := s.scopeMatches(ki, key.Namespace)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	if key.Namespace == "" {
		return fmt.Errorf("store/kube: %s is namespaced: metadata.namespace is required", key)
	}
	return fmt.Errorf("store/kube: %s is cluster-scoped: metadata.namespace must be empty", key)
}

// applyPayload converts a typed object into the SSA body: the whole object
// minus status and server-managed metadata, with the managed-by annotation
// set and, when non-empty, resourceVersion as a precondition.
func applyPayload(ki *kindInfo, obj store.Object, manager, resourceVersion string) (*unstructured.Unstructured, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, fmt.Errorf("store/kube: encode %s: %w", store.KeyOf(obj), err)
	}
	u := &unstructured.Unstructured{Object: m}
	delete(u.Object, "status")
	md, _ := u.Object["metadata"].(map[string]any)
	if md == nil {
		md = map[string]any{}
	}
	for _, f := range serverManagedMetadata {
		delete(md, f)
	}
	u.Object["metadata"] = md
	u.SetGroupVersionKind(ki.gvk)
	ann := u.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[v1alpha1.ManagedByAnnotation] = manager
	u.SetAnnotations(ann)
	if resourceVersion != "" {
		u.SetResourceVersion(resourceVersion)
	}
	return u, nil
}

// fromUnstructured decodes an apiserver response into a typed object.
func fromUnstructured(ki *kindInfo, u *unstructured.Unstructured) (store.Object, error) {
	out := ki.newObject()
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, out); err != nil {
		return nil, fmt.Errorf("store/kube: decode %s: %w", ki.kind, err)
	}
	normalize(ki, out)
	return out, nil
}

// normalize gives objects returned to callers the same shape as the other
// drivers: TypeMeta set, managedFields dropped.
func normalize(ki *kindInfo, obj store.Object) {
	obj.GetObjectKind().SetGroupVersionKind(ki.gvk)
	obj.SetManagedFields(nil)
}

// fieldManagerFor maps a kudurru manager ("api", "file:<path>", ...) to a
// valid SSA field manager: at most 128 printable characters. Longer or
// non-printable values keep a readable prefix and gain a hash suffix so
// distinct managers stay distinct. The full manager string always lives in
// the managed-by annotation.
func fieldManagerFor(manager string) string {
	valid := len(manager) <= metav1validation.FieldManagerMaxLength
	for _, r := range manager {
		if !unicode.IsPrint(r) {
			valid = false
			break
		}
	}
	if valid {
		return manager
	}
	sum := sha256.Sum256([]byte(manager))
	suffix := "~" + hex.EncodeToString(sum[:8])
	var b strings.Builder
	for _, r := range manager {
		if !unicode.IsPrint(r) {
			r = '_'
		}
		if b.Len()+len(string(r))+len(suffix) > metav1validation.FieldManagerMaxLength {
			break
		}
		b.WriteRune(r)
	}
	return b.String() + suffix
}

// conflictingManagers extracts the field managers named in an SSA conflict
// (HTTP 409 with FieldManagerConflict causes). The apiserver formats each
// cause message as `conflict with "<manager>"`, optionally followed by
// ` with subresource "<name>"` and, for Update-operation managers,
// ` using <apiVersion> at <time>` (k8s.io/apimachinery
// util/managedfields/internal/conflict.go).
func conflictingManagers(err error) []string {
	var st apierrors.APIStatus
	if !errors.As(err, &st) {
		return nil
	}
	status := st.Status()
	if status.Reason != metav1.StatusReasonConflict || status.Details == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, c := range status.Details.Causes {
		if c.Type != metav1.CauseTypeFieldManagerConflict {
			continue
		}
		name := strings.TrimPrefix(c.Message, "conflict with ")
		if q, err := strconv.QuotedPrefix(name); err == nil {
			if uq, err := strconv.Unquote(q); err == nil {
				name = uq
			}
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}
