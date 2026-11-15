package kube

import (
	"context"
	"errors"
	"fmt"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/rest"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// Direct exposes only uncached public-object CRUD. It owns no informers,
// watches, elections, saved state or background cluster discovery.
type Direct struct{ store *Store }

// NewDirect uses the same authenticated transport, validation and ownership
// checks as New, without starting the server driver's background runtime.
func NewDirect(cfg *rest.Config, opts Options) (*Direct, error) {
	if len(opts.InternalKinds) != 0 {
		return nil, errors.New("direct Kubernetes CRUD does not expose internal kinds")
	}
	if err := opts.setDefaults(); err != nil {
		return nil, err
	}
	mapper, err := directMapper()
	if err != nil {
		return nil, err
	}
	s, err := newStore(cfg, opts, false, mapper)
	if err != nil {
		return nil, err
	}
	return &Direct{store: s}, nil
}

// Get performs a consistent read using the caller's original context.
func (d *Direct) Get(ctx context.Context, key store.Key, into store.Object) error {
	return d.store.Get(ctx, key, into)
}

// List supports the full Kubernetes label-selector grammar on one bounded page.
func (d *Direct) List(ctx context.Context, kind store.Kind, opts store.ListOptions, selector string) (store.ObjectList, error) {
	parsed, err := labels.Parse(selector)
	if err != nil {
		return store.ObjectList{}, err
	}
	return d.store.list(ctx, kind, opts, parsed)
}

// Apply requires original absence/version evidence, preventing conflict retries
// from silently applying a CLI document to a replacement object.
func (d *Direct) Apply(ctx context.Context, obj store.Object, opts store.ApplyOptions) (store.Object, error) {
	if opts.Force || (!opts.CreateOnly && opts.ExpectedResourceVersion == "") {
		return nil, errors.New("direct apply requires original version or absence and forbids force ownership")
	}
	return d.store.Apply(ctx, obj, opts)
}

// Delete retains the driver's original UID/version ownership fence. A conflict
// is returned, never retried against a newly observed object.
func (d *Direct) Delete(ctx context.Context, key store.Key, opts store.DeleteOptions) error {
	if opts.Force {
		return errors.New("direct delete forbids force ownership")
	}
	return d.store.delete(ctx, key, opts, 1)
}

// Close invalidates the facade; there are no background children to join.
func (d *Direct) Close() error { return d.store.Close() }

// The shipped public CRDs fix resource names and scope. Static mapping avoids
// discovery's context-free RESTMapping requests extending an original CLI
// deadline or requiring unrelated API permissions.
func directMapper() (meta.RESTMapper, error) {
	files, err := validator.EmbeddedCRDs()
	if err != nil {
		return nil, err
	}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{v1alpha1.GroupVersion})
	for _, raw := range files {
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			return nil, fmt.Errorf("direct CRD mapping: %w", err)
		}
		if crd.Spec.Group != v1alpha1.Group {
			continue
		}
		scope := meta.RESTScopeNamespace
		if crd.Spec.Scope == apiextensionsv1.ClusterScoped {
			scope = meta.RESTScopeRoot
		}
		for _, version := range crd.Spec.Versions {
			if version.Name != v1alpha1.Version || !version.Served {
				continue
			}
			mapper.AddSpecific(v1alpha1.GroupVersion.WithKind(crd.Spec.Names.Kind), v1alpha1.GroupVersion.WithResource(crd.Spec.Names.Plural), v1alpha1.GroupVersion.WithResource(crd.Spec.Names.Singular), scope)
		}
	}
	return mapper, nil
}
