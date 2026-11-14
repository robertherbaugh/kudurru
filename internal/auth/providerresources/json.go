package providerresources

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"reflect"
	"slices"
	"unicode"
	"unicode/utf8"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
)

func boundedObject(raw []byte, limit int) (map[string]json.RawMessage, error) {
	if len(raw) > limit {
		return nil, ErrBounds
	}
	if !utf8.Valid(raw) {
		return nil, ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	nodes := 0
	if err := scan(d, 0, &nodes); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, ErrInvalid
	}
	var out map[string]json.RawMessage
	if json.Unmarshal(raw, &out) != nil || out == nil {
		return nil, ErrInvalid
	}
	return out, nil
}
func scan(d *json.Decoder, depth int, nodes *int) error {
	*nodes++
	if depth > 16 || *nodes > 8192 {
		return ErrBounds
	}
	token, err := d.Token()
	if err != nil {
		return ErrInvalid
	}
	switch t := token.(type) {
	case json.Delim:
		if t != '{' && t != '[' {
			return ErrInvalid
		}
		seen := map[string]bool{}
		count := 0
		for d.More() {
			count++
			if count > 1024 {
				return ErrBounds
			}
			if t == '{' {
				key, err := d.Token()
				if err != nil {
					return ErrInvalid
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return ErrInvalid
				}
				seen[name] = true
			}
			if err := scan(d, depth+1, nodes); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || t == '{' && end != json.Delim('}') || t == '[' && end != json.Delim(']') {
			return ErrInvalid
		}
	case string:
		if len(t) > MaxDiscoveryBytes {
			return ErrBounds
		}
	}
	return nil
}
func jsonString(fields map[string]json.RawMessage, key string, required bool) (string, error) {
	raw, ok := fields[key]
	if !ok && !required {
		return "", nil
	}
	var value string
	if !ok || json.Unmarshal(raw, &value) != nil || !safe(value, 2048, !required) {
		return "", ErrInvalid
	}
	return value, nil
}
func safe(s string, limit int, empty bool) bool {
	if len(s) > limit || s == "" && !empty || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func hash(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func cloneDescriptor(d servingauth.ProviderDescriptor) servingauth.ProviderDescriptor {
	raw, _ := json.Marshal(d.Spec)
	var spec publicv1.IdentityProviderSpec
	_ = json.Unmarshal(raw, &spec)
	d.Spec = spec
	return d
}
func checkBudget(v reflect.Value, budget *int) error {
	*budget += 32
	if *budget > servingauth.MaxProviderBytes {
		return ErrBounds
	}
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			return checkBudget(v.Elem(), budget)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if err := checkBudget(v.Field(i), budget); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if v.Len() > 64 {
			return ErrBounds
		}
		for i := 0; i < v.Len(); i++ {
			if err := checkBudget(v.Index(i), budget); err != nil {
				return err
			}
		}
	case reflect.String:
		if v.Len() > 4096 {
			return ErrBounds
		}
		*budget += 6 * v.Len()
	}
	if *budget > servingauth.MaxProviderBytes {
		return ErrBounds
	}
	return nil
}
func sortedStrings(in []string) []string { out := slices.Clone(in); slices.Sort(out); return out }

func boundedStrings(raw []byte, limit, maxString int) ([]string, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('[') {
		return nil, ErrInvalid
	}
	out := []string{}
	seen := map[string]bool{}
	for d.More() {
		if len(out) >= limit {
			return nil, ErrBounds
		}
		token, err := d.Token()
		if err != nil {
			return nil, ErrInvalid
		}
		s, ok := token.(string)
		if !ok || !safe(s, maxString, false) || seen[s] {
			return nil, ErrInvalid
		}
		seen[s] = true
		out = append(out, s)
	}
	if token, err = d.Token(); err != nil || token != json.Delim(']') {
		return nil, ErrInvalid
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, ErrInvalid
	}
	return out, nil
}
