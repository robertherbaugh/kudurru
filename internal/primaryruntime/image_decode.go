package primaryruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type imageRoster struct {
	zones   map[string]*public.DNSZone
	ledgers []*zk.ZoneLifecycle
	heads   map[string]*zonestate.View
}

func imageOptions(o *ImageOptions, image replication.Image) error {
	if o.Scheme == nil || o.ClusterID == "" || len(o.ClusterID) > 256 || strings.ContainsRune(o.ClusterID, 0) || image.Term.ClusterID != o.ClusterID || o.Now.IsZero() || o.Validation.SchemaVersion <= 0 || len(o.Validation.Kinds) == 0 || len(o.Validation.Kinds) > 1024 {
		return ErrInvalid
	}
	if o.MaxZones <= 0 {
		o.MaxZones = 10000
	}
	if o.MaxZones > 100000 {
		return ErrCapacity
	}
	o.Limits = o.Limits.Defaults()
	hard := (zonestate.Limits{}).Defaults()
	if o.Limits.MaxRecords > hard.MaxRecords || o.Limits.MaxWireBytes > hard.MaxWireBytes || o.Limits.MaxAggregateBytes > hard.MaxAggregateBytes || o.Limits.MaxChunks > hard.MaxChunks || o.Limits.MaxReceipts > hard.MaxReceipts {
		return ErrCapacity
	}
	o.Validation.Limits = o.Validation.Limits.Defaults()
	l, hardImage := o.Validation.Limits, (replication.Limits{}).Defaults()
	if l.MaxBytes > hardImage.MaxBytes || l.MaxObjects > hardImage.MaxObjects || l.MaxObjectBytes > hardImage.MaxObjectBytes || l.MaxEntries > hardImage.MaxEntries || l.MaxDepth > hardImage.MaxDepth {
		return ErrCapacity
	}
	known := make(map[string]string, len(o.Validation.Kinds))
	for _, k := range o.Validation.Kinds {
		gv, err := schema.ParseGroupVersion(k.APIVersion)
		if err != nil || k.Kind == "" || len(k.Kind) > 512 || len(k.APIVersion) > 512 || k.Count < 0 || known[k.Kind] != "" {
			return ErrInvalid
		}
		obj, err := o.Scheme.New(gv.WithKind(k.Kind))
		if err != nil {
			return err
		}
		if _, ok := obj.(store.Object); !ok {
			return ErrInvalid
		}
		known[k.Kind] = k.APIVersion
	}
	for _, k := range []string{"DNSZone", zk.KindZoneState, zk.KindZoneChunk, zk.KindZoneLifecycle} {
		if known[k] != public.GroupVersion.String() {
			return fmt.Errorf("%w: required local primary registry", ErrInvalid)
		}
	}
	// Reject finite container/byte overflows BEFORE Validate marshals an image.
	if len(image.KindManifest) > 1024 || len(image.RequiredFeatures) > 64 || len(image.Payload.Objects) > l.MaxObjects {
		return ErrCapacity
	}
	if err := image.Term.Validate(); err != nil {
		return err
	}
	if image.Parent != nil {
		if err := image.Parent.Validate(); err != nil {
			return err
		}
	}
	var size int64
	for _, k := range image.KindManifest {
		if len(k.Kind) > 512 || len(k.APIVersion) > 512 {
			return ErrCapacity
		}
	}
	for _, f := range image.RequiredFeatures {
		if len(f) > 512 {
			return ErrCapacity
		}
	}
	for _, raw := range image.Payload.Objects {
		if len(raw) > l.MaxObjectBytes {
			return ErrCapacity
		}
		size += int64(len(raw))
		if size > l.MaxBytes {
			return ErrCapacity
		}
	}
	count := 0
	for _, name := range replication.SectionNames() {
		section := replication.Section(&image.Payload, name)
		if len(*section) > l.MaxEntries-count {
			return ErrCapacity
		}
		count += len(*section)
		for _, row := range *section {
			if len(row.Key) > 512 || len(row.Data) > l.MaxObjectBytes {
				return ErrCapacity
			}
			size += int64(len(row.Key)) + int64(len(row.Data))
			if size > l.MaxBytes {
				return ErrCapacity
			}
		}
	}
	if len(image.Payload.ClusterSettings.Owner) > 512 || len(image.PayloadSHA256) > 64 {
		return ErrCapacity
	}
	return nil
}

func decodeImage(ctx context.Context, image replication.Image, opts ImageOptions) (imageRoster, error) {
	out := imageRoster{zones: map[string]*public.DNSZone{}, heads: map[string]*zonestate.View{}}
	heads := make([]*zk.ZoneState, 0)
	chunks := make(map[string]*zk.ZoneChunk)
	zonesByApex := make(map[string]bool)
	for _, raw := range image.Payload.Objects {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		var header struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return out, err
		}
		gv, err := schema.ParseGroupVersion(header.APIVersion)
		if err != nil {
			return out, err
		}
		obj, err := opts.Scheme.New(gv.WithKind(header.Kind))
		if err != nil {
			return out, err
		}
		if err := imageStrictJSON(raw, obj); err != nil {
			return out, err
		}
		metadata, ok := obj.(store.Object)
		if !ok || metadata.GetUID() == "" || metadata.GetGeneration() <= 0 || metadata.GetCreationTimestamp().Time.IsZero() {
			return out, fmt.Errorf("%w: image source metadata", ErrInvalid)
		}
		for _, value := range []string{metadata.GetName(), metadata.GetNamespace(), string(metadata.GetUID())} {
			if len(value) > 512 || strings.ContainsRune(value, 0) {
				return out, ErrCapacity
			}
		}
		switch v := obj.(type) {
		case *public.DNSZone:
			apex, err := dynamicupdate.CanonicalName(v.Spec.ZoneName)
			if err != nil || v.UID == "" || v.Namespace != "" || zonesByApex[apex] || out.zones[v.Name] != nil {
				return out, fmt.Errorf("%w: desired zone identity/apex", ErrInvalid)
			}
			if len(out.zones) >= opts.MaxZones {
				return out, ErrCapacity
			}
			zonesByApex[apex] = true
			out.zones[v.Name] = v
		case *zk.ZoneLifecycle:
			if v.Validate() != nil || v.Spec.Zone.ClusterID != opts.ClusterID {
				return out, ErrInvalid
			}
			if len(out.ledgers) >= opts.MaxZones {
				return out, ErrCapacity
			}
			for _, p := range v.Spec.Parents {
				if imageBoundaryID(p, v) != p.BoundaryID {
					return out, fmt.Errorf("%w: parent boundary identity", ErrInvalid)
				}
			}
			out.ledgers = append(out.ledgers, v)
		case *zk.ZoneState:
			if v.Validate() != nil || v.Spec.Zone.ClusterID != opts.ClusterID {
				return out, ErrInvalid
			}
			heads = append(heads, v)
		case *zk.ZoneChunk:
			if v.Validate() != nil {
				return out, ErrInvalid
			}
			chunks[v.Spec.Digest] = v
		}
	}
	var expanded int64
	for _, head := range heads {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		aggregate, err := zonestate.Decode(head, func(d string) (*zk.ZoneChunk, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			chunk := chunks[d]
			if chunk == nil {
				return nil, fmt.Errorf("%w: missing image chunk", ErrInvalid)
			}
			// Count references, including repeats/shared chunks, so expanding all
			// historical aggregates remains bounded by the image byte budget.
			expanded += int64(len(chunk.Spec.Data))
			if expanded > opts.Validation.Limits.MaxBytes {
				return nil, ErrCapacity
			}
			return chunk, nil
		}, opts.Limits)
		if err != nil {
			return out, err
		}
		var config public.DNSZone
		if err := imageStrictJSON(aggregate.Config, &config); err != nil {
			return out, err
		}
		if config.Spec.Type != public.ZoneTypePrimary || config.Name != head.Spec.Zone.ObjectName || string(config.UID) != head.Spec.Zone.UID || canonical(config.Spec.ZoneName) != head.Spec.Zone.Apex {
			return out, fmt.Errorf("%w: committed configuration identity", ErrInvalid)
		}
		if _, err := zone.BuildCommittedUnavailable(&config); err != nil {
			return out, err
		}
		out.heads[head.Name] = &zonestate.View{Head: head, Aggregate: aggregate, Expected: zonestate.Expected{Zone: head.Spec.Zone, ResourceVersion: head.ResourceVersion, CommitSequence: head.Spec.CommitSequence, RootDigest: head.Spec.RootDigest, AuthEpoch: head.Spec.AuthEpoch, Term: image.Term}}
	}
	return out, nil
}

func imageStrictJSON(raw []byte, into any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return ErrInvalid
	}
	return nil
}

func imageBoundaryID(p zk.ParentFence, child *zk.ZoneLifecycle) string {
	b, _ := json.Marshal(struct {
		Parent, Child                       zk.ZoneID
		ParentIncarnation, ChildIncarnation uint64
	}{p.Zone, child.Spec.Zone, p.Incarnation, child.Spec.Incarnation})
	return zk.Digest(b)
}
