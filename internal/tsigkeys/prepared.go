package tsigkeys

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Prepared retains one complete durable source, encryption owner and immutable
// file baseline. It has no live Lookup capability and performs no reader writes.
type Prepared struct {
	source     *Source
	keys       *dnsserver.StaticKeyring
	encryption string
	baseline   *dnsserver.StaticKeyring
}

// Prepare decrypts the entire bounded source before constructing any provider.
// Duplicate file/store names reject the candidate rather than shadowing a key.
func (s *Source) Prepare(sealer *Sealer, baseline *dnsserver.StaticKeyring) (*Prepared, error) {
	if s == nil || sealer == nil || baseline == nil {
		return nil, ErrInvalid
	}
	materials, _, err := s.Open(sealer)
	if err != nil {
		return nil, err
	}
	keys := make([]dnsserver.TSIGKey, 0, len(baseline.Names())+len(materials))
	for _, name := range baseline.Names() {
		secret, alg, ok := baseline.Lookup(name)
		if !ok {
			return nil, ErrInvalid
		}
		// A file baseline must remain independent of stored ownership.
		keys = append(keys, dnsserver.TSIGKey{Name: name, Algorithm: alg, Secret: secret})
	}
	for _, material := range materials {
		keys = append(keys, material.Key())
	}
	ring, err := dnsserver.NewStaticKeyring(keys...)
	if err != nil {
		return nil, ErrInvalid
	}
	return &Prepared{source: s, keys: ring, encryption: sealer.Identity(), baseline: baseline}, nil
}

// Keys returns the original immutable provider, safe for held read requests.
func (p *Prepared) Keys() *dnsserver.StaticKeyring {
	if p == nil {
		return nil
	}
	return p.keys
}

// SourceIdentity names exact durable input rows independently of key bytes.
func (p *Prepared) SourceIdentity() string {
	if p == nil {
		return ""
	}
	return p.source.Identity()
}

// EncryptionIdentity names the immutable encryption resource used to prepare.
func (p *Prepared) EncryptionIdentity() string {
	if p == nil {
		return ""
	}
	return p.encryption
}

// Matches requires the exact source from the candidate complete image.
func (p *Prepared) Matches(ctx context.Context, image *replication.Image) error {
	if p == nil {
		return ErrInvalid
	}
	source, err := FromImage(ctx, image)
	if err != nil {
		return err
	}
	if source.Identity() != p.SourceIdentity() {
		return ErrSource
	}
	return nil
}

// CheckTx excludes durable changes while a coordinator still holds old keys.
// The caller must hold all original writer/credential/dependency guards too.
func (p *Prepared) CheckTx(ctx context.Context, tx *sqlite.CommandTx) error {
	if p == nil {
		return ErrInvalid
	}
	return p.source.CheckTx(ctx, tx)
}
