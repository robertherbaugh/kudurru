package kube

import (
	"fmt"
	"reflect"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// kindInfo describes one kudurru.io kind the driver serves.
type kindInfo struct {
	kind    store.Kind
	gvk     schema.GroupVersionKind
	listGVK schema.GroupVersionKind
	typ     reflect.Type // struct type of the object
	listTyp reflect.Type // struct type of the list
}

// newObject returns an empty typed object with its GroupVersionKind set.
func (k *kindInfo) newObject() store.Object {
	obj := reflect.New(k.typ).Interface().(store.Object)
	obj.GetObjectKind().SetGroupVersionKind(k.gvk)
	return obj
}

// newList returns an empty typed list with its GroupVersionKind set.
func (k *kindInfo) newList() runtime.Object {
	l := reflect.New(k.listTyp).Interface().(runtime.Object)
	l.GetObjectKind().SetGroupVersionKind(k.listGVK)
	return l
}

// registry is the set of kinds served by the driver, derived from the
// kudurru.io scheme so that a new kind registered in api/v1alpha1 is picked
// up by Watch, Snapshot and Restore without touching the driver.
type registry struct {
	byKind map[store.Kind]*kindInfo
	kinds  []store.Kind // sorted, for deterministic iteration
}

// newRegistry collects every kind in the kudurru.io group version of s that
// has a matching <Kind>List type and whose object satisfies store.Object
// and is not itself a list. List-ness is decided by type, not by name:
// "BlockList" is a kind whose list is "BlockListList". The meta types that
// metav1.AddToGroupVersion registers (WatchEvent, ListOptions, ...) have no
// list counterpart and are skipped.
func newRegistry(s *runtime.Scheme) (*registry, error) {
	known := s.KnownTypes(v1alpha1.GroupVersion)
	r := &registry{byKind: map[store.Kind]*kindInfo{}}
	for name, typ := range known {
		listTyp, ok := known[name+"List"]
		if !ok {
			continue
		}
		obj := reflect.New(typ).Interface()
		if _, isList := obj.(metav1.ListInterface); isList {
			continue
		}
		if _, ok := obj.(store.Object); !ok {
			continue
		}
		if _, ok := reflect.New(listTyp).Interface().(metav1.ListInterface); !ok {
			continue
		}
		k := store.Kind(name)
		r.byKind[k] = &kindInfo{
			kind:    k,
			gvk:     v1alpha1.GroupVersion.WithKind(name),
			listGVK: v1alpha1.GroupVersion.WithKind(name + "List"),
			typ:     typ,
			listTyp: listTyp,
		}
		r.kinds = append(r.kinds, k)
	}
	if len(r.kinds) == 0 {
		return nil, fmt.Errorf("store/kube: no %s kinds registered in the scheme", v1alpha1.GroupVersion)
	}
	sort.Slice(r.kinds, func(i, j int) bool { return r.kinds[i] < r.kinds[j] })
	return r, nil
}

// get returns the kind info or an error naming the unknown kind.
func (r *registry) get(k store.Kind) (*kindInfo, error) {
	ki, ok := r.byKind[k]
	if !ok {
		return nil, fmt.Errorf("store/kube: unknown kind %q", k)
	}
	return ki, nil
}

// forObject returns the kind info for a typed object.
func (r *registry) forObject(obj store.Object) (*kindInfo, error) {
	return r.get(store.KindOf(obj))
}
