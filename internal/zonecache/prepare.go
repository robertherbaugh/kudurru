package zonecache

import (
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// PrepareOptions describes one complete, already-decoded object cut. Lists
// must be the owner's immutable exact-digest compilation for THIS cut; Prepare
// does not download or certify list bodies. Generation is a local view counter,
// never a replication generation. Now controls deterministic expiry projections.
type PrepareOptions struct {
	Kinds      []store.Kind
	Lists      *blocking.Snapshot
	Generation uint64
	Now        time.Time
}

// Prepare indexes a defensive copy of a complete object cut and builds every
// requested view. It never watches, publishes, writes status, persists, or
// retains an older builder result. ANY builder error/panic rejects the whole cut.
// Builders own their opaque output and must not mutate the input or retain
// mutable external configuration. Unknown kinds and duplicate keys are errors.
func Prepare(opts PrepareOptions, objects []store.Object, builders []Builder) (*Snapshot, error) {
	kinds := slices.Clone(opts.Kinds)
	if len(kinds) == 0 {
		kinds = DefaultKinds()
	}
	slices.Sort(kinds)
	idx := &index{kinds: kinds, byKind: make(map[store.Kind]*kindIndex), rvs: make(map[store.Kind]string)}
	byKind := make(map[store.Kind][]store.Object, len(kinds))
	for i, k := range kinds {
		if k == "" || (i > 0 && kinds[i-1] == k) {
			return nil, fmt.Errorf("zonecache: invalid prepare kind manifest")
		}
		byKind[k] = nil
	}
	seen := make(map[store.Key]bool, len(objects))
	for _, obj := range objects {
		if prepareNil(obj) {
			return nil, fmt.Errorf("zonecache: nil prepare object")
		}
		key := store.KeyOf(obj)
		if _, ok := byKind[key.Kind]; !ok || key.Name == "" || seen[key] {
			return nil, fmt.Errorf("zonecache: invalid or duplicate prepare object key %s", key)
		}
		seen[key] = true
		copyObj := obj.DeepCopyObject().(store.Object)
		byKind[key.Kind] = append(byKind[key.Kind], copyObj)
		idx.rvs[key.Kind] = rvMax(idx.rvs[key.Kind], copyObj.GetResourceVersion())
	}
	for _, k := range kinds {
		idx.byKind[k] = newKindIndex(k, byKind[k])
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	set := ObjectSet{idx: idx, lists: opts.Lists, now: opts.Now, allChanged: true, listsChanged: true}
	views := make(map[string]any, len(builders))
	for _, builder := range builders {
		if prepareNil(builder) || builder.Name() == "" {
			return nil, fmt.Errorf("zonecache: prepare builder needs a name")
		}
		name := builder.Name()
		if _, ok := views[name]; ok {
			return nil, fmt.Errorf("zonecache: duplicate prepare builder %q", name)
		}
		v, err := safeBuild(builder, set)
		if err != nil {
			return nil, fmt.Errorf("zonecache: prepare builder %q: %w", name, err)
		}
		if prepareNil(v) {
			return nil, fmt.Errorf("zonecache: prepare builder %q returned no view", name)
		}
		views[name] = v
	}
	return &Snapshot{Generation: opts.Generation, State: Live, Objects: set, BuiltAt: opts.Now, views: views}, nil
}

func prepareNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
