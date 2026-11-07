package inbound

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Rehash every carrier and root. Raw-framing failures cannot rely on digests.
func imageWithRawDocument(t *testing.T, base SourceImage, data []byte) SourceImage {
	t.Helper()
	head := base.Head.DeepCopy()
	head.Spec.RootDigest = kinds.Digest(data)
	head.Spec.Chunks = nil
	out := SourceImage{Head: head}
	for offset := 0; offset < len(data); offset += kinds.MaxSourceChunkBytes {
		part := bytes.Clone(data[offset:min(len(data), offset+kinds.MaxSourceChunkBytes)])
		i := len(out.Chunks)
		digest := kinds.Digest(part)
		c := &kinds.IntegrationSourceChunk{ObjectMeta: metav1.ObjectMeta{Name: kinds.SourceChunkName(head.Spec.Owner, head.Spec.Sequence, i, digest)}, Spec: kinds.IntegrationSourceChunkSpec{FormatVersion: 1, Owner: head.Spec.Owner, Sequence: head.Spec.Sequence, Index: i, Digest: digest, Data: part}}
		out.Chunks = append(out.Chunks, c)
		head.Spec.Chunks = append(head.Spec.Chunks, kinds.SourceChunkRef{Name: c.Name, Digest: digest, Bytes: len(part)})
	}
	return out
}

func TestSourceStrictOriginalFramingWithConsistentDigests(t *testing.T) {
	d, _, at := sourceDocument(t)
	base := encodeDocument(t, d, at)
	encoded, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	original := string(encoded)
	for name, wire := range map[string]string{
		"duplicate root":               strings.Replace(original, `"formatVersion":1`, `"formatVersion":1,"formatVersion":1`, 1),
		"duplicate complete":           strings.Replace(original, `"Complete":true`, `"Complete":false,"Complete":true`, 1),
		"escaped duplicate":            strings.Replace(original, `"Complete":true`, `"Complete":true,"\u0043omplete":true`, 1),
		"null complete":                strings.Replace(original, `"Complete":true`, `"Complete":null`, 1),
		"missing complete":             strings.Replace(original, `,"Complete":true`, "", 1),
		"case alias":                   strings.Replace(original, `"Complete":true`, `"complete":true`, 1),
		"case client alias":            strings.Replace(original, `"id":"a"`, `"ID":"a"`, 1),
		"duplicate client id":          strings.Replace(original, `"id":"a"`, `"id":"b","id":"a"`, 1),
		"null observed":                strings.Replace(original, `"observedAt":"1970-01-02T03:46:40Z"`, `"observedAt":null`, 1),
		"unknown metadata":             strings.Replace(original, `"metadata":{`, `"metadata":{"unknown":true,`, 1),
		"null nested optional boolean": strings.Replace(original, `"kudurru":{`, `"kudurru":{"disabled":null,`, 1),
		"surrogate":                    strings.Replace(original, `"id":"a"`, `"id":"\ud800"`, 1),
		"invalid utf8":                 strings.Replace(original, `"id":"a"`, "\"id\":\"\xff\"", 1),
		"trailing root":                original + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if wire == original {
				t.Fatal("framing mutation missed fixture")
			}
			if _, err := DecodeSource(imageWithRawDocument(t, base, []byte(wire))); err == nil {
				t.Fatal("attacker-consistent historical authority accepted")
			}
		})
	}
	for _, path := range []string{"owner", "snapshot", "clients", "desired", "ips", "observedAt", "expiresAt", "source", "id"} {
		t.Run("missing "+path, func(t *testing.T) {
			var root map[string]any
			if err := json.Unmarshal(encoded, &root); err != nil {
				t.Fatal(err)
			}
			snap := root["snapshot"].(map[string]any)
			switch path {
			case "owner", "snapshot":
				delete(root, path)
			case "clients":
				delete(snap, "Clients")
			case "desired":
				delete(snap, "Desired")
			default:
				delete(snap["Clients"].([]any)[0].(map[string]any), path)
			}
			wire, err := json.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeSource(imageWithRawDocument(t, base, wire)); err == nil {
				t.Fatal("required authority field omitted")
			}
		})
	}
}

func TestSourceCanonicalOptionalsUnicodeNullAndOriginalAge(t *testing.T) {
	d, _, at := sourceDocument(t)
	rr := d.Snapshot.Desired.RecordSets[0]
	rr.Annotations = map[string]string{"note": "😀 <>& \" \\ \u2028"}
	rr.Labels = map[string]string{"application": "device"}
	rr.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Secret", Name: "fixture", UID: "original", Controller: boolPtr(false), BlockOwnerDeletion: boolPtr(true)}}
	rr.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "test", FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:metadata":{"f:labels":{"f:application":{}}}}`)}}
	image := encodeDocument(t, d, at)
	decoded, err := DecodeSource(image)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(d)
	again, _ := json.Marshal(decoded)
	if !bytes.Equal(original, again) {
		t.Fatal("valid canonical optionals or original timestamps changed")
	}
	spaced := bytes.Replace(original, []byte("😀"), []byte(`\ud83d\ude00`), 1)
	if _, err := DecodeSource(imageWithRawDocument(t, image, spaced)); err != nil {
		t.Fatal("valid UTF16 pair rejected", err)
	}
	var tree any
	if err := json.Unmarshal(original, &tree); err != nil {
		t.Fatal(err)
	}
	pretty, err := json.MarshalIndent(tree, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeSource(imageWithRawDocument(t, image, pretty)); err != nil {
		t.Fatal("irrelevant whitespace rejected", err)
	}
	// Existing emitted null required arrays and explicit false optional pointers
	// survive; omitempty fields retain exactly their original canonical shape.
	d.Snapshot.Desired = nil
	d.Snapshot.Clients = nil
	d.Targets = nil
	d.Objects = nil
	d.Allocations = nil
	d.Groups = nil
	d.Reverse = nil
	if _, err := DecodeSource(encodeDocument(t, d, at)); err != nil {
		t.Fatal("canonical null arrays rejected", err)
	}
}
func boolPtr(v bool) *bool { return &v }

func TestSourcePreMarshalBoundsAcrossWholePublicObjects(t *testing.T) {
	original, _, at := sourceDocument(t)
	for name, mutate := range map[string]func(*Document){
		"metadata annotation": func(d *Document) {
			d.Snapshot.Desired.RecordSets[0].Annotations = map[string]string{"note": strings.Repeat("x", maxDocumentString+1)}
		},
		"metadata key": func(d *Document) {
			d.Snapshot.Desired.RecordSets[0].Labels = map[string]string{strings.Repeat("x", maxDocumentString+1): "value"}
		},
		"status ignored by semantics": func(d *Document) {
			d.Snapshot.Desired.RecordSets[0].Status.Publications = []public.PublicationState{{Message: strings.Repeat("x", maxDocumentString+1)}}
		},
		"endpoint comment": func(d *Document) {
			d.Snapshot.Desired.RecordSets[0].Spec.Endpoints[0].Kudurru.Comment = strings.Repeat("x", maxDocumentString+1)
		},
		"inventory name": func(d *Document) { d.Snapshot.Clients[0].Name = strings.Repeat("x", 254) },
		"inventory tags": func(d *Document) { d.Snapshot.Clients[0].Tags = make([]string, 65) },
		"metadata count": func(d *Document) {
			d.Snapshot.Desired.RecordSets[0].Annotations = make(map[string]string, maxDocumentCollection+1)
			for i := 0; i < maxDocumentCollection+1; i++ {
				d.Snapshot.Desired.RecordSets[0].Annotations[strconv.Itoa(i)] = "x"
			}
		},
		"metadata utf8": func(d *Document) { d.Snapshot.Desired.RecordSets[0].Annotations = map[string]string{"x": "\xff"} },
		"managed fields raw": func(d *Document) {
			d.Snapshot.Desired.RecordSets[0].ManagedFields = []metav1.ManagedFieldsEntry{{FieldsV1: metav1.NewFieldsV1(strings.Repeat("x", maxMetadataJSON+1))}}
		},
		"managed fields duplicates": func(d *Document) {
			d.Snapshot.Desired.RecordSets[0].ManagedFields = []metav1.ManagedFieldsEntry{{FieldsV1: metav1.NewFieldsV1(`{"a":{},"a":{}}`)}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := cloneDocument(t, original)
			mutate(&d)
			if err := checkDocumentBounds(d); err == nil {
				t.Fatal("public-field preflight missing")
			}
			if _, err := EncodeSource(d, 1, kinds.Digest([]byte("config")), kinds.Digest([]byte("credential")), at); err == nil {
				t.Fatal("unbounded authority encoded")
			}
		})
	}
	// Shared string backing keeps input small. Aggregate estimate must stop
	// before marshaling all valid-width metadata strings into >23MiB JSON.
	d := cloneDocument(t, original)
	d.Snapshot.Desired.RecordSets[0].Annotations = map[string]string{}
	shared := strings.Repeat("x", maxDocumentString)
	for i := 0; i < 4000; i++ {
		d.Snapshot.Desired.RecordSets[0].Annotations[strconv.Itoa(i)] = shared
	}
	if err := checkDocumentBounds(d); !errors.Is(err, errDocumentBound) {
		t.Fatal("aggregate preallocation bound missed", err)
	}
	if _, err := IntentDigest(d.Snapshot.Desired.RecordSets[0].Spec); err != nil {
		t.Fatal("valid ordinary spec rejected", err)
	}
	if _, err := IntentDigest(d.Snapshot.Desired.RecordSets[0]); err == nil {
		t.Fatal("standalone digest marshaled unbounded public metadata")
	}
}

func TestSourceCannotSilentlyRepairInventory(t *testing.T) {
	original, _, at := sourceDocument(t)
	for name, mutate := range map[string]func(*inventory.Client){
		"trimmed tag":      func(c *inventory.Client) { c.Tags = []string{" tag "} },
		"noncanonical mac": func(c *inventory.Client) { c.MACs = []string{"02:AB:CD:EF:00:01"} },
		"duplicate tags":   func(c *inventory.Client) { c.Tags = []string{"a", "a"} },
	} {
		t.Run(name, func(t *testing.T) {
			d := cloneDocument(t, original)
			mutate(&d.Snapshot.Clients[0])
			if err := ValidateDocument(d, at); err == nil {
				t.Fatal("original evidence normalized behind digest")
			}
			wire, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeSource(imageWithRawDocument(t, encodeDocument(t, original, at), wire)); err == nil {
				t.Fatal("historical malformed evidence repaired")
			}
		})
	}
}

type customMarshalBomb struct{ called *bool }

func (b customMarshalBomb) MarshalJSON() ([]byte, error) {
	*b.called = true
	return []byte(`"unsafe"`), nil
}
func TestSourceDigestRejectsUnknownCustomMarshalersAndCycles(t *testing.T) {
	called := false
	if _, err := IntentDigest(customMarshalBomb{called: &called}); err == nil || called {
		t.Fatal("unchecked custom marshaler called")
	}
	bomb := customMarshalBomb{called: &called}
	if _, err := IntentDigest(&bomb); err == nil || called {
		t.Fatal("unchecked pointer marshaler called")
	}
	type cycle struct{ Next *cycle }
	node := &cycle{}
	node.Next = node
	if _, err := IntentDigest(node); err == nil {
		t.Fatal("cyclic graph allocated")
	}
}

func TestDocumentSizePreflightDoesNotUndercount(t *testing.T) {
	d, _, _ := sourceDocument(t)
	d.Snapshot.Desired.RecordSets[0].Annotations = map[string]string{"note": "<>& \n\x01\u2028\u2029😀"}
	budget := documentBudget{}
	if err := budget.value(reflect.ValueOf(d), 0); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if budget.bytes < len(data) {
		t.Fatalf("preflight %d < encoded %d", budget.bytes, len(data))
	}
	if err := checkValueBounds(time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func TestSourcePreflightRejectsZoneSuffixAndNestedRawStrings(t *testing.T) {
	d, _, _ := sourceDocument(t)
	// Use IPv6 to exercise the untrusted zone suffix.
	d.Snapshot.Clients[0].IPs[0] = netip.MustParseAddr("2001:db8::1").WithZone(strings.Repeat("z", maxDocumentString+1))
	if err := checkDocumentBounds(d); err == nil {
		t.Fatal("zoned address reached text marshaler")
	}
	d, _, _ = sourceDocument(t)
	d.Snapshot.Desired.RecordSets[0].ManagedFields = []metav1.ManagedFieldsEntry{{FieldsV1: metav1.NewFieldsV1(`{"nested":"` + strings.Repeat("x", maxDocumentString+1) + `"}`)}}
	if err := checkDocumentBounds(d); err == nil {
		t.Fatal("opaque metadata bypassed nested string bound")
	}
}

func TestSourceRawBoundsPrecedeCanonicalMarshal(t *testing.T) {
	for _, wire := range [][]byte{
		[]byte(`{"unknown":"` + strings.Repeat("x", maxDocumentString+1) + `"}`),
		[]byte("[" + strings.Repeat("0,", maxDocumentCollection) + "0]"),
		[]byte(`{"known":` + strings.Repeat("9", 33) + `}`),
	} {
		if err := checkRawSourceBounds(wire); err == nil {
			t.Fatal("unbounded historical value reached canonical marshal")
		}
	}
	if err := checkRawSourceBounds([]byte(`{"known":[null,false,true,"ok",12]}`)); err != nil {
		t.Fatal("ordinary bounded JSON rejected", err)
	}
}

func TestSourceOriginalAdmissionTimeMustBeEncodable(t *testing.T) {
	d, _, _ := sourceDocument(t)
	d.Snapshot.Desired = nil
	d.Snapshot.Clients = nil
	d.Targets = nil
	d.Objects = nil
	d.Allocations = nil
	d.Groups = nil
	if _, err := EncodeSource(d, 1, kinds.Digest([]byte("config")), kinds.Digest([]byte("credential")), time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("unencodable carrier admission time accepted")
	}
}
