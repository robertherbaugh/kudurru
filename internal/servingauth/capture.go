package servingauth

import (
	"context"
	"encoding/json"
	"slices"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Cut is a sealed complete auth projection from ONE canonical object/security
// image. Its fields are private; an ordinary DNS snapshot cannot construct it.
type Cut struct {
	base     replication.Generation
	objects  map[store.Key]store.Object
	security Security
}

// String redacts private owner data in diagnostics.
func (*Cut) String() string { return "servingauth cut (private)" }

// GoString redacts private owner data in Go diagnostics.
func (c *Cut) GoString() string { return c.String() }

// Base is the full canonical identity, never a DNS view's local generation.
func (c *Cut) Base() replication.Generation {
	if c == nil {
		return replication.Generation{}
	}
	return c.base
}

// CaptureImage validates the full local manifest BEFORE projecting public IAM,
// private identities, Kudurru and security. No store or watcher is retained.
func CaptureImage(ctx context.Context, image *replication.Image, registry Schema) (*Cut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if image == nil || registry.Scheme == nil || registry.SchemaVersion < 1 {
		return nil, ErrInvalid
	}
	if len(image.Payload.Objects) > MaxObjects || len(image.KindManifest) > 1024 || len(registry.Kinds) > 1024 {
		return nil, ErrBounds
	}
	if len(image.Format) > 128 || len(image.PayloadSHA256) > 64 || len(image.Payload.ClusterSettings.Owner) > 512 || len(image.RequiredFeatures) > 64 || image.Term.Validate() != nil {
		return nil, ErrInvalid
	}
	if image.Parent != nil && (len(image.Parent.Digest) > 64 || image.Parent.Term.Validate() != nil) {
		return nil, ErrInvalid
	}
	for _, feature := range image.RequiredFeatures {
		if len(feature) > 512 {
			return nil, ErrBounds
		}
	}
	for _, kinds := range [][]replication.Kind{image.KindManifest, registry.Kinds} {
		for _, kind := range kinds {
			if len(kind.APIVersion) > 512 || len(kind.Kind) > 512 {
				return nil, ErrBounds
			}
		}
	}
	total := 0
	for _, raw := range image.Payload.Objects {
		if len(raw) > MaxObjectBytes {
			return nil, ErrBounds
		}
		total += len(raw)
		if total > MaxImageBytes {
			return nil, ErrBounds
		}
	}
	for _, section := range replication.SectionNames() {
		rows := replication.Section(&image.Payload, section)
		if len(*rows) > MaxRows {
			return nil, ErrBounds
		}
		for _, row := range *rows {
			if len(row.Data) > MaxRootBytes || len(row.Key) > 512 {
				return nil, ErrBounds
			}
			total += len(row.Data)
			if total > MaxImageBytes {
				return nil, ErrBounds
			}
		}
	}
	kinds := map[string]bool{}
	for _, kind := range registry.Kinds {
		if kinds[kind.Kind] {
			return nil, ErrManifest
		}
		kinds[kind.Kind] = true
	}
	for _, kind := range requiredKinds {
		if !kinds[kind] {
			return nil, ErrManifest
		}
	}
	validated, err := replication.Validate(*image, replication.Validation{SchemaVersion: registry.SchemaVersion, Kinds: slices.Clone(registry.Kinds)})
	if err != nil {
		return nil, ErrManifest
	}
	copyImage := validated.Image()
	security, err := DecodeSecurity(copyImage.Payload.Security)
	if err != nil {
		return nil, err
	}
	cut := &Cut{base: copyImage.Generation(), objects: map[store.Key]store.Object{}, security: security}
	for _, raw := range copyImage.Payload.Objects {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		var header struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
		}
		if json.Unmarshal(raw, &header) != nil {
			return nil, ErrInvalid
		}
		if !slices.Contains(requiredKinds, header.Kind) {
			continue
		}
		gv, err := schema.ParseGroupVersion(header.APIVersion)
		if err != nil {
			return nil, ErrInvalid
		}
		runtimeObj, err := registry.Scheme.New(gv.WithKind(header.Kind))
		if err != nil {
			return nil, ErrManifest
		}
		obj, ok := runtimeObj.(store.Object)
		if !ok || !strict(raw, obj, MaxObjectBytes) {
			return nil, ErrInvalid
		}
		if err = addObject(cut, obj); err != nil {
			return nil, err
		}
	}
	if err = validateSettings(cut); err != nil {
		return nil, err
	}
	return cut, nil
}
func addObject(c *Cut, obj store.Object) error {
	if obj == nil || obj.GetUID() == "" || len(obj.GetUID()) > 512 || obj.GetCreationTimestamp().Time.IsZero() {
		return ErrInvalid
	}
	if validate, ok := obj.(interface{ Validate() error }); ok && validate.Validate() != nil {
		return ErrInvalid
	}
	k := store.KeyOf(obj)
	if _, exists := c.objects[k]; exists {
		return ErrInvalid
	}
	c.objects[k] = obj.DeepCopyObject().(store.Object)
	return nil
}
func validateSettings(c *Cut) error {
	obj, ok := c.objects[store.Key{Kind: "Kudurru", Name: "default"}]
	if !ok {
		return ErrManifest
	}
	if _, ok = obj.(*publicv1.Kudurru); !ok {
		return ErrInvalid
	}
	for k := range c.objects {
		if k.Kind == "Kudurru" && (k.Name != "default" || k.Namespace != "") {
			return ErrInvalid
		}
	}
	return nil
}
func accountObject(c *Cut, a AccountRef) store.Object {
	return c.objects[store.Key{Kind: store.Kind(a.Kind), Namespace: a.Namespace, Name: a.Name}]
}
func accountRef(obj store.Object) AccountRef {
	return AccountRef{Kind: string(store.KindOf(obj)), Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: string(obj.GetUID())}
}
func inactive(obj store.Object) bool {
	if obj == nil || obj.GetDeletionTimestamp() != nil {
		return true
	}
	switch o := obj.(type) {
	case *internalv1.LocalUser:
		return o.Spec.Disabled
	case *publicv1.ServiceAccount:
		return o.Spec.Disabled
	}
	return false
}
