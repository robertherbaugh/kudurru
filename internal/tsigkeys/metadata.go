package tsigkeys

import (
	"slices"
	"strings"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/tsigkinds/v1alpha1"
)

// Metadata contains only management bindings, never ciphertext or key bytes.
// File entries have no durable UID/resourceVersion and cannot be mutated here.
type Metadata struct {
	KeyName         string    `json:"keyName"`
	Algorithm       string    `json:"algorithm"`
	Source          string    `json:"source"`
	UID             string    `json:"uid,omitempty"`
	ResourceVersion string    `json:"resourceVersion,omitempty"`
	CreatedAt       time.Time `json:"createdAt,omitzero"`
}

func rowMetadata(row *kinds.TSIGKey) Metadata {
	return Metadata{KeyName: row.Spec.KeyName, Algorithm: row.Spec.Algorithm, Source: "store", UID: string(row.UID), ResourceVersion: row.ResourceVersion, CreatedAt: row.CreationTimestamp.Time.UTC().Truncate(time.Second)}
}

// Metadata returns detached values from this ORIGINAL complete source/provider.
// It performs no lookup against a live store, current coordinator or key file.
func (p *Prepared) Metadata() ([]Metadata, error) {
	if p == nil || p.source == nil || p.baseline == nil {
		return nil, ErrInvalid
	}
	out := make([]Metadata, 0, len(p.source.records)+len(p.baseline.Names()))
	for _, name := range p.baseline.Names() {
		identity, ok := p.baseline.Identity(name)
		if !ok {
			return nil, ErrInvalid
		}
		out = append(out, Metadata{KeyName: name, Algorithm: identity.Algorithm, Source: "file"})
	}
	for _, row := range p.source.records {
		out = append(out, rowMetadata(row))
	}
	slices.SortFunc(out, func(a, b Metadata) int { return strings.Compare(a.KeyName, b.KeyName) })
	return out, nil
}

func (s *Source) row(name string) *kinds.TSIGKey {
	if s == nil {
		return nil
	}
	for _, row := range s.records {
		if row.Spec.KeyName == name {
			return row
		}
	}
	return nil
}
