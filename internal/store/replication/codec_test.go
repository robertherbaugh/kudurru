package replication

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func sample(t *testing.T) Image {
	t.Helper()
	i := Image{StoreSchemaVersion: 2, Term: Term{ClusterID: "cluster", Epoch: 1, PrimaryID: "primary", PromotionID: "term"}, Revision: 5, ObjectResourceVersion: 9, CreatedAt: time.Unix(100, 0).UTC(), KindManifest: []Kind{{APIVersion: "kudurru.io/v1alpha1", Kind: "DNSZone"}, {APIVersion: "kudurru.io/v1alpha1", Kind: "LocalUser"}}}
	i.Payload.Objects = []json.RawMessage{json.RawMessage(`{"apiVersion":"kudurru.io/v1alpha1","kind":"DNSZone","metadata":{"name":"home","resourceVersion":"9"},"status":{"serial":4294967295},"spec":{}}`)}
	i.Payload.Security = []Entry{{Key: "sealed", Data: json.RawMessage(`{"ciphertext":"opaque-encrypted-by-owner"}`)}}
	if err := Seal(&i); err != nil {
		t.Fatal(err)
	}
	return i
}

func validation(i Image) Validation { return Validation{SchemaVersion: 2, Kinds: i.KindManifest} }

func TestCompleteImageStableDigestAndPrivateCopy(t *testing.T) {
	i := sample(t)
	v, err := Validate(i, validation(i))
	if err != nil {
		t.Fatal(err)
	}
	detached := v.Image()
	detached.Payload.Objects[0][0] = '!'
	detached.KindManifest[0].Kind = "changed"
	if _, err := Validate(v.Image(), validation(i)); err != nil {
		t.Fatalf("validated image was mutable: %v", err)
	}
	i.CreatedAt = time.Unix(200, 0)
	if err := Seal(&i); err != nil {
		t.Fatal(err)
	}
	if i.PayloadSHA256 != v.Image().PayloadSHA256 {
		t.Fatal("receipt/source clock changed content digest")
	}
	if len(i.KindManifest) != 2 || i.KindManifest[1].Count != 0 {
		t.Fatal("zero-object kind omitted")
	}
	if !bytes.Contains(v.Image().Payload.Objects[0], []byte(`4294967295`)) {
		t.Fatal("status lost")
	}
}

func TestRejectUnsafeCompleteImages(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Image)
		want   error
	}{
		{"new schema", func(i *Image) { i.StoreSchemaVersion = 3 }, ErrIncompatible},
		{"unknown feature", func(i *Image) { i.RequiredFeatures[0] = "future" }, ErrIncompatible},
		{"missing kind", func(i *Image) { i.KindManifest = i.KindManifest[:1] }, ErrIncompatible},
		{"wrong count", func(i *Image) { i.KindManifest[0].Count++ }, ErrInvalid},
		{"missing section", func(i *Image) { i.Payload.Security = nil }, ErrInvalid},
		{"duplicate object", func(i *Image) { i.Payload.Objects = append(i.Payload.Objects, i.Payload.Objects[0]) }, ErrInvalid},
		{"duplicate entry", func(i *Image) { i.Payload.Security = append(i.Payload.Security, i.Payload.Security[0]) }, ErrInvalid},
		{"tampered ciphertext", func(i *Image) { i.Payload.Security[0].Data = json.RawMessage(`{"ciphertext":"different"}`) }, ErrInvalid},
		{"second settings owner", func(i *Image) { i.Payload.ClusterSettings.Owner = "file" }, ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := sample(t)
			opts := validation(i)
			tc.mutate(&i)
			if _, err := Validate(i, opts); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestDecodeBoundsDuplicatesUnknownAndTruncation(t *testing.T) {
	i := sample(t)
	raw, _ := json.Marshal(i)
	for _, tc := range []struct {
		name string
		raw  []byte
		opts Validation
		want error
	}{
		{"bytes", raw, Validation{SchemaVersion: 2, Kinds: i.KindManifest, Limits: Limits{MaxBytes: 32}}, ErrTooLarge},
		{"objects", raw, Validation{SchemaVersion: 2, Kinds: i.KindManifest, Limits: Limits{MaxObjectBytes: 10}}, ErrTooLarge},
		{"depth", raw, Validation{SchemaVersion: 2, Kinds: i.KindManifest, Limits: Limits{MaxDepth: 2}}, ErrTooLarge},
		{"duplicate header", bytes.Replace(raw, []byte(`"complete":true`), []byte(`"complete":true,"complete":true`), 1), validation(i), ErrInvalid},
		{"duplicate private field", bytes.Replace(raw, []byte(`"ciphertext":"opaque-encrypted-by-owner"`), []byte(`"ciphertext":"a","ciphertext":"b"`), 1), validation(i), ErrInvalid},
		{"unknown header", bytes.Replace(raw, []byte(`"complete":true`), []byte(`"complete":true,"unexpected":1`), 1), validation(i), ErrInvalid},
		{"truncated", raw[:len(raw)-1], validation(i), ErrInvalid},
		{"trailing", append(append([]byte{}, raw...), []byte(`{}`)...), validation(i), ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode(bytes.NewReader(tc.raw), tc.opts); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	if _, err := Decode(strings.NewReader(`null`), validation(i)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("null: %v", err)
	}
}

func TestRequiredCanonicalCountersParentAndDigestBinding(t *testing.T) {
	i := sample(t)
	raw, _ := json.Marshal(i)
	for _, change := range []struct{ from, to string }{
		{`"revision":"5"`, `"revision":"05"`},
		{`"revision":"5",`, ``},
		{`"objectResourceVersion":"9"`, `"objectResourceVersion":"09"`},
		{`"epoch":"1"`, `"epoch":"01"`},
	} {
		if _, err := Decode(bytes.NewReader(bytes.Replace(raw, []byte(change.from), []byte(change.to), 1)), validation(i)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("noncanonical %s: %v", change.to, err)
		}
	}
	changed := i
	changed.ObjectResourceVersion++
	if _, err := Validate(changed, validation(i)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("source head not digest-bound: %v", err)
	}
	changed = i
	parent := i.Generation()
	changed.Parent = &parent
	if err := Seal(&changed); err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(changed, validation(i)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("same epoch parent accepted: %v", err)
	}
	changed.Term.Epoch++
	if err := Seal(&changed); err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(changed, validation(i)); err != nil {
		t.Fatalf("valid parent rejected: %v", err)
	}
	if changed.PayloadSHA256 == i.PayloadSHA256 {
		t.Fatal("parent not digest-bound")
	}
	label := i.Generation()
	label.Term.PrimaryName = "another display label"
	if label.ETag() != i.Generation().ETag() {
		t.Fatal("same authority identity generated different etag")
	}
}

func TestManifestAndSectionOrderingAndEntryBounds(t *testing.T) {
	i := sample(t)
	i.KindManifest[0], i.KindManifest[1] = i.KindManifest[1], i.KindManifest[0]
	i.PayloadSHA256, _ = Digest(i)
	if _, err := Validate(i, validation(i)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unordered manifest accepted: %v", err)
	}
	i = sample(t)
	i.Payload.Security = append(i.Payload.Security, Entry{Key: "second", Data: json.RawMessage(`{}`)})
	if err := Seal(&i); err != nil {
		t.Fatal(err)
	}
	opts := validation(i)
	opts.Limits.MaxEntries = 1
	if _, err := Validate(i, opts); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("entry limit: %v", err)
	}
	for _, raw := range []string{`{"x":1,"x":2}`, `null`, `{} {}`} {
		if _, err := ValidateEntry("security", Entry{Key: "bad", Data: json.RawMessage(raw)}, Limits{}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ambiguous local entry: %v", err)
		}
	}
}
