package providerregistry

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Bound and reject duplicate keys before reflection or typed allocations.
func strict(raw []byte, out any, limit int) error {
	if len(raw) > limit {
		return ErrBounds
	}
	if !utf8.Valid(raw) {
		return ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	nodes := 0
	if err := walk(d, 0, &nodes); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrInvalid
	}
	if err := fields(raw, reflect.TypeOf(out).Elem()); err != nil {
		return err
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return ErrInvalid
	}
	return nil
}

func walk(d *json.Decoder, depth int, nodes *int) error {
	*nodes++
	if depth > 16 || *nodes > 8192 {
		return ErrBounds
	}
	t, err := d.Token()
	if err != nil {
		return ErrInvalid
	}
	switch v := t.(type) {
	case json.Delim:
		if v != '{' && v != '[' {
			return ErrInvalid
		}
		seen := map[string]bool{}
		count := 0
		for d.More() {
			count++
			if count > 1024 {
				return ErrBounds
			}
			if v == '{' {
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
			if err := walk(d, depth+1, nodes); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || v == '{' && end != json.Delim('}') || v == '[' && end != json.Delim(']') {
			return ErrInvalid
		}
	case string:
		if len(v) > MaxRowBytes {
			return ErrBounds
		}
	}
	return nil
}

// encoding/json otherwise accepts case-insensitive aliases and omitted bools.
func fields(raw []byte, typ reflect.Type) error {
	if typ == reflect.TypeFor[time.Time]() {
		return nil
	}
	if typ.Kind() == reflect.Pointer {
		if bytes.Equal(raw, []byte("null")) {
			return nil
		}
		return fields(raw, typ.Elem())
	}
	switch typ.Kind() {
	case reflect.Struct:
		var values map[string]json.RawMessage
		if json.Unmarshal(raw, &values) != nil || values == nil {
			return ErrInvalid
		}
		known := map[string]bool{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			tag := strings.Split(f.Tag.Get("json"), ",")
			name := tag[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			known[name] = true
			value, ok := values[name]
			if !ok {
				if !strings.Contains(f.Tag.Get("json"), "omitempty") {
					return ErrManifest
				}
				continue
			}
			if strings.Contains(f.Tag.Get("json"), ",string") {
				var counter string
				if json.Unmarshal(value, &counter) != nil {
					return ErrInvalid
				}
				n, err := strconv.ParseUint(counter, 10, 64)
				if err != nil || n == 0 || strconv.FormatUint(n, 10) != counter {
					return ErrInvalid
				}
			}
			if f.Type.Kind() == reflect.Bool && !bytes.Equal(value, []byte("true")) && !bytes.Equal(value, []byte("false")) {
				return ErrInvalid
			}
			if err := fields(value, f.Type); err != nil {
				return err
			}
		}
		for name := range values {
			if !known[name] {
				return ErrInvalid
			}
		}
	case reflect.Slice:
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return ErrInvalid
		}
		if len(values) > 1024 {
			return ErrBounds
		}
		for _, value := range values {
			if err := fields(value, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
