package replication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// EncodedSize is exactly the length of the whole encoding, including what
// json.Marshal changes in raw documents (HTML and line-separator escapes,
// whitespace), empty and nil lists, and every section.
func TestEncodedSizeIsTheLengthOfTheEncoding(t *testing.T) {
	base := sample(t)
	cases := map[string]func(*Image){
		"sample":      func(*Image) {},
		"no objects":  func(i *Image) { i.Payload.Objects = []json.RawMessage{} },
		"nil objects": func(i *Image) { i.Payload.Objects = nil },
		"nil section": func(i *Image) { i.Payload.Dedup = nil },
		"escapes and whitespace": func(i *Image) {
			i.Payload.Objects = append(i.Payload.Objects, json.RawMessage("{ \"apiVersion\": \"kudurru.io/v1alpha1\", \"kind\": \"DNSZone\",\n \"metadata\": {\"name\": \"<a&b>\", \"note\": \"line sep\"} }"))
		},
		"many": func(i *Image) {
			for k := range 300 {
				i.Payload.Objects = append(i.Payload.Objects, json.RawMessage(fmt.Sprintf(`{"kind":"DNSZone","metadata":{"name":"zone-%d"}}`, k)))
			}
			for _, name := range SectionNames() {
				for k := range 3 {
					*Section(&i.Payload, name) = append(*Section(&i.Payload, name), Entry{Key: fmt.Sprintf("%s-%d", name, k), Data: json.RawMessage(`{"v":"<x>"}`)})
				}
			}
		},
	}
	for name, edit := range cases {
		i := base
		i.Payload.Objects = append([]json.RawMessage(nil), base.Payload.Objects...)
		i.Payload.Security = append([]Entry(nil), base.Payload.Security...)
		edit(&i)
		b, err := json.Marshal(i)
		if err != nil {
			t.Fatal(name, err)
		}
		got, err := EncodedSize(t.Context(), i, 0)
		if err != nil || got != int64(len(b)) {
			t.Errorf("%s: EncodedSize = %d, %v; len(json.Marshal) = %d", name, got, err, len(b))
		}
		// A bound below the size stops early with a size above it; a bound
		// at the size does not refuse.
		if over, err := EncodedSize(t.Context(), i, int64(len(b))-1); err != nil || over <= int64(len(b))-1 {
			t.Errorf("%s: EncodedSize under a bound one byte short = %d, %v", name, over, err)
		}
		if at, err := EncodedSize(t.Context(), i, int64(len(b))); err != nil || at != int64(len(b)) {
			t.Errorf("%s: EncodedSize at its own bound = %d, %v", name, at, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := EncodedSize(ctx, base, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("EncodedSize with an ended context: %v", err)
	}
}
