package secondary

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// BoundSnapshot contains only acquired copies matching one immutable provider
// and configured cut. Its maps/provenance are private; it is not an UPDATE proof.
type BoundSnapshot struct{ zones map[string]*zone.Zone }

// Lookup returns only the exact configured identity's credential-bound copy.
// Callers retain placeholder coverage at missing data or absolute expiry.
func (s *BoundSnapshot) Lookup(identity string) *zone.Zone {
	if s == nil {
		return nil
	}
	return s.zones[identity]
}

// Bind checks the ACQUIRED fingerprint against this immutable provider without
// store/network/manager lookups. Nil keys is invalid, even for unsigned copies;
// callers may explicitly supply NewStaticKeyring() for an empty provider.
// Equality is the existing durable fingerprint contract, not acquisition epoch
// or ABA proof: restoring identical key bytes can accept an unexpired old copy.
// Root's outer credential-generation cursor fences pending publication.
func (s *Snapshot) Bind(config []*public.DNSZone, keys *dnsserver.StaticKeyring) (*BoundSnapshot, error) {
	if keys == nil || len(config) > 100000 {
		return nil, fmt.Errorf("secondary: missing immutable provider or excessive binding cut")
	}
	out := &BoundSnapshot{zones: make(map[string]*zone.Zone)}
	seen := make(map[string]bool)
	for _, z := range config {
		if z == nil {
			return nil, fmt.Errorf("secondary: nil binding configuration")
		}
		if z.Spec.Type != public.ZoneTypeSecondary {
			continue
		}
		id := zone.SecondaryIdentity(z)
		if seen[id] {
			return nil, fmt.Errorf("secondary: duplicate binding identity")
		}
		seen[id] = true
		if s == nil || z.Spec.Secondary == nil {
			continue
		}
		fingerprint, ok := boundCredential(z.Spec.Secondary.TSIGKeyRef, keys)
		if !ok || s.credentials[id] == "" || s.credentials[id] != fingerprint {
			continue
		}
		model := s.zones[id]
		if model != nil && model.SecondaryIdentity() == id {
			out.zones[id] = model
		}
	}
	return out, nil
}

func boundCredential(ref string, keys *dnsserver.StaticKeyring) (string, bool) {
	if ref == "" {
		return "unsigned", true
	}
	secret, algorithm, ok := keys.Lookup(dnsserver.TSIGKeyName(ref))
	if !ok || len(secret) == 0 {
		return "", false
	}
	if _, ok := dnsserver.TSIGAlgorithm(algorithm); !ok {
		return "", false
	}
	sum := sha256.Sum256(append(append([]byte(algorithm), 0), secret...))
	return hex.EncodeToString(sum[:]), true
}
