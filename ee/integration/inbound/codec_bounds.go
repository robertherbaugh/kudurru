package inbound

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	maxDocumentBytes      = min(kinds.MaxSourceBytes, kinds.MaxSourceChunks*kinds.MaxSourceChunkBytes)
	maxDocumentString     = 8192
	maxDocumentCollection = kinds.MaxSourceRecordSets + kinds.MaxSourceZones
	maxMetadataJSON       = 64 << 10
)

var errDocumentBound = errors.New("inbound: source string/count/encoding bound")

// checkDocumentBounds precedes semantic validators that hash whole desired
// specs. Public objects include metadata and status, so every field is bounded.
func checkDocumentBounds(d Document) error {
	if len(d.Snapshot.Clients) > kinds.MaxSourceClients || len(d.Targets) > kinds.MaxSourceZones+1 || len(d.Objects) > kinds.MaxSourceRecordSets+kinds.MaxSourceZones || len(d.Allocations) > kinds.MaxSourceAllocations || len(d.Groups) > kinds.MaxSourceGroups || len(d.Reverse) > kinds.MaxSourceZones {
		return errDocumentBound
	}
	if batch := d.Snapshot.Desired; batch != nil {
		if len(batch.Zones) > kinds.MaxSourceZones || len(batch.RecordSets) > kinds.MaxSourceRecordSets || len(batch.ClientGroups) > kinds.MaxSourceGroups {
			return errDocumentBound
		}
	}
	// Reject zoned addresses before MarshalText can allocate a zone suffix.
	for _, client := range d.Snapshot.Clients {
		for _, ip := range client.IPs {
			if ip.Zone() != "" {
				return errDocumentBound
			}
		}
	}
	// Reuse all exact inventory string/address/tag/count and escaped byte caps.
	state := inventory.State{FormatVersion: inventory.StateFormatVersion, Generation: 1, Sources: []inventory.SourceState{{ID: host.SourceID(d.Owner), Revision: 1, Clients: d.Snapshot.Clients}}}
	if err := inventory.CheckStateBounds(state, maxDocumentBytes); err != nil {
		return errDocumentBound
	}
	return checkValueBounds(d)
}

func checkValueBounds(value any) error {
	budget := documentBudget{}
	return budget.value(reflect.ValueOf(value), 0)
}

// The typed graph has only fixed public structs and primitive collections.
// This counter has no full-payload allocation and never calls an arbitrary
// Marshaler. Inline struct braces/time upper bounds can overcount slightly.
type documentBudget struct{ bytes int }

func (b *documentBudget) add(n int) error {
	if n < 0 || n > maxDocumentBytes-b.bytes {
		return errDocumentBound
	}
	b.bytes += n
	return nil
}
func (b *documentBudget) str(s string) error {
	if len(s) > maxDocumentString || !utf8.ValidString(s) {
		return errDocumentBound
	}
	n := 2 + len(s)
	for _, r := range s {
		switch r {
		case '"', '\\', '\b', '\f', '\n', '\r', '\t':
			n++
		case '<', '>', '&':
			n += 5
		case '\u2028', '\u2029':
			n += 3
		default:
			if r < 0x20 {
				n += 5
			}
		}
	}
	return b.add(n)
}
func (b *documentBudget) value(v reflect.Value, depth int) error {
	if depth > 32 {
		return errDocumentBound
	}
	if !v.IsValid() {
		return b.add(4)
	}
	if v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.CanInterface() && !v.IsNil() && customSourceMarshaler(v.Interface()) && !knownSourceLeaf(v.Elem().Type()) {
			return errDocumentBound
		}
		if v.IsNil() {
			return b.add(4)
		}
		return b.value(v.Elem(), depth+1)
	}
	if v.CanInterface() {
		switch leaf := v.Interface().(type) {
		case time.Time:
			data, err := leaf.MarshalJSON()
			if err != nil {
				return errDocumentBound
			}
			return b.add(len(data))
		case metav1.Time:
			data, err := leaf.MarshalJSON()
			if err != nil {
				return errDocumentBound
			}
			return b.add(len(data))
		case metav1.MicroTime:
			data, err := leaf.MarshalJSON()
			if err != nil {
				return errDocumentBound
			}
			return b.add(len(data))
		case netip.Addr:
			if leaf.Zone() != "" {
				return errDocumentBound
			}
			text, err := leaf.MarshalText()
			if err != nil || len(text) > 64 {
				return errDocumentBound
			}
			return b.str(string(text))
		case json.RawMessage:
			if len(leaf) > maxMetadataJSON {
				return errDocumentBound
			}
			canonical, err := canonicalSourceJSON(leaf, maxMetadataJSON)
			if err != nil {
				return errDocumentBound
			}
			if err := checkRawSourceFields(canonical); err != nil {
				return err
			}
			return b.add(max(len(leaf), len(canonical)) * 6)
		case metav1.FieldsV1:
			length := leaf.GetRawReader().Size()
			if length == 0 {
				return b.add(4)
			}
			if length > maxMetadataJSON {
				return errDocumentBound
			}
			raw := leaf.GetRawBytes()
			canonical, err := canonicalSourceJSON(raw, maxMetadataJSON)
			if err != nil {
				return errDocumentBound
			}
			if err := checkRawSourceFields(canonical); err != nil {
				return err
			}
			// json.Marshal preserves raw field content; escaping may expand it.
			return b.add(max(len(raw), len(canonical)) * 6)
		}
	}
	if v.CanInterface() && customSourceMarshaler(v.Interface()) {
		return errDocumentBound
	}
	switch v.Kind() {
	case reflect.String:
		return b.str(v.String())
	case reflect.Bool:
		if v.Bool() {
			return b.add(4)
		}
		return b.add(5)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return b.add(len(strconv.FormatInt(v.Int(), 10)))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return b.add(len(strconv.FormatUint(v.Uint(), 10)))
	case reflect.Float32, reflect.Float64:
		return b.add(32)
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return b.add(4)
		}
		if v.Type().Elem().Kind() == reflect.Uint8 {
			if v.Len() > maxMetadataJSON {
				return errDocumentBound
			}
			return b.add(2 + 4*((v.Len()+2)/3))
		}
		if v.Len() > maxDocumentCollection {
			return errDocumentBound
		}
		if err := b.add(2 + max(0, v.Len()-1)); err != nil {
			return err
		}
		for i := 0; i < v.Len(); i++ {
			if err := b.value(v.Index(i), depth+1); err != nil {
				return err
			}
		}
	case reflect.Map:
		if v.IsNil() {
			return b.add(4)
		}
		if v.Len() > maxDocumentCollection || v.Type().Key().Kind() != reflect.String {
			return errDocumentBound
		}
		if err := b.add(2 + max(0, v.Len()-1)); err != nil {
			return err
		}
		iter := v.MapRange()
		for iter.Next() {
			if err := b.str(iter.Key().String()); err != nil {
				return err
			}
			if err := b.add(1); err != nil {
				return err
			}
			if err := b.value(iter.Value(), depth+1); err != nil {
				return err
			}
		}
	case reflect.Struct:
		if err := b.add(2); err != nil {
			return err
		}
		typ := v.Type()
		fields := 0
		for i := 0; i < v.NumField(); i++ {
			field := typ.Field(i)
			if field.PkgPath != "" {
				continue
			}
			tag := strings.Split(field.Tag.Get("json"), ",")
			if tag[0] == "-" {
				continue
			}
			child := v.Field(i)
			if slices.Contains(tag, "omitempty") && emptyJSONValue(child) {
				continue
			}
			if slices.Contains(tag, "omitzero") && child.IsZero() {
				continue
			}
			if fields > 0 {
				if err := b.add(1); err != nil {
					return err
				}
			}
			fields++
			name := tag[0]
			if name == "" && !field.Anonymous {
				name = field.Name
			}
			if name != "" {
				if err := b.str(name); err != nil {
					return err
				}
				if err := b.add(1); err != nil {
					return err
				}
			}
			if slices.Contains(tag, "string") {
				if err := b.add(2); err != nil {
					return err
				}
			}
			if err := b.value(child, depth+1); err != nil {
				return err
			}
		}
	default:
		return errDocumentBound
	}
	return nil
}
func customSourceMarshaler(value any) bool {
	if _, ok := value.(json.Marshaler); ok {
		return true
	}
	_, ok := value.(encoding.TextMarshaler)
	return ok
}
func knownSourceLeaf(typ reflect.Type) bool {
	return typ == reflect.TypeFor[time.Time]() || typ == reflect.TypeFor[metav1.Time]() || typ == reflect.TypeFor[metav1.MicroTime]() || typ == reflect.TypeFor[netip.Addr]() || typ == reflect.TypeFor[metav1.FieldsV1]() || typ == reflect.TypeFor[json.RawMessage]()
}
func emptyJSONValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64, reflect.Interface, reflect.Pointer:
		return v.IsZero()
	}
	return false
}

func canonicalSourceJSON(data []byte, limit int) ([]byte, error) {
	if len(data) == 0 || len(data) > limit || !validSourceUnicode(data) {
		return nil, errDocumentBound
	}
	if err := checkRawSourceBounds(data); err != nil {
		return nil, err
	}
	entry, err := replication.ValidateEntry("runtime", replication.Entry{Key: "source-codec", Data: data}, replication.Limits{MaxObjectBytes: limit, MaxDepth: 32})
	if err != nil {
		return nil, err
	}
	return entry.Data, nil
}
func strictDocumentJSON(data []byte) (Document, error) {
	if len(data) == 0 || len(data) > maxDocumentBytes || !validSourceUnicode(data) {
		return Document{}, errDocumentBound
	}
	if err := checkRawSourceBounds(data); err != nil {
		return Document{}, err
	}
	var document Document
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return Document{}, err
	}
	if err := checkDocumentBounds(document); err != nil {
		return Document{}, err
	}
	original, err := canonicalSourceJSON(data, maxDocumentBytes)
	if err != nil {
		return Document{}, err
	}
	typed, err := json.Marshal(document)
	if err != nil {
		return Document{}, err
	}
	canonical, err := canonicalSourceJSON(typed, maxDocumentBytes)
	if err != nil || !bytes.Equal(original, canonical) {
		return Document{}, errors.New("inbound: ambiguous original source JSON")
	}
	return document, nil
}

// encoding/json substitutes malformed UTF8/unpaired surrogates. Reject them
// before canonicalization could disguise corruption behind a consistent digest.
func validSourceUnicode(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	for i := 0; i < len(data); i++ {
		if data[i] != '"' {
			continue
		}
		i++
		for i < len(data) && data[i] != '"' {
			if data[i] != '\\' {
				i++
				continue
			}
			i++
			if i >= len(data) {
				return false
			}
			if data[i] != 'u' {
				i++
				continue
			}
			if i+4 >= len(data) {
				return false
			}
			n, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
			if err != nil {
				return false
			}
			i += 5
			if n >= 0xdc00 && n <= 0xdfff {
				return false
			}
			if n >= 0xd800 && n <= 0xdbff {
				if i+5 >= len(data) || data[i] != '\\' || data[i+1] != 'u' {
					return false
				}
				low, err := strconv.ParseUint(string(data[i+2:i+6]), 16, 16)
				if err != nil || low < 0xdc00 || low > 0xdfff {
					return false
				}
				i += 6
			}
		}
	}
	return true
}

// Managed-field/raw metadata still obeys per-string and collection bounds.
func checkRawSourceFields(data []byte) error {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return errDocumentBound
	}
	return checkValueBounds(value)
}

// Token limits precede typed decoding/canonical marshaling. Unknown nested
// metadata cannot allocate an unbounded collection or oversized output string.
func checkRawSourceBounds(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := sourceWireValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errDocumentBound
	}
	return nil
}
func sourceWireValue(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return errDocumentBound
	}
	token, err := decoder.Token()
	if err != nil {
		return errDocumentBound
	}
	switch value := token.(type) {
	case json.Delim:
		if value != '{' && value != '[' {
			return errDocumentBound
		}
		count := 0
		for decoder.More() {
			count++
			if count > maxDocumentCollection {
				return errDocumentBound
			}
			if value == '{' {
				key, err := decoder.Token()
				if err != nil {
					return errDocumentBound
				}
				name, ok := key.(string)
				if !ok || len(name) > maxDocumentString {
					return errDocumentBound
				}
			}
			if err := sourceWireValue(decoder, depth+1); err != nil {
				return err
			}
		}
		ending, err := decoder.Token()
		if err != nil || value == '{' && ending != json.Delim('}') || value == '[' && ending != json.Delim(']') {
			return errDocumentBound
		}
	case string:
		if len(value) > maxDocumentString {
			return errDocumentBound
		}
	case json.Number:
		if len(value) > 32 {
			return errDocumentBound
		}
	case bool, nil:
	default:
		return errDocumentBound
	}
	return nil
}
