package validator

import (
	"fmt"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime/schema"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

// kindSchema is everything the apiserver derives from one CRD version to
// serve a custom resource: the structural schema (pruning, defaulting, CEL),
// the OpenAPI validators for the main resource and the status subresource,
// and the compiled x-kubernetes-validations programs. It mirrors
// crdHandler.getOrCreateServingInfoFor in k8s.io/apiextensions-apiserver.
type kindSchema struct {
	gvk        schema.GroupVersionKind
	plural     string
	namespaced bool

	structural      *structuralschema.Structural
	schemaValidator apiservervalidation.SchemaValidator
	// statusValidator is nil when the CRD has no status subresource.
	statusValidator apiservervalidation.SchemaValidator
	hasStatus       bool
	// cel is nil when the schema has no x-kubernetes-validations rules.
	cel *cel.Validator
}

// parseCRD decodes a CustomResourceDefinition manifest and builds the
// serving schema of its storage version.
func parseCRD(doc []byte) (*kindSchema, error) {
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(doc, &crd); err != nil {
		return nil, fmt.Errorf("decode CRD: %w", err)
	}
	if crd.Kind != "CustomResourceDefinition" {
		return nil, fmt.Errorf("decode CRD: kind is %q, want CustomResourceDefinition", crd.Kind)
	}
	if crd.Spec.PreserveUnknownFields {
		return nil, fmt.Errorf("CRD %s: preserveUnknownFields is not supported", crd.Name)
	}
	var version *apiextensionsv1.CustomResourceDefinitionVersion
	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Storage {
			version = &crd.Spec.Versions[i]
			break
		}
	}
	if version == nil {
		return nil, fmt.Errorf("CRD %s: no storage version", crd.Name)
	}
	if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
		return nil, fmt.Errorf("CRD %s/%s: no openAPIV3Schema", crd.Name, version.Name)
	}

	internal := &apiextensions.CustomResourceValidation{}
	if err := apiextensionsv1.Convert_v1_CustomResourceValidation_To_apiextensions_CustomResourceValidation(version.Schema, internal, nil); err != nil {
		return nil, fmt.Errorf("CRD %s: convert schema: %w", crd.Name, err)
	}

	ss, err := structuralschema.NewStructural(internal.OpenAPIV3Schema)
	if err != nil {
		return nil, fmt.Errorf("CRD %s: schema is not structural: %w", crd.Name, err)
	}
	if errs := structuralschema.ValidateStructural(nil, ss); len(errs) > 0 {
		return nil, fmt.Errorf("CRD %s: schema is not structural: %w", crd.Name, errs.ToAggregate())
	}
	// Like the apiserver: work on a copy and drop default values that
	// pruning would remove anyway.
	ss = ss.DeepCopy()
	if err := structuraldefaulting.PruneDefaults(ss); err != nil {
		return nil, fmt.Errorf("CRD %s: prune defaults: %w", crd.Name, err)
	}

	sv, _, err := apiservervalidation.NewSchemaValidator(internal.OpenAPIV3Schema)
	if err != nil {
		return nil, fmt.Errorf("CRD %s: build schema validator: %w", crd.Name, err)
	}

	ks := &kindSchema{
		gvk:             schema.GroupVersionKind{Group: crd.Spec.Group, Version: version.Name, Kind: crd.Spec.Names.Kind},
		plural:          crd.Spec.Names.Plural,
		namespaced:      crd.Spec.Scope == apiextensionsv1.NamespaceScoped,
		structural:      ss,
		schemaValidator: sv,
		// CEL programs are compiled once here, as in customresource.NewStrategy.
		cel: cel.NewValidator(ss, true, celconfig.PerCallLimit),
	}

	if version.Subresources != nil && version.Subresources.Status != nil {
		ks.hasStatus = true
		if st, ok := internal.OpenAPIV3Schema.Properties["status"]; ok {
			ks.statusValidator, _, err = apiservervalidation.NewSchemaValidator(&st)
			if err != nil {
				return nil, fmt.Errorf("CRD %s: build status validator: %w", crd.Name, err)
			}
		}
	}
	return ks, nil
}
