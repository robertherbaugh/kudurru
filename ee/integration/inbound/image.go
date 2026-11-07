package inbound

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// MaxImageSourceBytes bounds all encoded source carriers, including permanent
// allocations, before typed decoding. The full image retains its protocol caps.
const MaxImageSourceBytes = 64 << 20

// ImageOptions describes the immutable LOCAL full registry and pure validation.
// It must not be inferred from the incoming manifest. Callbacks perform no I/O.
type ImageOptions struct {
	Scheme     *runtime.Scheme
	Validation replication.Validation
}

// DecodedImage contains detached original carriers from one complete image.
// Allocations include permanent reservations not used by a current observation.
// This is evidence, not source admission or a mutation/publication capability.
type DecodedImage struct {
	Base        replication.Generation
	Sources     []SourceImage
	Allocations []*kinds.IntegrationNameAllocation
}

// DecodeImage validates one complete image against the explicit local registry,
// then verifies all source references and immutable allocation evidence. Retained
// heads keep their original chunks; unreferenced chunks are rejected, never GC'd.
// No current desired object is substituted for historical evidence.
func DecodeImage(image *replication.Image, opts ImageOptions) (*DecodedImage, error) {
	if err := sourceImageOptions(&opts); err != nil {
		return nil, err
	}
	if err := sourceImageBounds(image, opts.Validation.Limits); err != nil {
		return nil, err
	}
	out := &DecodedImage{Base: image.Generation(), Sources: []SourceImage{}, Allocations: []*kinds.IntegrationNameAllocation{}}
	chunks := map[string]*kinds.IntegrationSourceChunk{}
	allocations := map[string]*kinds.IntegrationNameAllocation{}
	uids := map[string]bool{}
	owners := map[kinds.Owner]bool{}
	allocationCounts := map[kinds.Owner]int{}
	for _, raw := range image.Payload.Objects {
		var header struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return nil, replication.ErrInvalid
		}
		if !slices.Contains(kinds.SourceKinds, header.Kind) {
			continue
		}
		object, err := decodeSourceCarrier(raw, header.Kind, image, opts.Scheme)
		if err != nil {
			return nil, err
		}
		uid := string(object.GetUID())
		if uids[uid] {
			return nil, invalidSourceImage("duplicate carrier UID")
		}
		uids[uid] = true
		switch v := object.(type) {
		case *kinds.IntegrationSourceHead:
			if owners[v.Spec.Owner] || v.Spec.AcceptedAt.After(image.CreatedAt) || v.Spec.WithdrawnAt.After(image.CreatedAt) {
				return nil, invalidSourceImage("head owner/time")
			}
			owners[v.Spec.Owner] = true
			out.Sources = append(out.Sources, SourceImage{Head: v})
		case *kinds.IntegrationSourceChunk:
			if chunks[v.Name] != nil {
				return nil, invalidSourceImage("duplicate chunk")
			}
			chunks[v.Name] = v
		case *kinds.IntegrationNameAllocation:
			allocationCounts[v.Spec.Owner]++
			if allocations[v.Name] != nil || allocationCounts[v.Spec.Owner] > kinds.MaxSourceAllocations || v.Spec.CreatedAt.After(image.CreatedAt) {
				return nil, invalidSourceImage("allocation owner/time/count")
			}
			allocations[v.Name] = v
			out.Allocations = append(out.Allocations, v)
		}
	}
	used := map[string]bool{}
	claimedUIDs := map[string]kinds.Owner{}
	claimedUIDKeys := map[string]string{}
	for i := range out.Sources {
		s := &out.Sources[i]
		for _, ref := range s.Head.Spec.Chunks {
			chunk := chunks[ref.Name]
			if chunk == nil || used[ref.Name] {
				return nil, invalidSourceImage("missing/shared chunk")
			}
			used[ref.Name] = true
			s.Chunks = append(s.Chunks, chunk)
		}
		doc, err := DecodeSource(*s)
		if err != nil {
			return nil, err
		}
		for _, proof := range doc.Objects {
			if prior := claimedUIDs[proof.UID]; prior.UID != "" && prior != doc.Owner {
				return nil, invalidSourceImage("desired UID claimed by different source owners")
			}
			if key := claimedUIDKeys[proof.UID]; key != "" && key != evidenceKey(proof) {
				return nil, invalidSourceImage("desired UID reused for a different object key")
			}
			// A recreated object key can coexist with retained original evidence,
			// but a different source cannot adopt the original owned incarnation.
			claimedUIDKeys[proof.UID] = evidenceKey(proof)
			claimedUIDs[proof.UID] = doc.Owner
		}
		for _, proof := range doc.Allocations {
			current := allocations[proof.Object.Name]
			if current == nil || string(current.UID) != proof.Object.UID || current.ResourceVersion != proof.Object.ResourceVersion || current.Spec != proof.Spec {
				return nil, invalidSourceImage("allocation evidence differs from original carrier")
			}
		}
	}
	if len(used) != len(chunks) {
		return nil, invalidSourceImage("orphan source chunk")
	}
	// Original source framing was checked before generic canonicalization could
	// repair Unicode or case aliases. Generic validation covers the entire image,
	// all zero-count manifests, ordering, RV ceilings, digest and other owners.
	if _, err := replication.Validate(*image, opts.Validation); err != nil {
		return nil, err
	}
	slices.SortFunc(out.Sources, func(a, b SourceImage) int { return strings.Compare(a.Head.Name, b.Head.Name) })
	slices.SortFunc(out.Allocations, func(a, b *kinds.IntegrationNameAllocation) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func invalidSourceImage(reason string) error {
	return fmt.Errorf("%w: inbound source image: %s", replication.ErrInvalid, reason)
}

func sourceImageOptions(opts *ImageOptions) error {
	if opts.Scheme == nil || opts.Validation.SchemaVersion <= 0 || len(opts.Validation.Kinds) == 0 || len(opts.Validation.Kinds) > 1024 {
		return replication.ErrIncompatible
	}
	known := map[string]string{}
	for _, k := range opts.Validation.Kinds {
		if k.Kind == "" || k.APIVersion == "" || len(k.Kind) > 256 || len(k.APIVersion) > 256 || known[k.Kind] != "" {
			return replication.ErrIncompatible
		}
		known[k.Kind] = k.APIVersion
	}
	for _, kind := range kinds.SourceKinds {
		gvk := public.GroupVersion.WithKind(kind)
		object, err := opts.Scheme.New(gvk)
		if err != nil || known[kind] != public.GroupVersion.String() || !sourceCarrierType(object, kind) {
			return replication.ErrIncompatible
		}
	}
	l := opts.Validation.Limits.Defaults()
	defaults := (replication.Limits{}).Defaults()
	l.MaxBytes = min(l.MaxBytes, defaults.MaxBytes)
	l.MaxObjects = min(l.MaxObjects, defaults.MaxObjects)
	l.MaxObjectBytes = min(l.MaxObjectBytes, defaults.MaxObjectBytes)
	l.MaxEntries = min(l.MaxEntries, defaults.MaxEntries)
	l.MaxDepth = min(l.MaxDepth, defaults.MaxDepth)
	opts.Validation.Limits = l
	return nil
}

// Preflight the whole caller-owned typed envelope before Validate's full marshal
// or any carrier decoding. Aggregate counts/bytes precede typed carrier decode.
func sourceImageBounds(image *replication.Image, limits replication.Limits) error {
	if image == nil || len(image.Payload.Objects) > limits.MaxObjects || len(image.KindManifest) > 1024 || len(image.RequiredFeatures) > 64 {
		return replication.ErrTooLarge
	}
	if len(image.Format) > 256 || len(image.PayloadSHA256) > 256 || len(image.Payload.ClusterSettings.Owner) > 256 || image.Term.Validate() != nil {
		return replication.ErrInvalid
	}
	if image.Parent != nil && image.Parent.Validate() != nil {
		return replication.ErrInvalid
	}
	// Envelope scalars are bounded above; include their worst-case JSON string
	// escaping before replication.Validate allocates its complete encoding.
	total := int64(32<<10 + len(image.KindManifest)*4096 + len(image.RequiredFeatures)*2048)
	for _, k := range image.KindManifest {
		if len(k.Kind) > 256 || len(k.APIVersion) > 256 {
			return replication.ErrTooLarge
		}
	}
	for _, feature := range image.RequiredFeatures {
		if len(feature) > 256 {
			return replication.ErrTooLarge
		}
	}
	for _, raw := range image.Payload.Objects {
		if len(raw) > limits.MaxObjectBytes {
			return replication.ErrTooLarge
		}
		if !validSourceUnicode(raw) {
			return replication.ErrInvalid
		}
		total += sourceRawEncodedBytes(raw) + 1
		if total > limits.MaxBytes {
			return replication.ErrTooLarge
		}
	}
	entries := 0
	for _, name := range replication.SectionNames() {
		rows := *replication.Section(&image.Payload, name)
		if len(rows) > limits.MaxEntries-entries {
			return replication.ErrTooLarge
		}
		entries += len(rows)
		for _, row := range rows {
			if len(row.Data) > limits.MaxObjectBytes || len(row.Key) > 512 {
				return replication.ErrTooLarge
			}
			if !validSourceUnicode(row.Data) {
				return replication.ErrInvalid
			}
			total += sourceRawEncodedBytes(row.Data) + int64(len(row.Key)*6+32)
			if total > limits.MaxBytes {
				return replication.ErrTooLarge
			}
		}
	}
	counts := map[string]int{}
	ownerAllocations := map[kinds.Owner]int{}
	sourceBytes := 0
	for _, raw := range image.Payload.Objects {
		var h struct {
			Kind string `json:"kind"`
			Spec struct {
				Owner kinds.Owner `json:"owner"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(raw, &h); err != nil {
			return replication.ErrInvalid
		}
		if !slices.Contains(kinds.SourceKinds, h.Kind) {
			continue
		}
		if len(raw) > kinds.MaxObjectBytes || len(raw) > MaxImageSourceBytes-sourceBytes {
			return replication.ErrTooLarge
		}
		sourceBytes += len(raw)
		counts[h.Kind]++
		if counts[kinds.KindIntegrationSourceHead] > kinds.MaxSourceCount || counts[kinds.KindIntegrationSourceChunk] > kinds.MaxSourceCount*kinds.MaxSourceChunks || counts[kinds.KindIntegrationNameAllocation] > kinds.MaxClusterAllocations {
			return replication.ErrTooLarge
		}
		if h.Kind == kinds.KindIntegrationNameAllocation {
			ownerAllocations[h.Spec.Owner]++
			if ownerAllocations[h.Spec.Owner] > kinds.MaxSourceAllocations {
				return replication.ErrTooLarge
			}
		}
	}
	return nil
}

// RawMessage marshaling compacts whitespace and escapes literal HTML and line
// separators. Count the un-compacted upper bound so raw byte caps cannot become
// a many-times-larger whole-image allocation during generic validation.
func sourceRawEncodedBytes(raw []byte) int64 {
	size := int64(len(raw))
	for i, b := range raw {
		if b == '<' || b == '>' || b == '&' {
			size += 5
		}
		if b == 0xe2 && i+2 < len(raw) && raw[i+1] == 0x80 && (raw[i+2] == 0xa8 || raw[i+2] == 0xa9) {
			size += 3
		}
	}
	return size
}

func sourceCarrierType(object runtime.Object, kind string) bool {
	switch object.(type) {
	case *kinds.IntegrationSourceHead:
		return kind == kinds.KindIntegrationSourceHead
	case *kinds.IntegrationSourceChunk:
		return kind == kinds.KindIntegrationSourceChunk
	case *kinds.IntegrationNameAllocation:
		return kind == kinds.KindIntegrationNameAllocation
	}
	return false
}

func decodeSourceCarrier(raw json.RawMessage, kind string, image *replication.Image, scheme *runtime.Scheme) (store.Object, error) {
	if !validSourceUnicode(raw) {
		return nil, invalidSourceImage("original carrier Unicode")
	}
	canonical, err := replication.ValidateEntry("runtime", replication.Entry{Key: "source-carrier", Data: raw}, replication.Limits{MaxObjectBytes: kinds.MaxObjectBytes, MaxDepth: 32})
	if err != nil {
		return nil, err
	}
	gvk := schema.GroupVersionKind{Group: public.GroupVersion.Group, Version: public.GroupVersion.Version, Kind: kind}
	value, err := scheme.New(gvk)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return nil, err
	}
	object, ok := value.(store.Object)
	if !ok {
		return nil, replication.ErrIncompatible
	}
	created := object.GetCreationTimestamp()
	if object.GetObjectKind().GroupVersionKind() != gvk || object.GetNamespace() != "" || !boundedText(string(object.GetUID()), 256) || object.GetGeneration() < 1 || created.IsZero() || created.After(image.CreatedAt) {
		return nil, invalidSourceImage("actual carrier identity")
	}
	rv, err := strconv.ParseUint(object.GetResourceVersion(), 10, 63)
	if err != nil || rv == 0 || rv > image.ObjectResourceVersion || strconv.FormatUint(rv, 10) != object.GetResourceVersion() {
		return nil, invalidSourceImage("actual carrier resourceVersion")
	}
	// Bound metadata/spec strings before validators marshal complete objects.
	var owner kinds.Owner
	var validate func() error
	switch v := value.(type) {
	case *kinds.IntegrationSourceHead:
		owner, validate = v.Spec.Owner, v.Validate
		if err := checkValueBounds(v); err != nil {
			return nil, err
		}
	case *kinds.IntegrationSourceChunk:
		owner, validate = v.Spec.Owner, v.Validate
		bounded := *v
		bounded.Spec.Data = nil // bytes have their independent 180KiB wire bound
		if len(v.Spec.Data) > kinds.MaxSourceChunkBytes {
			return nil, replication.ErrTooLarge
		}
		if err := checkValueBounds(bounded); err != nil {
			return nil, err
		}
	case *kinds.IntegrationNameAllocation:
		owner, validate = v.Spec.Owner, v.Validate
		if v.Spec.Scope.ClusterID != image.Term.ClusterID {
			return nil, replication.ErrWrongCluster
		}
		if err := checkValueBounds(v); err != nil {
			return nil, err
		}
	default:
		return nil, replication.ErrIncompatible
	}
	if owner.ClusterID != image.Term.ClusterID {
		return nil, replication.ErrWrongCluster
	}
	if err := validate(); err != nil {
		return nil, err
	}
	typed, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	encoded, err := replication.ValidateEntry("runtime", replication.Entry{Key: "source-carrier", Data: typed}, replication.Limits{MaxObjectBytes: kinds.MaxObjectBytes, MaxDepth: 32})
	if err != nil || !bytes.Equal(canonical.Data, encoded.Data) {
		return nil, invalidSourceImage("ambiguous original carrier fields")
	}
	return object, nil
}
