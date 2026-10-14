// Package servingbuild composes concrete owners of a complete serving image.
// Preparation never publishes individual owners or reads independent watchers.
package servingbuild

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Schema is the immutable LOCAL registry, including every private owner kind.
// It never derives supported kinds from an untrusted incoming image manifest.
type Schema struct {
	scheme    *runtime.Scheme
	validator *validator.Validator
	kinds     []replication.Kind
	public    map[store.Kind]bool
	entry     func(string, replication.Entry) error
}

// NewSchema checks local registration before any candidate can be prepared.
// The supplied scheme, validator and entry validator must remain immutable.
func NewSchema(scheme *runtime.Scheme, validation *validator.Validator, entry func(string, replication.Entry) error) (*Schema, error) {
	if scheme == nil || validation == nil || entry == nil {
		return nil, fmt.Errorf("servingbuild: complete local validators required")
	}
	s := &Schema{scheme: scheme, validator: validation, public: map[store.Kind]bool{}, entry: entry}
	for _, kind := range validation.Kinds() {
		s.public[kind] = true
	}
	known := scheme.KnownTypes(public.GroupVersion)
	for name, typ := range known {
		list, ok := known[name+"List"]
		if !ok {
			continue
		}
		obj := reflect.New(typ).Interface()
		if _, ok := obj.(store.Object); !ok {
			continue
		}
		if _, ok := obj.(metav1.ListInterface); ok {
			continue
		}
		if _, ok := reflect.New(list).Interface().(metav1.ListInterface); !ok {
			continue
		}
		if !s.public[store.Kind(name)] {
			if _, ok := obj.(interface{ Validate() error }); !ok {
				return nil, fmt.Errorf("servingbuild: private kind has no validator")
			}
		}
		s.kinds = append(s.kinds, replication.Kind{APIVersion: public.GroupVersion.String(), Kind: name})
	}
	slices.SortFunc(s.kinds, func(a, b replication.Kind) int { return compare(a.Kind, b.Kind) })
	for _, required := range []string{"DNSZone", "DNSRecordSet", "BlockList", "ClientGroup", "Policy", "Kudurru", "ServiceAccount", "RoleBinding", "IdentityProvider", "LocalUser", "APIToken", "PrincipalState", "ZoneState", "ZoneChunk", "ZoneLifecycle"} {
		if !slices.ContainsFunc(s.kinds, func(k replication.Kind) bool { return k.Kind == required }) {
			return nil, fmt.Errorf("servingbuild: missing local kind %s", required)
		}
	}
	return s, nil
}

func compare(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func (s *Schema) validation(ctx context.Context) replication.Validation {
	return replication.Validation{SchemaVersion: store.SchemaVersion, Kinds: slices.Clone(s.kinds), ValidateEntry: s.entry, ValidateObject: func(raw json.RawMessage) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		object, err := s.decode(raw)
		if err != nil {
			return err
		}
		created := object.GetCreationTimestamp()
		if object.GetUID() == "" || object.GetGeneration() < 1 || created.IsZero() {
			return fmt.Errorf("servingbuild: source metadata missing")
		}
		if !s.public[store.KindOf(object)] {
			return object.(interface{ Validate() error }).Validate()
		}
		if err := s.validator.Validate(ctx, object.DeepCopyObject().(store.Object), nil); err != nil {
			return err
		}
		if s.validator.HasStatusSubresource(store.KindOf(object)) {
			return s.validator.ValidateStatus(ctx, object.DeepCopyObject().(store.Object), object.DeepCopyObject().(store.Object))
		}
		return nil
	}}
}

func (s *Schema) auth() servingauth.Schema {
	return servingauth.Schema{Scheme: s.scheme, Kinds: slices.Clone(s.kinds), SchemaVersion: store.SchemaVersion}
}

func (s *Schema) decode(raw json.RawMessage) (store.Object, error) {
	var meta struct{ APIVersion, Kind string }
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, replication.ErrInvalid
	}
	gv, err := schema.ParseGroupVersion(meta.APIVersion)
	if err != nil || gv != public.GroupVersion {
		return nil, replication.ErrIncompatible
	}
	obj, err := s.scheme.New(gv.WithKind(meta.Kind))
	if err != nil {
		return nil, replication.ErrIncompatible
	}
	value, ok := obj.(store.Object)
	if !ok {
		return nil, replication.ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return nil, replication.ErrInvalid
	}
	return value, nil
}
