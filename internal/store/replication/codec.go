package replication

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
)

// FeatureNames returns the complete required capabilities for this image format.
func FeatureNames() []string {
	return []string{"complete-set-v1", "singleton-settings-v1", "opaque-sections-v1", "provider-registry-v1", "inventory-state-v1", "auth-browser-attempts-v1"}
}

// Canonical checks duplicate JSON keys/depth and preserves numbers exactly.
func canonical(raw []byte, depth int) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	value, err := parseValue(d, depth)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: trailing JSON", ErrInvalid)
	}
	return json.Marshal(value)
}

func parseValue(d *json.Decoder, left int) (any, error) {
	if left <= 0 {
		return nil, fmt.Errorf("%w: JSON depth", ErrTooLarge)
	}
	t, err := d.Token()
	if err != nil {
		return nil, fmt.Errorf("%w: JSON", ErrInvalid)
	}
	if delim, ok := t.(json.Delim); ok {
		switch delim {
		case '{':
			m := map[string]any{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return nil, fmt.Errorf("%w: JSON key", ErrInvalid)
				}
				k, ok := key.(string)
				if !ok {
					return nil, ErrInvalid
				}
				if _, exists := m[k]; exists {
					return nil, fmt.Errorf("%w: duplicate JSON key", ErrInvalid)
				}
				v, err := parseValue(d, left-1)
				if err != nil {
					return nil, err
				}
				m[k] = v
			}
			_, err = d.Token()
			return m, err
		case '[':
			list := []any{}
			for d.More() {
				v, err := parseValue(d, left-1)
				if err != nil {
					return nil, err
				}
				list = append(list, v)
			}
			_, err = d.Token()
			return list, err
		default:
			return nil, ErrInvalid
		}
	}
	return t, nil
}

type objectHeader struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Namespace       string `json:"namespace"`
		Name            string `json:"name"`
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
}

func objectKey(raw json.RawMessage) (string, objectHeader, error) {
	var h objectHeader
	if err := json.Unmarshal(raw, &h); err != nil || h.Kind == "" || h.APIVersion == "" || h.Metadata.Name == "" {
		return "", h, fmt.Errorf("%w: object identity", ErrInvalid)
	}
	return h.Kind + "\x00" + h.Metadata.Namespace + "\x00" + h.Metadata.Name, h, nil
}

// Seal sorts a producer's complete image and computes a stable digest. It
// does not substitute for receiver validation against a supported registry.
func Seal(i *Image) error {
	i.Format, i.FormatVersion, i.Complete = Format, FormatVersion, true
	i.RequiredFeatures = FeatureNames()
	if i.Payload.ClusterSettings.Owner == "" {
		i.Payload.ClusterSettings.Owner = SettingsOwner
	}
	if i.Payload.Objects == nil {
		i.Payload.Objects = []json.RawMessage{}
	}
	sort.Slice(i.KindManifest, func(a, b int) bool { return i.KindManifest[a].Kind < i.KindManifest[b].Kind })
	keys := make(map[string]string)
	counts := make(map[string]int)
	for _, raw := range i.Payload.Objects {
		key, h, err := objectKey(raw)
		if err != nil {
			return err
		}
		keys[string(raw)] = key
		counts[h.Kind]++
	}
	sort.Slice(i.Payload.Objects, func(a, b int) bool { return keys[string(i.Payload.Objects[a])] < keys[string(i.Payload.Objects[b])] })
	for n := range i.KindManifest {
		i.KindManifest[n].Count = counts[i.KindManifest[n].Kind]
	}
	for _, name := range SectionNames() {
		rows := Section(&i.Payload, name)
		if *rows == nil {
			*rows = []Entry{}
		}
		sort.Slice(*rows, func(a, b int) bool { return (*rows)[a].Key < (*rows)[b].Key })
	}
	digest, err := Digest(*i)
	if err != nil {
		return err
	}
	i.PayloadSHA256 = digest
	return nil
}

// Digest binds the complete manifest and payload; source clocks are excluded.
func Digest(i Image) (string, error) {
	raw, err := json.Marshal(struct {
		Manifest              []Kind      `json:"manifest"`
		Payload               Payload     `json:"payload"`
		ObjectResourceVersion uint64      `json:"objectResourceVersion,string"`
		Parent                *Generation `json:"parent"`
	}{i.KindManifest, i.Payload, i.ObjectResourceVersion, i.Parent})
	if err != nil {
		return "", err
	}
	b, err := canonical(raw, 64)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func Decode(r io.Reader, opts Validation) (*Validated, error) {
	l := opts.Limits.Defaults()
	if l.MaxBytes == math.MaxInt64 {
		return nil, ErrInvalid
	}
	b, err := io.ReadAll(io.LimitReader(r, l.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > l.MaxBytes {
		return nil, ErrTooLarge
	}
	c, err := canonical(b, l.MaxDepth)
	if err != nil {
		return nil, err
	}
	// Counters are required canonical decimal strings, including zero. The
	// encoding/json string option alone accepts leading zeroes/missing fields.
	var header map[string]json.RawMessage
	if err := json.Unmarshal(c, &header); err != nil {
		return nil, ErrInvalid
	}
	var term map[string]json.RawMessage
	if err := json.Unmarshal(header["term"], &term); err != nil {
		return nil, ErrInvalid
	}
	for _, raw := range []json.RawMessage{header["revision"], header["objectResourceVersion"], term["epoch"]} {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, ErrInvalid
		}
		n, err := strconv.ParseUint(value, 10, 64)
		if err != nil || strconv.FormatUint(n, 10) != value {
			return nil, fmt.Errorf("%w: noncanonical counter", ErrInvalid)
		}
	}
	var i Image
	d := json.NewDecoder(bytes.NewReader(c))
	d.DisallowUnknownFields()
	if err := d.Decode(&i); err != nil {
		return nil, fmt.Errorf("%w: envelope", ErrInvalid)
	}
	// Missing/null sections are unsafe: an omitted section is not a deletion.
	if i.Payload.Objects == nil || i.Payload.Runtime == nil || i.Payload.Security == nil || i.Payload.Membership == nil || i.Payload.Dedup == nil || i.RequiredFeatures == nil || i.KindManifest == nil {
		return nil, fmt.Errorf("%w: missing complete section", ErrInvalid)
	}
	if err := validate(i, opts); err != nil {
		return nil, err
	}
	return &Validated{encoded: c}, nil
}

func Validate(i Image, opts Validation) (*Validated, error) {
	b, err := json.Marshal(i)
	if err != nil {
		return nil, err
	}
	return Decode(bytes.NewReader(b), opts)
}

func validate(i Image, opts Validation) error {
	l := opts.Limits.Defaults()
	if i.Format != Format || i.FormatVersion != FormatVersion || i.StoreSchemaVersion != opts.SchemaVersion {
		return ErrIncompatible
	}
	if !i.Complete || i.CreatedAt.IsZero() || i.Payload.ClusterSettings.Owner != SettingsOwner {
		return fmt.Errorf("%w: incomplete image", ErrInvalid)
	}
	if err := i.Term.Validate(); err != nil {
		return err
	}
	if i.Parent != nil {
		if err := i.Parent.Validate(); err != nil {
			return err
		}
		if i.Parent.Term.ClusterID != i.Term.ClusterID || i.Parent.Term.Epoch >= i.Term.Epoch {
			return fmt.Errorf("%w: parent authority", ErrInvalid)
		}
	}
	if i.ObjectResourceVersion > uint64(^uint64(0)>>1) {
		return fmt.Errorf("%w: object counter", ErrInvalid)
	}
	features := map[string]bool{}
	for _, f := range i.RequiredFeatures {
		if features[f] {
			return ErrInvalid
		}
		features[f] = true
	}
	if len(features) != len(FeatureNames()) {
		return ErrIncompatible
	}
	for _, f := range FeatureNames() {
		if !features[f] {
			return ErrIncompatible
		}
	}
	known := map[string]string{}
	for _, k := range opts.Kinds {
		known[k.Kind] = k.APIVersion
	}
	if len(i.KindManifest) != len(known) {
		return fmt.Errorf("%w: incomplete kind manifest", ErrIncompatible)
	}
	manifest := map[string]int{}
	for n, k := range i.KindManifest {
		if n > 0 && i.KindManifest[n-1].Kind >= k.Kind {
			return fmt.Errorf("%w: manifest order", ErrInvalid)
		}
		if _, ok := manifest[k.Kind]; ok {
			return fmt.Errorf("%w: duplicate kind", ErrInvalid)
		}
		if known[k.Kind] != k.APIVersion || k.Count < 0 {
			return ErrIncompatible
		}
		manifest[k.Kind] = k.Count
	}
	if len(i.Payload.Objects) > l.MaxObjects {
		return ErrTooLarge
	}
	counts, keys := map[string]int{}, map[string]bool{}
	lastKey := ""
	for _, raw := range i.Payload.Objects {
		if len(raw) > l.MaxObjectBytes {
			return ErrTooLarge
		}
		key, h, err := objectKey(raw)
		if err != nil {
			return err
		}
		if keys[key] {
			return fmt.Errorf("%w: duplicate object", ErrInvalid)
		}
		if lastKey != "" && key <= lastKey {
			return fmt.Errorf("%w: object order", ErrInvalid)
		}
		lastKey = key
		keys[key] = true
		if known[h.Kind] != h.APIVersion {
			return ErrIncompatible
		}
		rv, err := strconv.ParseUint(h.Metadata.ResourceVersion, 10, 63)
		if err != nil || rv == 0 || rv > i.ObjectResourceVersion || strconv.FormatUint(rv, 10) != h.Metadata.ResourceVersion {
			return fmt.Errorf("%w: object resourceVersion", ErrInvalid)
		}
		counts[h.Kind]++
		if opts.ValidateObject != nil {
			if err := opts.ValidateObject(append(json.RawMessage(nil), raw...)); err != nil {
				return err
			}
		}
	}
	for kind, count := range manifest {
		if counts[kind] != count {
			return fmt.Errorf("%w: manifest count", ErrInvalid)
		}
	}
	entries := 0
	for _, name := range SectionNames() {
		seen := map[string]bool{}
		lastKey := ""
		for _, entry := range *Section(&i.Payload, name) {
			entries++
			if entries > l.MaxEntries || len(entry.Data) > l.MaxObjectBytes || len(entry.Key) > 512 {
				return ErrTooLarge
			}
			if entry.Key == "" || seen[entry.Key] || len(entry.Data) == 0 || bytes.Equal(entry.Data, []byte("null")) {
				return fmt.Errorf("%w: section entry", ErrInvalid)
			}
			seen[entry.Key] = true
			if lastKey != "" && entry.Key <= lastKey {
				return fmt.Errorf("%w: section order", ErrInvalid)
			}
			lastKey = entry.Key
			if opts.ValidateEntry != nil {
				if err := opts.ValidateEntry(name, entry); err != nil {
					return err
				}
			}
		}
	}
	if len(i.PayloadSHA256) != 64 {
		return fmt.Errorf("%w: digest", ErrInvalid)
	}
	digest, err := Digest(i)
	if err != nil {
		return err
	}
	if digest != i.PayloadSHA256 {
		return fmt.Errorf("%w: digest mismatch", ErrInvalid)
	}
	if opts.ValidateImage != nil {
		return opts.ValidateImage(&i)
	}
	return nil
}

// ValidateEntry also serves the local transaction path: a locally accepted
// extension row must not poison the next complete image with ambiguous JSON.
func ValidateEntry(section string, entry Entry, limits Limits) (Entry, error) {
	l := limits.Defaults()
	if Section(&Payload{}, section) == nil || entry.Key == "" {
		return Entry{}, ErrInvalid
	}
	if len(entry.Key) > 512 || len(entry.Data) > l.MaxObjectBytes {
		return Entry{}, ErrTooLarge
	}
	b, err := canonical(entry.Data, l.MaxDepth)
	if err != nil {
		return Entry{}, err
	}
	if bytes.Equal(b, []byte("null")) {
		return Entry{}, ErrInvalid
	}
	entry.Data = b
	return entry, nil
}
