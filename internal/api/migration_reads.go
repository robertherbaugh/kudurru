package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sort"

	internal "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// MigrationReadSource projects private journals from the original whole lease.
// It never independently samples the live Store or replaces the request cut.
type MigrationReadSource interface {
	MigrationSnapshot(context.Context) ([]*internal.MigrationRun, replication.Generation, error)
}
type migrationRead interface {
	Get(context.Context, store.Key, store.Object) error
	List(context.Context, store.Kind, store.ListOptions) (store.ObjectList, error)
}
type capturedMigrationRead struct {
	base    replication.Generation
	objects map[store.Key]store.Object
}

func (s *Server) migrationReader(ctx context.Context) (migrationRead, error) {
	if !s.currentMigrations() {
		return s.opts.Store, nil
	}
	if s.opts.MigrationReads == nil || s.opts.ObjectReads == nil {
		return nil, errUnavailable("original migration image unavailable")
	}
	journals, base, err := s.opts.MigrationReads.MigrationSnapshot(ctx)
	if err != nil {
		return nil, storeError(err)
	}
	objects, publicBase, err := s.capturedObjects(ctx)
	if err != nil {
		return nil, err
	}
	if !base.Same(publicBase) {
		return nil, errUnavailable("migration image mismatch")
	}
	r := &capturedMigrationRead{base: base, objects: map[store.Key]store.Object{}}
	for _, run := range journals {
		r.objects[store.KeyOf(run)] = run
	}
	for _, kind := range objects.Kinds() {
		for _, o := range objects.List(kind) {
			r.objects[store.KeyOf(o)] = o
		}
	}
	return r, nil
}
func (r *capturedMigrationRead) Get(ctx context.Context, key store.Key, into store.Object) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	o, ok := r.objects[key]
	if !ok {
		return store.ErrNotFound
	}
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}
func (r *capturedMigrationRead) List(ctx context.Context, kind store.Kind, opts store.ListOptions) (store.ObjectList, error) {
	out := store.ObjectList{Items: []store.Object{}}
	after := ""
	if opts.Continue != "" {
		raw, err := base64.RawURLEncoding.DecodeString(opts.Continue)
		if err != nil || len(raw) > 8192 {
			return out, errInvalid("invalid migration cursor")
		}
		var cursor struct {
			Base replication.Generation
			Kind store.Kind
			Name string
		}
		if json.Unmarshal(raw, &cursor) != nil || !cursor.Base.Same(r.base) || cursor.Kind != kind {
			return out, errConflict("Migration history changed", "Restart pagination")
		}
		after = cursor.Name
	}
	var keys []store.Key
	for k := range r.objects {
		if k.Kind == kind && (opts.Namespace == "" || k.Namespace == opts.Namespace) && k.String() > after {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	for i, k := range keys {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		out.Items = append(out.Items, r.objects[k])
		if opts.Limit > 0 && len(out.Items) == opts.Limit {
			if i+1 < len(keys) {
				raw, _ := json.Marshal(struct {
					Base replication.Generation
					Kind store.Kind
					Name string
				}{r.base, kind, k.String()})
				out.Continue = base64.RawURLEncoding.EncodeToString(raw)
			}
			break
		}
	}
	return out, nil
}
func migrationRecordSets(ctx context.Context, r migrationRead) ([]*public.DNSRecordSet, error) {
	var out []*public.DNSRecordSet
	count, total := 0, 0
	seen := map[string]bool{}
	for cursor := ""; ; {
		page, err := r.List(ctx, "DNSRecordSet", store.ListOptions{Limit: 500, Continue: cursor})
		if err != nil {
			return nil, err
		}
		for _, o := range page.Items {
			count++
			raw, err := json.Marshal(o)
			if err != nil {
				return nil, err
			}
			total += len(raw)
			if count > 100000 || total > 64<<20 {
				return nil, errUnavailable("migration record inventory exceeds bounds")
			}
			rs, ok := o.(*public.DNSRecordSet)
			if !ok {
				return nil, errUnavailable("invalid migration record inventory")
			}
			out = append(out, rs)
		}
		if page.Continue == "" {
			return out, nil
		}
		if seen[page.Continue] || len(page.Items) == 0 {
			return nil, errUnavailable("incomplete migration record inventory")
		}
		seen[page.Continue] = true
		cursor = page.Continue
	}
}
