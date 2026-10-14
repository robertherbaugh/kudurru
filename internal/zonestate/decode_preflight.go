package zonestate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
)

// scanAggregate validates bounded JSON without a generic DOM or typed slices.
// In particular a compact numeric types array cannot amplify into thousands of
// copied grants before the policy count check. Token strings are bounded by
// the already bounded encoded input; container workspace is charged separately.
func scanAggregate(data []byte, l Limits) error {
	if int64(len(data)) > l.MaxAggregateBytes {
		return ErrCapacity
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	left := l.MaxAggregateBytes
	typesLeft := dynamicupdate.MaxTotalGrantTypes
	var walk func(string, int) error
	walk = func(path string, depth int) error {
		if depth == 0 || left < 32 {
			return ErrCapacity
		}
		left -= 32
		t, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			keys := map[string]bool{}
			for d.More() {
				t, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := t.(string)
				if !ok || keys[key] {
					return ErrInvalid
				}
				if int64(len(key))+64 > left {
					return ErrCapacity
				}
				left -= int64(len(key)) + 64
				keys[key] = true
				if err := walk(path+"/"+key, depth-1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return ErrInvalid
			}
		case '[':
			limit := l.MaxRecords
			switch path {
			case "/policy/credentials", "/policy/grants":
				limit = dynamicupdate.MaxPolicyEntries
			case "/policy/grants/*/types":
				limit = dynamicupdate.MaxGrantTypes
			case "/receipts":
				limit = l.MaxReceipts
			}
			count := 0
			for d.More() {
				count++
				if count > limit {
					return ErrCapacity
				}
				if path == "/policy/grants/*/types" {
					typesLeft--
					if typesLeft < 0 {
						return ErrCapacity
					}
				}
				if err := walk(path+"/*", depth-1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return ErrInvalid
			}
		default:
			return ErrInvalid
		}
		return nil
	}
	if err := walk("", 64); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("%w: trailing JSON", ErrInvalid)
	}
	return nil
}

func strictDecodeAggregate(data []byte, into *Aggregate, l Limits) error {
	if err := scanAggregate(data, l); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}
