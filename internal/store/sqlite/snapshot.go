package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// Snapshot implements store.Store: a `kind: List` YAML document of every
// object, read in one transaction (a consistent cut) and sorted by key. It
// is the backup format and what `kudurru apply -f` and Restore accept.
// opts.Safe omits Options.InternalKinds.
func (s *Store) Snapshot(ctx context.Context, w io.Writer, opts store.SnapshotOptions) (err error) {
	defer s.observe(opSnapshot, time.Now(), &err)
	if s.isClosed() {
		return store.ErrClosed
	}
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("store/sqlite: snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT kind, data FROM objects`)
	if err != nil {
		return fmt.Errorf("store/sqlite: snapshot: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := []store.Object{}
	for rows.Next() {
		var kind, data string
		if err := rows.Scan(&kind, &data); err != nil {
			return fmt.Errorf("store/sqlite: snapshot: %w", err)
		}
		k := store.Kind(kind)
		if opts.Safe && s.internal[k] {
			continue
		}
		ki, err := s.kinds.get(k)
		if err != nil {
			// A kind this binary does not serve (written by a newer one
			// at the same schema version): keep it out of the backup
			// rather than fail the whole snapshot.
			s.log.Warn("snapshot skips an unknown kind", "kind", kind)
			continue
		}
		o := ki.newObject()
		if err := decodeInto(ki, data, o); err != nil {
			return err
		}
		items = append(items, o)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store/sqlite: snapshot: %w", err)
	}
	sort.SliceStable(items, func(i, j int) bool { return store.KeyOf(items[i]).String() < store.KeyOf(items[j]).String() })
	b, err := yaml.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
	if err != nil {
		return fmt.Errorf("store/sqlite: snapshot: %w", err)
	}
	_, err = w.Write(b)
	return err
}

// Restore implements store.Store as apply: every object in r (a `kind:
// List` document, a single object, or a multi-document YAML/JSON stream)
// is applied with its recorded managed-by owner as Manager, or "bootstrap"
// if it has none, exactly as the kube driver does. resourceVersion is
// ignored; uid and creationTimestamp are kept (D10); status is not
// restored (the controller recomputes it). Every object is attempted and
// the errors are joined.
func (s *Store) Restore(ctx context.Context, r io.Reader) (err error) {
	defer s.observe(opRestore, time.Now(), &err)
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
			errs = append(errs, fmt.Errorf("restore %s: %w", store.KeyOf(o), err))
		}
	}
	return errors.Join(errs...)
}

// decodeObjects reads typed kudurru objects from a YAML or JSON stream,
// flattening `kind: List` documents, through the scheme.
func decodeObjects(scheme *runtime.Scheme, r io.Reader) ([]store.Object, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(r, 4096)
	var out []store.Object
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, fmt.Errorf("store/sqlite: decode: %w", err)
		}
		if len(doc) == 0 {
			continue
		}
		u := &unstructured.Unstructured{Object: doc}
		if u.IsList() {
			list, err := u.ToList()
			if err != nil {
				return nil, fmt.Errorf("store/sqlite: decode list: %w", err)
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
		return nil, fmt.Errorf("store/sqlite: %s %s: %w", gvk, u.GetName(), err)
	}
	obj, ok := ro.(store.Object)
	if !ok {
		return nil, fmt.Errorf("store/sqlite: %s is not a kudurru object", gvk)
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, obj); err != nil {
		return nil, fmt.Errorf("store/sqlite: %s %s: %w", gvk, u.GetName(), err)
	}
	obj.GetObjectKind().SetGroupVersionKind(gvk)
	return obj, nil
}
