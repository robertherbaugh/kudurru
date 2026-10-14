package servingauth

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"reflect"
	"strings"
)

// Hard caps are checked before typed JSON allocations and compiler maps.
const (
	MaxRows          = 4096
	MaxRowBytes      = 64 << 10
	MaxRootBytes     = 512 << 10
	MaxSecurityBytes = 4 << 20
	MaxObjects       = 100000
	MaxObjectBytes   = 1536 << 10
	MaxImageBytes    = 256 << 20
	MaxGrants        = 128
	FormatVersion    = 1
	RootKey          = "auth/v1/root"
	ownerPrefix      = "auth/"
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
func strictOwner(raw []byte, into any, capBytes int) bool {
	if !boundedJSON(raw, capBytes) || !ownerFields(raw, reflect.TypeOf(into).Elem()) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(into) == nil
}

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
			if field.Type == reflect.TypeFor[AccountRef]() && !ownerFields(value, field.Type) {
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
func boundedJSON(raw []byte, capBytes int) bool {
	if len(raw) == 0 || len(raw) > capBytes {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	nodes := 0
	var walk func(int) bool
	walk = func(depth int) bool {
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
			return true
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
				if !walk(depth + 1) {
					return false
				}
			}
		case '[':
			count := 0
			for d.More() {
				count++
				if count > MaxRows || !walk(depth+1) {
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
	return err == io.EOF
}
