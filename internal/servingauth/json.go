package servingauth

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"sync"
	"unicode/utf8"
)

// Hard caps are checked before typed JSON allocations and compiler maps.
const (
	MaxRows           = 4096
	MaxRowBytes       = 64 << 10
	MaxRootBytes      = 512 << 10
	MaxSecurityBytes  = 4 << 20
	MaxObjects        = 100000
	MaxObjectBytes    = 1536 << 10
	MaxImageBytes     = 256 << 20
	MaxGrants         = 128
	FormatVersion     = 1
	RootFormatVersion = 5
	RootKey           = "auth/v1/root"
	ownerPrefix       = "auth/"
)

func hash(v any) string {
	b, _ := json.Marshal(v)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
func strict(raw []byte, into any, capBytes int) bool {
	if !boundedJSON(raw, capBytes) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(into) == nil
}

// Owner field names are exact. encoding/json alone accepts case-insensitive
// aliases, allowing two differently cased keys to overwrite the same field.
// The bound and the exact names (ownerFields) are checked in one walk before
// the strict decode.
func strictOwner(raw []byte, into any, capBytes int) bool {
	if !boundedOwnerJSON(raw, capBytes, reflect.TypeOf(into).Elem()) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(into) == nil
}

// ownerFields reports whether raw is a JSON object whose member names are all
// exact owner field names, with AccountRef and ProviderRef members that are
// such objects in turn. It is the definition boundedOwnerJSON implements in
// its bounding walk.
func ownerFields(raw []byte, typ reflect.Type) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return false
	}
	for name, value := range fields {
		found := false
		for i := range typ.NumField() {
			field := typ.Field(i)
			if strings.Split(field.Tag.Get("json"), ",")[0] != name {
				continue
			}
			found = true
			if (field.Type == reflect.TypeFor[AccountRef]() || field.Type == reflect.TypeFor[ProviderRef]()) && !ownerFields(value, field.Type) {
				return false
			}
			break
		}
		if !found {
			return false
		}
	}
	return true
}

// ownerNamesByType caches ownerNames per owner type.
var ownerNamesByType sync.Map // reflect.Type -> map[string]reflect.Type

// ownerNames maps each JSON member name of an owner type to what ownerFields
// finds for it: the first field with that name, and for an AccountRef or
// ProviderRef field the type whose names its value must use (nil otherwise).
func ownerNames(typ reflect.Type) map[string]reflect.Type {
	if names, ok := ownerNamesByType.Load(typ); ok {
		return names.(map[string]reflect.Type)
	}
	names := map[string]reflect.Type{}
	for i := range typ.NumField() {
		field := typ.Field(i)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if _, ok := names[name]; ok {
			continue
		}
		var nested reflect.Type
		if field.Type == reflect.TypeFor[AccountRef]() || field.Type == reflect.TypeFor[ProviderRef]() {
			nested = field.Type
		}
		names[name] = nested
	}
	ownerNamesByType.Store(typ, names)
	return names
}

// boundedJSON bounds raw before any typed decode: one valid UTF-8 JSON value
// of at most 32 levels and 50,000 nodes, at most MaxRows items in an array,
// and no repeated member name in an object.
func boundedJSON(raw []byte, capBytes int) bool {
	return boundedOwnerJSON(raw, capBytes, nil)
}

// boundedOwnerJSON is boundedJSON and, for a non-nil owner type,
// ownerFields(raw, owner), in one walk.
func boundedOwnerJSON(raw []byte, capBytes int, owner reflect.Type) bool {
	if len(raw) == 0 || len(raw) > capBytes || !utf8.Valid(raw) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	nodes := 0
	// A non-nil typ requires the value to be an object whose member names
	// are typ's exact owner names (ownerFields).
	var walk func(int, reflect.Type) bool
	walk = func(depth int, typ reflect.Type) bool {
		nodes++
		if depth > 32 || nodes > 50000 {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return typ == nil
		}
		if typ != nil && delim != '{' {
			return false
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				tok, e := d.Token()
				key, ok := tok.(string)
				if e != nil || !ok || seen[key] {
					return false
				}
				seen[key] = true
				var nested reflect.Type
				if typ != nil {
					var known bool
					if nested, known = ownerNames(typ)[key]; !known {
						return false
					}
				}
				if !walk(depth+1, nested) {
					return false
				}
			}
		case '[':
			count := 0
			for d.More() {
				count++
				if count > MaxRows || !walk(depth+1, nil) {
					return false
				}
			}
		default:
			return false
		}
		_, err = d.Token()
		return err == nil
	}
	if !walk(0, owner) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}
