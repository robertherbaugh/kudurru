package host

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	validation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
)

// Action schemas use the existing OpenAPI/JSON Schema validator dependency.
// Unsupported extensions/references fail closed rather than fetching schemas.
func validateParams(schema, params json.RawMessage) error {
	if len(schema) == 0 || len(schema) > 64<<10 {
		return ErrUnsupported
	}
	var s extv1.JSONSchemaProps
	d := json.NewDecoder(bytes.NewReader(schema))
	d.DisallowUnknownFields()
	if err := d.Decode(&s); err != nil {
		return ErrUnsupported
	}
	count := 0
	if !boundedSchema(s, 0, &count) {
		return ErrUnsupported
	}
	var internal apiext.JSONSchemaProps
	if err := extv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&s, &internal, nil); err != nil {
		return ErrUnsupported
	}
	v, _, err := validation.NewSchemaValidator(&internal)
	if err != nil {
		return ErrUnsupported
	}
	var value any
	if err := json.Unmarshal(params, &value); err != nil {
		return ErrInvalid
	}
	if len(validation.ValidateCustomResource(nil, value, v)) > 0 {
		return ErrInvalid
	}
	return nil
}
func boundedSchema(s extv1.JSONSchemaProps, depth int, count *int) bool {
	*count++
	if depth > 16 || *count > 1024 || s.Ref != nil || len(s.XValidations) > 0 || len(s.Pattern) > 256 || len(s.Definitions) > 0 || len(s.Dependencies) > 0 {
		return false
	}
	for _, child := range s.Properties {
		if !boundedSchema(child, depth+1, count) {
			return false
		}
	}
	for _, child := range s.PatternProperties {
		if !boundedSchema(child, depth+1, count) {
			return false
		}
	}
	for _, list := range [][]extv1.JSONSchemaProps{s.AllOf, s.AnyOf, s.OneOf} {
		for _, child := range list {
			if !boundedSchema(child, depth+1, count) {
				return false
			}
		}
	}
	for _, child := range []*extv1.JSONSchemaProps{s.Not} {
		if child != nil && !boundedSchema(*child, depth+1, count) {
			return false
		}
	}
	if s.Items != nil {
		if s.Items.Schema != nil && !boundedSchema(*s.Items.Schema, depth+1, count) {
			return false
		}
		for _, child := range s.Items.JSONSchemas {
			if !boundedSchema(child, depth+1, count) {
				return false
			}
		}
	}
	for _, child := range []*extv1.JSONSchemaPropsOrBool{s.AdditionalProperties, s.AdditionalItems} {
		if child != nil && child.Schema != nil && !boundedSchema(*child.Schema, depth+1, count) {
			return false
		}
	}
	return true
}
func containsSecret(raw json.RawMessage, secrets map[string][]byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	for {
		token, err := d.Token()
		if err == io.EOF {
			return false
		}
		if err != nil {
			return true
		}
		s, ok := token.(string)
		if !ok {
			continue
		}
		for _, secret := range secrets {
			if len(secret) > 0 && strings.Contains(s, string(secret)) {
				return true
			}
		}
	}
}
