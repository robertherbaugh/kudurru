// Package iamlegacy converts pre-v0.2 public identity configuration without
// granting host capabilities or touching private credential values.
package iamlegacy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// MaxDocumentBytes bounds one legacy identity document before decoding.
const MaxDocumentBytes = 1 << 20

// Convert preserves object identity, ownership, status and intent. A legacy
// file path becomes a reference only: the host must still grant access. Private
// ServiceAccount authority must be moved before calling this function.
// Errors deliberately omit field values and underlying decoder messages.
func Convert(kind string, raw []byte) (store.Object, error) {
	invalid := errors.New("invalid legacy IAM document")
	if len(raw) > MaxDocumentBytes || !boundedJSON(raw) {
		return nil, invalid
	}
	var document map[string]json.RawMessage
	if json.Unmarshal(raw, &document) != nil {
		return nil, invalid
	}
	var spec map[string]json.RawMessage
	if json.Unmarshal(document["spec"], &spec) != nil || spec == nil {
		return nil, invalid
	}
	var obj store.Object
	switch kind {
	case publicv1.KindServiceAccount:
		obj = &publicv1.ServiceAccount{}
	case publicv1.KindRoleBinding:
		obj = &publicv1.RoleBinding{}
		// Empty legacy scope kind means cluster. Keep that intent while
		// omitting the field under the public schema's enum.
		if value := spec["scope"]; len(value) != 0 {
			var scope map[string]json.RawMessage
			if json.Unmarshal(value, &scope) != nil {
				return nil, invalid
			}
			if bytes.Equal(scope["kind"], []byte(`""`)) {
				delete(scope, "kind")
				spec["scope"], _ = json.Marshal(scope)
			}
		}
	case publicv1.KindIdentityProvider:
		obj = &publicv1.IdentityProvider{}
		var file string
		if field, exists := spec["clientSecretFile"]; exists {
			if json.Unmarshal(field, &file) != nil {
				return nil, invalid
			}
			delete(spec, "clientSecretFile")
		}
		ref := spec["clientSecretRef"]
		if bytes.Equal(ref, []byte("null")) {
			ref = nil
			delete(spec, "clientSecretRef")
		}
		if file != "" {
			if len(ref) != 0 {
				return nil, invalid
			}
			spec["clientSecretRef"], _ = json.Marshal(publicv1.CredentialReference{File: file})
		} else if len(ref) != 0 {
			var fields map[string]json.RawMessage
			if json.Unmarshal(ref, &fields) != nil {
				return nil, invalid
			}
			if fields["name"] != nil || fields["key"] != nil {
				var old publicv1.SecretKeyRef
				if strict(ref, &old) != nil {
					return nil, invalid
				}
				spec["clientSecretRef"], _ = json.Marshal(publicv1.CredentialReference{SecretRef: &publicv1.CredentialSecretRef{Name: old.Name, Key: old.Key}})
			}
		}
	default:
		return nil, invalid
	}
	document["spec"], _ = json.Marshal(spec)
	converted, err := json.Marshal(document)
	if err != nil || strict(converted, obj) != nil {
		return nil, invalid
	}
	gvk := obj.GetObjectKind().GroupVersionKind()
	if gvk.Kind != "" && gvk.Kind != kind || gvk.Version != "" && gvk.GroupVersion() != publicv1.GroupVersion {
		return nil, invalid
	}
	obj.GetObjectKind().SetGroupVersionKind(publicv1.GroupVersion.WithKind(kind))
	if obj.(interface{ Validate() error }).Validate() != nil {
		return nil, invalid
	}
	return obj, nil
}

func strict(raw []byte, into any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(into)
}

func boundedJSON(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var walk func(int) bool
	nodes := 0
	walk = func(depth int) bool {
		nodes++
		if depth > 64 || nodes > 100000 {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		delim, container := token.(json.Delim)
		if !container {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				name, ok := key.(string)
				if err != nil || !ok || seen[name] {
					return false
				}
				seen[name] = true
				if !walk(depth + 1) {
					return false
				}
			}
		case '[':
			for d.More() {
				if !walk(depth + 1) {
					return false
				}
			}
		default:
			return false
		}
		_, err = d.Token()
		return err == nil
	}
	if !walk(0) {
		return false
	}
	_, err := d.Token()
	return errors.Is(err, io.EOF)
}
