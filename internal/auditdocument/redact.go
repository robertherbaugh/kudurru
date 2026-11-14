// Package auditdocument prepares credential-free audit documents without
// depending on audit recorders or store drivers.
package auditdocument

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"unicode"

	iam "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
)

var privateKinds = runtimekinds.Private()

// Redact returns a detached audit document. Private carriers are omitted except
// for the token's existing safe allowlist. Invalid documents are never retained.
// This is structural redaction of credential fields, not free-text inspection.
func Redact(kind string, doc json.RawMessage) json.RawMessage {
	if len(doc) == 0 || bytes.Equal(bytes.TrimSpace(doc), []byte("null")) {
		return nil
	}
	if strings.EqualFold(kind, iam.KindAPIToken) {
		var token iam.APIToken
		if json.Unmarshal(doc, &token) != nil {
			return nil
		}
		return token.AuditDocument()
	}
	if slices.ContainsFunc(privateKinds, func(private string) bool { return strings.EqualFold(kind, private) }) {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(doc))
	decoder.UseNumber()
	value, err := decode(decoder, 0)
	if err != nil || decoder.Decode(new(any)) != io.EOF {
		return nil
	}
	// A bare string has no structural field identifying whether it is a secret.
	if _, ok := value.(string); ok {
		return nil
	}
	if !clean(value) {
		return bytes.Clone(doc)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return data
}

func clean(value any) (changed bool) {
	switch v := value.(type) {
	case map[string]any:
		// Historical summaries may contain a raw credential prefix and a hash
		// of the original secret. Never trust a claimed prior redaction marker.
		if _, present := v["truncated"]; present {
			if _, ok := v["prefix"]; ok {
				delete(v, "prefix")
				changed = true
			}
			if _, ok := v["sha256"]; ok {
				delete(v, "sha256")
				changed = true
			}
		}
		for key, child := range v {
			if sensitive(key) {
				delete(v, key)
				changed = true
				continue
			}
			changed = clean(child) || changed
		}
	case []any:
		for _, child := range v {
			changed = clean(child) || changed
		}
	}
	return changed
}

func sensitive(key string) bool {
	name := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, key)
	// References identify granted capabilities; they contain no credential.
	if strings.HasSuffix(name, "secretref") || strings.HasSuffix(name, "secretfile") || name == "secretname" {
		return false
	}
	switch name {
	case "annotations", "managedfields", "authorization", "cookie", "cookies", "setcookie", "dsn", "privatekey", "encryptionkey", "recoverycodes", "totpseed", "totpsecret":
		return true
	}
	return strings.Contains(name, "password") || strings.Contains(name, "secret") ||
		strings.HasSuffix(name, "token") || strings.HasSuffix(name, "apikey")
}

// decode rejects duplicate object keys, including escaped equivalents. Keeping
// unchanged source bytes is safe only if no overwritten field can hide a secret.
// Excessive nesting is omitted conservatively rather than recursing unboundedly.
func decode(d *json.Decoder, depth int) (any, error) {
	if depth > 128 {
		return nil, errors.New("audit document nesting exceeds limit")
	}
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	delim, container := token.(json.Delim)
	if !container {
		return token, nil
	}
	switch delim {
	case '{':
		object := map[string]any{}
		for d.More() {
			keyToken, err := d.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("invalid audit object key")
			}
			if _, exists := object[key]; exists {
				return nil, errors.New("ambiguous audit object key")
			}
			child, err := decode(d, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = child
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return nil, errors.New("invalid audit object")
		}
		return object, nil
	case '[':
		array := []any{}
		for d.More() {
			child, err := decode(d, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, child)
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return nil, errors.New("invalid audit array")
		}
		return array, nil
	default:
		return nil, errors.New("invalid audit document")
	}
}
