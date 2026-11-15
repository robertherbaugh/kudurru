package rpz

import (
	"fmt"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
)

// BoundSnapshot pins selected source identities and acquired credentials to one
// immutable provider. It exposes no keys or credential fingerprints.
type BoundSnapshot struct{ snapshot *Snapshot }

// Select preserves ordered source selection and required unavailable sentinels.
// A configuration absent from Bind cannot borrow another source's runtime.
func (b *BoundSnapshot) Select(lists []*public.BlockList, configurationAvailable bool) *Selection {
	var s *Snapshot
	if b != nil {
		s = b.snapshot
	}
	return s.Select(lists, configurationAvailable)
}

// Bind uses only the supplied immutable provider and this captured snapshot.
// Missing, rotated or metadata-less data stays unavailable, never omitted from
// subsequent required selection. Nil keys is invalid; explicit empty is allowed.
// File inputs preserve the legacy durable fingerprint; stored inputs include
// their credential epoch, so identical bytes cannot revive old data;
// root's outer credential-generation cursor fences pending publication.
func (s *Snapshot) Bind(lists []*public.BlockList, keys *dnsserver.StaticKeyring) (*BoundSnapshot, error) {
	if keys == nil || len(lists) > 100000 {
		return nil, fmt.Errorf("rpz: missing immutable provider or excessive binding cut")
	}
	out := &Snapshot{zones: map[string]*Zone{}}
	seen := map[string]bool{}
	count := 0
	for _, list := range lists {
		if list == nil {
			return nil, fmt.Errorf("rpz: nil binding list")
		}
		for i := range list.Spec.Sources {
			if !configured(list, i) {
				continue
			}
			count++
			if count > 100000 {
				return nil, fmt.Errorf("rpz: excessive binding sources")
			}
			id := SourceIdentity(list, i)
			if seen[id] {
				return nil, fmt.Errorf("rpz: duplicate binding identity")
			}
			seen[id] = true
			_, config, err := sourceConfig(list, i)
			if err != nil || s == nil {
				continue
			}
			fingerprint, ok := boundCredential(config.Spec.Secondary.TSIGKeyRef, keys)
			if !ok || s.credentials[id] == "" || s.credentials[id] != fingerprint {
				continue
			}
			z := s.Lookup(id)
			if z != nil && z.spec.Identity == id {
				out.zones[id] = z
			}
		}
	}
	return &BoundSnapshot{snapshot: out}, nil
}

func boundCredential(ref string, keys *dnsserver.StaticKeyring) (string, bool) {
	if ref == "" {
		return "unsigned", true
	}
	return dnsserver.AcquisitionFingerprint(keys, ref)
}
