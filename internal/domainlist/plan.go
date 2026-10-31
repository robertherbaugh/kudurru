package domainlist

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strconv"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// Objects is the domain editor's object seam. A serving command supplies a
// detached Plan; ordinary controllers can continue supplying their Store.
type Objects interface {
	Get(context.Context, store.Key, store.Object) error
	List(context.Context, store.Kind, store.ListOptions) (store.ObjectList, error)
	Apply(context.Context, store.Object, store.ApplyOptions) (store.Object, error)
	Delete(context.Context, store.Key, store.DeleteOptions) error
}

// Plan edits a bounded detached inventory. It never writes to its source or
// retries a transaction. The caller validates and authorizes Changes before
// staging them in the same transaction that supplied the inventory.
type Plan struct {
	original map[store.Key]store.Object
	current  map[store.Key]store.Object
	changed  map[store.Key]bool
}

// Change is one final object edit; nil Desired deletes Original.
type Change struct {
	Key               store.Key
	Original, Desired store.Object
}

func NewPlan(objects []store.Object) *Plan {
	p := &Plan{original: map[store.Key]store.Object{}, current: map[store.Key]store.Object{}, changed: map[store.Key]bool{}}
	for _, o := range objects {
		key := store.KeyOf(o)
		p.original[key] = o.DeepCopyObject().(store.Object)
		p.current[key] = o.DeepCopyObject().(store.Object)
	}
	return p
}

func (p *Plan) Get(ctx context.Context, key store.Key, into store.Object) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	o := p.current[key]
	if o == nil {
		return store.ErrNotFound
	}
	dst, src := reflect.ValueOf(into), reflect.ValueOf(o.DeepCopyObject())
	if dst.Kind() != reflect.Pointer || dst.IsNil() || dst.Type() != src.Type() {
		return fmt.Errorf("domain plan: incompatible destination for %s", key)
	}
	dst.Elem().Set(src.Elem())
	return nil
}

func (p *Plan) List(ctx context.Context, kind store.Kind, opts store.ListOptions) (store.ObjectList, error) {
	if err := ctx.Err(); err != nil {
		return store.ObjectList{}, err
	}
	var keys []store.Key
	for key, o := range p.current {
		if key.Kind != kind || opts.Namespace != "" && key.Namespace != opts.Namespace {
			continue
		}
		match := true
		for k, v := range opts.LabelSelector {
			if o.GetLabels()[k] != v {
				match = false
			}
		}
		if match {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(a, b store.Key) int {
		if a.Namespace != b.Namespace {
			if a.Namespace < b.Namespace {
				return -1
			}
			return 1
		}
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	start := 0
	if opts.Continue != "" {
		var err error
		start, err = strconv.Atoi(opts.Continue)
		if err != nil || start < 0 || start > len(keys) {
			return store.ObjectList{}, fmt.Errorf("invalid plan cursor")
		}
	}
	end := len(keys)
	if opts.Limit > 0 {
		end = min(end, start+opts.Limit)
	}
	out := store.ObjectList{Items: []store.Object{}}
	for _, key := range keys[start:end] {
		out.Items = append(out.Items, p.current[key].DeepCopyObject().(store.Object))
	}
	if end < len(keys) {
		out.Continue = strconv.Itoa(end)
	}
	return out, nil
}

func (p *Plan) Apply(ctx context.Context, obj store.Object, opts store.ApplyOptions) (store.Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := store.KeyOf(obj)
	old := p.current[key]
	if old != nil {
		if opts.CreateOnly || opts.ExpectedResourceVersion != "" && opts.ExpectedResourceVersion != old.GetResourceVersion() {
			return nil, store.ErrConflict
		}
		if owner := store.ManagedBy(old); owner != "" && owner != opts.Manager && !opts.Force {
			return nil, &store.ErrManagedBy{Key: key, Owner: owner}
		}
	}
	next := obj.DeepCopyObject().(store.Object)
	store.SetManagedBy(next, opts.Manager)
	p.current[key], p.changed[key] = next, true
	return next.DeepCopyObject().(store.Object), nil
}

func (p *Plan) Delete(ctx context.Context, key store.Key, opts store.DeleteOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	old := p.current[key]
	if old == nil {
		return store.ErrNotFound
	}
	if owner := store.ManagedBy(old); owner != "" && owner != opts.Manager && !opts.Force {
		return &store.ErrManagedBy{Key: key, Owner: owner}
	}
	delete(p.current, key)
	p.changed[key] = true
	return nil
}

// Checkpoint lets a batch discard every detached edit of a failed item while
// preserving earlier successful items. No transaction mutation has occurred.
func (p *Plan) Checkpoint() func() {
	current := make(map[store.Key]store.Object, len(p.current))
	changed := make(map[store.Key]bool, len(p.changed))
	for k, v := range p.current {
		current[k] = v
	}
	for k, v := range p.changed {
		changed[k] = v
	}
	return func() { p.current, p.changed = current, changed }
}

func (p *Plan) Changes() []Change {
	out := make([]Change, 0, len(p.changed))
	for key := range p.changed {
		out = append(out, Change{Key: key, Original: p.original[key], Desired: p.current[key]})
	}
	slices.SortFunc(out, func(a, b Change) int {
		if a.Key.String() < b.Key.String() {
			return -1
		}
		if a.Key == b.Key {
			return 0
		}
		return 1
	})
	return out
}
