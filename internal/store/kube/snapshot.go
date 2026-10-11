package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// The local.db snapshot is written by internal/zonecache for every driver
// (D79); the driver's own MaterializedSnapshot/PersistSnapshots from the
// store-kube spike were removed in M2.

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
