package replication

import (
	"context"
	"encoding/json"
)

// EncodedSize is len(json.Marshal(i)), computed one object and one section
// entry at a time, so a size check neither builds the whole encoding nor
// outlasts its operation (#281). It checks ctx between elements and returns
// ctx's error once ctx ends. Once the size passes limit it stops early and
// returns a size above limit; a limit of zero or less never stops early.
//
// The arithmetic relies on the encoding of Image: the payload's object list
// and sections have no omitempty and encode as JSON arrays of their
// elements, so each list adds its elements' own encodings, and the commas
// between them, to the encoding of the image with every list empty.
func EncodedSize(ctx context.Context, i Image, limit int64) (int64, error) {
	p := &progress{ctx: ctx}
	if err := p.now(); err != nil {
		return 0, err
	}
	var lists [][]any
	empty := i
	empty.Payload.Objects = []json.RawMessage{}
	lists = append(lists, elements(i.Payload.Objects))
	for _, name := range SectionNames() {
		lists = append(lists, elements(*Section(&i.Payload, name)))
		*Section(&empty.Payload, name) = []Entry{}
	}
	b, err := json.Marshal(empty)
	if err != nil {
		return 0, err
	}
	size := int64(len(b))
	over := func() bool { return limit > 0 && size > limit }
	for _, list := range lists {
		if list == nil {
			// A nil list encodes as null, not as the empty array counted.
			size += int64(len("null") - len("[]"))
		}
		for k, element := range list {
			if err := p.step(); err != nil {
				return 0, err
			}
			b, err := json.Marshal(element)
			if err != nil {
				return 0, err
			}
			size += int64(len(b))
			if k > 0 {
				size++ // the comma before this element
			}
			if over() {
				return size, nil
			}
		}
	}
	if err := p.now(); err != nil {
		return 0, err
	}
	return size, nil
}

// elements keeps a nil list nil, so EncodedSize can count it as null.
func elements[T any](list []T) []any {
	if list == nil {
		return nil
	}
	out := make([]any, len(list))
	for k := range list {
		out[k] = list[k]
	}
	return out
}
