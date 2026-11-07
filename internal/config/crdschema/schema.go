// Package crdschema derives singleton admission fields from the settings
// metadata used by the runtime, API, CLI and dashboard.
package crdschema

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/config"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// Settings builds the strict nested shape of Kudurru.spec.settings. Host-only,
// node-scoped and secret keys never become publicly writable schema fields.
func Settings() (apiextensionsv1.JSONSchemaProps, error) {
	root := apiextensionsv1.JSONSchemaProps{Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{}}
	for _, k := range config.Schema {
		if !config.PublicClusterKey(k) {
			continue
		}
		s, err := shape(k.Shape())
		if err != nil {
			return root, fmt.Errorf("%s: %w", k.Path, err)
		}
		s.Description = k.Help
		switch k.Path {
		case "branding.logoLight", "branding.logoDark", "branding.favicon":
			// Only these fields carry bounded image data. Keep the ordinary
			// settings string cap for every other field.
			n := int64(config.BrandingImageDataURLBytes)
			s.MaxLength = &n
		}
		insert(&root, strings.Split(k.Path, "."), s)
	}
	return root, nil
}

func insert(root *apiextensionsv1.JSONSchemaProps, path []string, value apiextensionsv1.JSONSchemaProps) {
	if root.Properties == nil {
		root.Properties = map[string]apiextensionsv1.JSONSchemaProps{}
	}
	if len(path) == 1 {
		root.Properties[path[0]] = value
		return
	}
	child := root.Properties[path[0]]
	child.Type = "object"
	insert(&child, path[1:], value)
	root.Properties[path[0]] = child
}

func shape(s config.Shape) (apiextensionsv1.JSONSchemaProps, error) {
	var out apiextensionsv1.JSONSchemaProps
	switch s.Type {
	case config.TypeString, config.TypeDuration, config.TypeBytes:
		out.Type = "string"
		// String bounds also make future admission CEL costs finite. Settings
		// strings usually carry addresses/patterns; image fields have an exact
		// encoded-size override in Settings.
		n := int64(8192)
		out.MaxLength = &n
	case config.TypeInt:
		out.Type, out.Format = "integer", "int64"
	case config.TypeBool:
		out.Type = "boolean"
	case config.TypeStringList, config.TypeList:
		out.Type = "array"
		n := int64(10000)
		out.MaxItems = &n
		item := s.Items
		if item == nil && s.Type == config.TypeStringList {
			item = &config.Shape{Type: config.TypeString}
		}
		if item == nil {
			return out, fmt.Errorf("list is missing its item shape")
		}
		inner, err := shape(*item)
		if err != nil {
			return out, err
		}
		out.Items = &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &inner}
	case config.TypeMap:
		out.Type = "object"
		n := int64(10000)
		out.MaxProperties = &n
		if s.Values == nil {
			return out, fmt.Errorf("map is missing its value shape")
		}
		inner, err := shape(*s.Values)
		if err != nil {
			return out, err
		}
		out.AdditionalProperties = &apiextensionsv1.JSONSchemaPropsOrBool{Allows: true, Schema: &inner}
	case config.TypeObject:
		out.Type, out.Properties = "object", map[string]apiextensionsv1.JSONSchemaProps{}
		if len(s.Fields) == 0 {
			return out, fmt.Errorf("object is missing its field shapes")
		}
		for _, f := range s.Fields {
			inner, err := shape(f.Shape)
			if err != nil {
				return out, fmt.Errorf("%s: %w", f.Name, err)
			}
			inner.Description = f.Help
			out.Properties[f.Name] = inner
			if f.Required {
				out.Required = append(out.Required, f.Name)
			}
		}
	default:
		return out, fmt.Errorf("unsupported settings shape %q", s.Type)
	}
	if s.Min != nil {
		n := float64(*s.Min)
		out.Minimum = &n
	}
	if s.Max != nil {
		n := float64(*s.Max)
		out.Maximum = &n
	}
	for _, option := range s.Enum {
		raw, err := json.Marshal(option.Value)
		if err != nil {
			return out, err
		}
		out.Enum = append(out.Enum, apiextensionsv1.JSON{Raw: raw})
	}
	return out, nil
}

// Populate replaces only the generic Settings carrier in a generated CRD.
func Populate(crd *apiextensionsv1.CustomResourceDefinition) error {
	if crd.Spec.Names.Kind != "Kudurru" {
		return fmt.Errorf("settings schema: expected Kudurru CRD")
	}
	settings, err := Settings()
	if err != nil {
		return err
	}
	for i := range crd.Spec.Versions {
		version := &crd.Spec.Versions[i]
		if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
			return fmt.Errorf("settings schema: missing structural schema")
		}
		root := version.Schema.OpenAPIV3Schema
		spec, ok := root.Properties["spec"]
		if !ok || spec.Properties == nil {
			return fmt.Errorf("settings schema: missing spec properties")
		}
		spec.Properties["settings"] = *settings.DeepCopy()
		root.Properties["spec"] = spec
	}
	return nil
}
