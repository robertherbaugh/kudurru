package sqlite

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
	kind store.Kind
	gvk  schema.GroupVersionKind
	typ  reflect.Type // struct type of the object
}

// newObject returns an empty typed object with its GroupVersionKind set.
func (k *kindInfo) newObject() store.Object {
	obj := reflect.New(k.typ).Interface().(store.Object)
	obj.GetObjectKind().SetGroupVersionKind(k.gvk)
	return obj
}

// registry is the set of kinds served by the driver, derived from the
// kudurru.io group version of the scheme so that a kind registered in
// api/v1alpha1 is served without touching the driver. The derivation rule
// is the kube driver's: a type with a matching <Kind>List type that is a
// store.Object and not itself a list.
type registry struct {
	byKind map[store.Kind]*kindInfo
	kinds  []store.Kind // sorted
}

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
		r.byKind[k] = &kindInfo{kind: k, gvk: v1alpha1.GroupVersion.WithKind(name), typ: typ}
		r.kinds = append(r.kinds, k)
	}
	if len(r.kinds) == 0 {
		return nil, fmt.Errorf("store/sqlite: no %s kinds registered in the scheme", v1alpha1.GroupVersion)
	}
	sort.Slice(r.kinds, func(i, j int) bool { return r.kinds[i] < r.kinds[j] })
	return r, nil
}

// get returns the kind info or an error naming the unknown kind.
func (r *registry) get(k store.Kind) (*kindInfo, error) {
	ki, ok := r.byKind[k]
	if !ok {
		return nil, fmt.Errorf("store/sqlite: unknown kind %q", k)
	}
	return ki, nil
}
