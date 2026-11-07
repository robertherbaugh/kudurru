// Package resolverstate projects structurally validated managed root history
// into immutable resolver input. It performs no storage, network, publication,
// or admission. A projection of proposed or decoded history is NOT evidence of
// durable acceptance, and supplies neither a serving permit nor a live resolver.
// The future caller must establish the exact accepted complete-image source and
// independent local retirement, installation, and final-emission prerequisites.
package resolverstate

import (
	"errors"
	"slices"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

// ErrInvalid is a fixed structural refusal that discloses no received records.
var ErrInvalid = errors.New("trustanchors/resolverstate: invalid owner")

// Disposition describes structural input, never permission to serve it.
type Disposition uint8

// Disposition values distinguish invalid input, missing initial keys, and loss
// of accepted keys. No empty set requests shipped or static fallback anchors.
const (
	Invalid Disposition = iota
	AwaitingBootstrap
	Usable
	Quarantined
)

// Projection retains detached, immutable structural input. Only Project creates
// a nonzero value; the zero value is Invalid and has no keys or identity. No
// projection certifies an outer Command, authenticates an image, or admits use.
type Projection struct {
	identity    Identity
	disposition Disposition
	ds          []string
	revoked     []string
	domainID    [32]byte
	ownerID     [32]byte
}

// Project validates the complete owner before selecting any keys. It accepts
// structural proposals as well as decoded history without distinguishing their
// durability; that independent caller obligation cannot be inferred here.
func Project(owner accepted.Snapshot) (Projection, error) {
	if _, err := accepted.EncodeRows(owner); err != nil {
		return Projection{}, ErrInvalid
	}
	root := owner.Inspect()
	p := Projection{identity: Identity{
		ClusterID:  root.ClusterID,
		HistoryID:  root.HistoryID,
		PolicyID:   root.PolicyID,
		SeedDigest: root.SeedDigest,
		Owner:      owner.Version(),
	}}
	switch root.Phase {
	case accepted.Dormant:
		p.disposition = AwaitingBootstrap
	case accepted.Managed:
		state, err := owner.State()
		if err != nil {
			return Projection{}, ErrInvalid
		}
		doc := state.Inspect()
		for _, key := range doc.Keys {
			switch key.Status {
			case trustanchors.Valid, trustanchors.Missing:
				ds, err := effectiveDS(key.DNSKEY)
				if err != nil {
					return Projection{}, ErrInvalid
				}
				p.ds = append(p.ds, ds)
			case trustanchors.Revoked:
				p.revoked = append(p.revoked, key.ID)
			case trustanchors.AddPending:
				// An authenticated candidate still has no resolver trust.
			default:
				return Projection{}, ErrInvalid
			}
		}
		slices.Sort(p.ds)
		p.ds = slices.Compact(p.ds)
		slices.Sort(p.revoked)
		if doc.Quarantined {
			if len(p.ds) != 0 {
				return Projection{}, ErrInvalid
			}
			p.disposition = Quarantined
		} else {
			if len(p.ds) == 0 {
				return Projection{}, ErrInvalid
			}
			p.disposition = Usable
		}
	default:
		return Projection{}, ErrInvalid
	}
	p.domainID, p.ownerID = identities(p.identity, p.disposition)
	return p, nil
}

func effectiveDS(text string) (string, error) {
	// EncodeRows already validates supported algorithms/material, exact key
	// identities, flags, history, and bounds. This projection never repairs a
	// malformed key, strips REVOKE, or skips a failed effective-key conversion.
	rr, err := dns.NewRR(text)
	if err != nil {
		return "", ErrInvalid
	}
	key, ok := rr.(*dns.DNSKEY)
	if !ok || key.Hdr.Name != "." || key.Hdr.Rrtype != dns.TypeDNSKEY || key.Hdr.Class != dns.ClassINET || key.Hdr.Ttl != 0 || key.Protocol != 3 || key.Flags != dns.ZONE|dns.SEP {
		return "", ErrInvalid
	}
	ds := key.ToDS(dns.SHA256)
	if ds == nil {
		return "", ErrInvalid
	}
	ds.Hdr.Ttl = 0
	return ds.String(), nil
}

// Identity returns detached structural metadata, never a commit receipt.
func (p Projection) Identity() Identity { return p.identity }

// Disposition returns Invalid for the zero value. Usable only means that the
// structural history contains keys; it is not a continuing lifetime predicate.
func (p Projection) Disposition() Disposition { return p.disposition }

// DS returns detached canonical SHA-256 DS records from Valid/Missing keys only.
// AwaitingBootstrap, Quarantined, and Invalid have no effective keys; empty must
// never be passed into a static resolver API that substitutes shipped defaults.
func (p Projection) DS() []string { return slices.Clone(p.ds) }

// RevokedIDs returns detached permanent normalized key identities for history
// inspection. These structural values are neither fresh revocation observations
// nor authority to persist local denial without original cryptographic evidence.
func (p Projection) RevokedIDs() []string { return slices.Clone(p.revoked) }

// DomainID binds root trust point and cluster/history/policy/seed. The zero
// projection returns zero; the digest authenticates neither source nor commit.
func (p Projection) DomainID() [32]byte { return p.domainID }

// OwnerID additionally binds the complete envelope and pure versions plus
// disposition. Equal effective keys cannot substitute for this exact identity.
func (p Projection) OwnerID() [32]byte { return p.ownerID }
