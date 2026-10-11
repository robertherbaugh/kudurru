package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// MaxObjectBytes is the largest object the API accepts: the 1.5 MiB
// request cap of etcd, so an object that fits the sqlite driver also fits
// a Kubernetes apiserver.
const MaxObjectBytes = 1536 << 10

// Write results, as POST /apply reports them per document.
const (
	ResultCreated   = "created"
	ResultUpdated   = "updated"
	ResultUnchanged = "unchanged"
	ResultError     = "error"
)

// writeRequest is one whole-object write through the API.
type writeRequest struct {
	kind kindInfo
	// body is the decoded document (apiVersion, kind, metadata, spec).
	body map[string]any
	// namespace and name, when set, must match the document (they come
	// from the URL); an empty document field is filled from them.
	namespace, name string
	// ifMatch is the If-Match resourceVersion ("" for none).
	ifMatch string
	// createOnly refuses to replace an existing object (POST create).
	createOnly bool
	// manager is the store manager (default store.ManagerAPI).
	manager string
}

// writeResult is the outcome of a write.
type writeResult struct {
	obj    store.Object
	result string
	key    store.Key
}

// readOnly returns the 503 read_only error in the ha-static shape.
func (s *Server) readOnly() error {
	if s.opts.Objects == nil {
		return nil
	}
	if err := s.opts.Objects.CheckAPIWrite(); err != nil {
		return storeError(err)
	}
	return nil
}

// objectScope is the RBAC scope that guards obj: zone:<name> for zones and
// for record sets (their zoneRef), cluster for everything else.
func objectScope(k kindInfo, obj store.Object) rbac.Scope {
	switch o := obj.(type) {
	case *v1alpha1.DNSZone:
		return rbac.Scope{Kind: rbac.ScopeZone, Name: o.Name}
	case *v1alpha1.DNSRecordSet:
		return rbac.Scope{Kind: rbac.ScopeZone, Name: o.Spec.ZoneRef.Name}
	}
	_ = k
	return clusterScope
}

// keyFor builds the store key of a kind; cluster-scoped kinds drop the
// namespace.
func keyFor(k kindInfo, namespace, name string) store.Key {
	if !k.namespaced {
		namespace = ""
	}
	return store.Key{Kind: k.kind, Namespace: namespace, Name: name}
}

// getObject reads one object of kind k.
func (s *Server) getObject(ctx context.Context, k kindInfo, namespace, name string) (store.Object, error) {
	obj := k.newObject()
	if err := s.opts.Store.Get(ctx, keyFor(k, namespace, name), obj); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errNotFound(keyFor(k, namespace, name).String())
		}
		return nil, storeError(err)
	}
	return obj, nil
}

// objectMap renders a stored object as its Kubernetes-shaped JSON
// document with apiVersion and kind set.
func objectMap(k kindInfo, obj store.Object) (map[string]any, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, storeError(err)
	}
	m["apiVersion"] = v1alpha1.GroupVersion.String()
	m["kind"] = string(k.kind)
	return m, nil
}

// sanitizeDocument drops what a client may not set: status (the status
// subresource is the controller's) and server-owned metadata, including
// the managedBy annotation (the manager comes from the request).
func sanitizeDocument(u *unstructured.Unstructured) {
	delete(u.Object, "status")
	meta, ok := u.Object["metadata"].(map[string]any)
	if !ok {
		return
	}
	for _, f := range []string{"resourceVersion", "uid", "generation", "creationTimestamp", "managedFields", "selfLink", "deletionTimestamp", "deletionGracePeriodSeconds"} {
		delete(meta, f)
	}
	if ann, ok := meta["annotations"].(map[string]any); ok {
		delete(ann, v1alpha1.ManagedByAnnotation)
		if len(ann) == 0 {
			delete(meta, "annotations")
		}
	}
}

// writeObject is the API's whole-object write (decision D70): the
// document is validated unstructured first (strict unknown fields, schema
// defaults), converted to the typed object, validated typed (defaults of
// non-pointer structs, transition rules against the stored object), and
// applied with manager "api". A document identical to the stored object is
// "unchanged" and not written. Objects managed by another writer answer
// 409 managed_by with the owner as hint; first-boot defaults (manager
// "bootstrap") are taken over.
func (s *Server) writeObject(ctx context.Context, req writeRequest) (writeResult, error) {
	if err := s.readOnly(); err != nil {
		return writeResult{}, err
	}
	manager := req.manager
	if manager == "" {
		manager = store.ManagerAPI
	}
	k := req.kind
	if req.body == nil {
		return writeResult{}, errInvalid("body: an api/v1alpha1 object is required")
	}
	u := &unstructured.Unstructured{Object: normalizeNumbers(runtime.DeepCopyJSONValue(toJSONValue(req.body))).(map[string]any)}
	if raw, err := json.Marshal(u.Object); err == nil && len(raw) > MaxObjectBytes {
		return writeResult{}, newError(http.StatusRequestEntityTooLarge, KeyTooLarge,
			fmt.Sprintf("the object is %d bytes; the limit is %d (the etcd request cap)", len(raw), MaxObjectBytes), "split it into several objects")
	}
	if u.GetAPIVersion() == "" {
		u.SetAPIVersion(v1alpha1.GroupVersion.String())
	}
	if u.GetKind() == "" {
		u.SetKind(string(k.kind))
	}
	if u.GetKind() != string(k.kind) {
		return writeResult{}, errInvalid("kind: %q does not match the URL kind %s", u.GetKind(), k.kind)
	}
	if gv := u.GetAPIVersion(); gv != v1alpha1.GroupVersion.String() {
		return writeResult{}, errInvalid("apiVersion: %q is not served (want %s)", gv, v1alpha1.GroupVersion.String())
	}
	if req.name != "" {
		switch u.GetName() {
		case "":
			u.SetName(req.name)
		case req.name:
		default:
			return writeResult{}, errInvalid("metadata.name: %q does not match the URL name %q", u.GetName(), req.name)
		}
	}
	if u.GetName() == "" {
		return writeResult{}, errInvalid("metadata.name: required")
	}
	if k.namespaced {
		ns := req.namespace
		switch {
		case u.GetNamespace() == "" && ns != "":
			u.SetNamespace(ns)
		case u.GetNamespace() == "":
			u.SetNamespace(s.opts.DefaultNamespace)
		case ns != "" && u.GetNamespace() != ns:
			return writeResult{}, errInvalid("metadata.namespace: %q does not match the URL namespace %q", u.GetNamespace(), ns)
		}
	} else if u.GetNamespace() != "" {
		return writeResult{}, errInvalid("metadata.namespace: %s is cluster-scoped and takes no namespace", k.kind)
	}
	sanitizeDocument(u)
	key := keyFor(k, u.GetNamespace(), u.GetName())

	existing := k.newObject()
	if err := s.opts.Store.Get(ctx, key, existing); errors.Is(err, store.ErrNotFound) {
		existing = nil
	} else if err != nil {
		return writeResult{}, storeError(err)
	}
	force := false
	if existing != nil {
		if req.createOnly {
			return writeResult{}, errConflict(key.String()+" already exists", "PUT it to replace it")
		}
		switch owner := store.ManagedBy(existing); owner {
		case "", manager:
		case store.ManagerBootstrap:
			force = true
		default:
			return writeResult{}, errManagedBy(key, owner)
		}
		if req.ifMatch != "" && req.ifMatch != existing.GetResourceVersion() {
			return writeResult{}, errPrecondition(req.ifMatch, existing.GetResourceVersion())
		}
	} else if req.ifMatch != "" {
		return writeResult{}, errPrecondition(req.ifMatch, "")
	}

	var old *unstructured.Unstructured
	if existing != nil {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(existing)
		if err != nil {
			return writeResult{}, storeError(err)
		}
		old = &unstructured.Unstructured{Object: m}
	}
	if err := s.opts.Validator.ValidateUnstructured(ctx, u, old); err != nil {
		return writeResult{}, storeError(err)
	}
	obj := k.newObject()
	if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(u.Object, obj, true); err != nil {
		return writeResult{}, errInvalid("%v", err)
	}
	gvk, _, _ := s.opts.Validator.GroupVersionKind(k.kind)
	obj.GetObjectKind().SetGroupVersionKind(gvk)
	if err := s.opts.Validator.Validate(ctx, obj, existing); err != nil {
		return writeResult{}, storeError(err)
	}
	if existing != nil && !force && store.ManagedBy(existing) == manager && sameContent(existing, obj, manager) {
		return writeResult{obj: existing, result: ResultUnchanged, key: key}, nil
	}
	opts := store.ApplyOptions{Manager: manager, Force: force}
	if existing != nil {
		// Optimistic concurrency between the read above and the write.
		opts.ExpectedResourceVersion = existing.GetResourceVersion()
	}
	written, err := s.opts.Store.Apply(ctx, obj, opts)
	if err != nil {
		return writeResult{}, storeError(err)
	}
	res := ResultUpdated
	if existing == nil {
		res = ResultCreated
	}
	return writeResult{obj: written, result: res, key: key}, nil
}

// deleteObject deletes one object as manager "api" (bootstrap defaults
// may be deleted; objects of other managers answer 409 managed_by).
func (s *Server) deleteObject(ctx context.Context, k kindInfo, existing store.Object, ifMatch string) error {
	if err := s.readOnly(); err != nil {
		return err
	}
	key := store.KeyOf(existing)
	key.Kind = k.kind
	owner := store.ManagedBy(existing)
	if owner != "" && owner != store.ManagerAPI && owner != store.ManagerBootstrap {
		return errManagedBy(key, owner)
	}
	if ifMatch != "" && ifMatch != existing.GetResourceVersion() {
		return errPrecondition(ifMatch, existing.GetResourceVersion())
	}
	err := s.opts.Store.Delete(ctx, key, store.DeleteOptions{Manager: store.ManagerAPI, Force: owner == store.ManagerBootstrap})
	if errors.Is(err, store.ErrNotFound) {
		return errNotFound(key.String())
	}
	return storeError(err)
}

// sameContent reports whether applying desired would change nothing:
// everything but metadata and status is equal, and so are the labels and
// the annotations (with managedBy set to manager).
func sameContent(existing, desired store.Object, manager string) bool {
	em, err1 := runtime.DefaultUnstructuredConverter.ToUnstructured(existing)
	dm, err2 := runtime.DefaultUnstructuredConverter.ToUnstructured(desired)
	if err1 != nil || err2 != nil {
		return false
	}
	skip := map[string]bool{"metadata": true, "status": true, "apiVersion": true, "kind": true}
	keys := map[string]struct{}{}
	for k := range em {
		keys[k] = struct{}{}
	}
	for k := range dm {
		keys[k] = struct{}{}
	}
	for k := range keys {
		if !skip[k] && !reflect.DeepEqual(em[k], dm[k]) {
			return false
		}
	}
	if !equalStringMaps(existing.GetLabels(), desired.GetLabels()) {
		return false
	}
	want := map[string]string{v1alpha1.ManagedByAnnotation: manager}
	for k, v := range desired.GetAnnotations() {
		if k != v1alpha1.ManagedByAnnotation {
			want[k] = v
		}
	}
	return equalStringMaps(existing.GetAnnotations(), want)
}

func equalStringMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// typedObject converts a typed v1alpha1 object into the document map the
// write path takes.
func typedObject(obj store.Object) (map[string]any, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	delete(m, "status")
	if meta, ok := m["metadata"].(map[string]any); ok {
		delete(meta, "creationTimestamp")
	}
	return m, nil
}

// toJSONValue turns a decoded document into plain JSON values
// (map[string]any, []any, string, bool, int64, float64, nil) so that
// runtime.DeepCopyJSONValue accepts it.
func toJSONValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = toJSONValue(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = toJSONValue(x)
		}
		return out
	case []map[string]any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = toJSONValue(x)
		}
		return out
	case []string:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = x
		}
		return out
	case int:
		return int64(t)
	case int32:
		return int64(t)
	case float32:
		return float64(t)
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		f, _ := t.Float64()
		return f
	}
	return v
}
