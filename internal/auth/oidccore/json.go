package oidccore

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"unicode/utf8"
)

// Parse a bounded object without allowing duplicate keys, repaired UTF-8 or
// unpaired escaped surrogates. Limits are enforced while walking, before maps
// can accumulate attacker-controlled members beyond the declared profile.
func parseObject(raw []byte, limit int) (map[string]any, error) {
	return parseObjectArrays(raw, limit, MaxJSONMembers)
}

func parseObjectArrays(raw []byte, limit, arrayLimit int) (map[string]any, error) {
	if len(raw) > limit {
		return nil, ErrBounds
	}
	if !utf8.Valid(raw) || !validSurrogates(raw) {
		return nil, ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	nodes := 0
	v, err := readJSON(d, 0, &nodes, arrayLimit)
	if err != nil {
		return nil, err
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, ErrInvalid
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, ErrInvalid
	}
	return m, nil
}

func readJSON(d *json.Decoder, depth int, nodes *int, arrayLimit int) (any, error) {
	if depth > MaxJSONDepth || *nodes >= MaxJSONNodes {
		return nil, ErrBounds
	}
	*nodes++
	t, err := d.Token()
	if err != nil {
		return nil, ErrInvalid
	}
	switch v := t.(type) {
	case json.Delim:
		switch v {
		case '{':
			m := map[string]any{}
			for d.More() {
				if len(m) >= MaxJSONMembers {
					return nil, ErrBounds
				}
				k, err := d.Token()
				s, ok := k.(string)
				if err != nil || !ok {
					return nil, ErrInvalid
				}
				if len(s) > MaxStringBytes {
					return nil, ErrBounds
				}
				if _, duplicate := m[s]; duplicate {
					return nil, ErrInvalid
				}
				value, err := readJSON(d, depth+1, nodes, arrayLimit)
				if err != nil {
					return nil, err
				}
				m[s] = value
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return nil, ErrInvalid
			}
			return m, nil
		case '[':
			a := []any{}
			for d.More() {
				if len(a) >= arrayLimit {
					return nil, ErrBounds
				}
				v, err := readJSON(d, depth+1, nodes, arrayLimit)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return nil, ErrInvalid
			}
			return a, nil
		default:
			return nil, ErrInvalid
		}
	case string:
		if len(v) > MaxStringBytes {
			return nil, ErrBounds
		}
	}
	return t, nil
}

func validSurrogates(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		n, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n < 0xd800 || n > 0xdbff {
			continue
		}
		if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return false
		}
		n, err = strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
		if err != nil || n < 0xdc00 || n > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}

func cloneJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = cloneJSON(v)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = cloneJSON(v)
		}
		return out
	default:
		return v
	}
}
