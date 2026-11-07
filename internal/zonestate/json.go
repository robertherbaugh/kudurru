package zonestate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// canonicalJSON rejects ambiguous duplicate keys and preserves exact decimal
// numbers. Nested desired/config data is bounded by depth and input bytes.
func canonicalJSON(raw []byte) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	budget := int64(128 << 20)
	v, err := readJSON(d, 64, &budget)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: trailing JSON", ErrInvalid)
	}
	return json.Marshal(v)
}
func readJSON(d *json.Decoder, depth int, budget *int64) (any, error) {
	if *budget < 32 {
		return nil, ErrCapacity
	}
	*budget -= 32
	if depth <= 0 {
		return nil, fmt.Errorf("%w: JSON depth", ErrCapacity)
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		if s, ok := t.(string); ok {
			if int64(len(s)) > *budget {
				return nil, ErrCapacity
			}
			*budget -= int64(len(s))
		}
		return t, nil
	}
	switch delim {
	case '{':
		out := map[string]any{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			k, ok := key.(string)
			if !ok {
				return nil, ErrInvalid
			}
			if int64(len(k))+64 > *budget {
				return nil, ErrCapacity
			}
			*budget -= int64(len(k)) + 64
			if _, ok := out[k]; ok {
				return nil, fmt.Errorf("%w: duplicate JSON key", ErrInvalid)
			}
			v, err := readJSON(d, depth-1, budget)
			if err != nil {
				return nil, err
			}
			out[k] = v
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return nil, ErrInvalid
		}
		return out, nil
	case '[':
		out := []any{}
		for d.More() {
			v, err := readJSON(d, depth-1, budget)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return nil, ErrInvalid
		}
		return out, nil
	default:
		return nil, ErrInvalid
	}
}
