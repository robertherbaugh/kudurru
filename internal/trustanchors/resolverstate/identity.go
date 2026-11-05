package resolverstate

import (
	"crypto/sha256"
	"encoding/binary"
	"strconv"

	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

// Identity is detached metadata for the exact structural owner. A value may
// originate from a proposal: it grants no durable acceptance or runtime permit.
type Identity struct {
	ClusterID  string
	HistoryID  string
	PolicyID   string
	SeedDigest string
	Owner      accepted.Version
}

func identities(id Identity, disposition Disposition) ([32]byte, [32]byte) {
	domain := framedDigest("kudurru/managed-root-anchors/domain/v1", ".", id.ClusterID, id.HistoryID, id.PolicyID, id.SeedDigest)
	owner := framedDigest("kudurru/managed-root-anchors/owner/v1", string(domain[:]),
		strconv.FormatUint(id.Owner.Generation, 10), id.Owner.Digest,
		strconv.FormatUint(id.Owner.Pure.Generation, 10), id.Owner.Pure.Digest,
		strconv.FormatUint(uint64(disposition), 10))
	return domain, owner
}

func framedDigest(tag string, fields ...string) [32]byte {
	// All production fields are bounded by complete-owner validation. Explicit
	// lengths distinguish field boundaries even when text contains delimiters;
	// separate versioned tags prevent domain/owner or future format confusion.
	wire := binary.BigEndian.AppendUint64(nil, uint64(len(tag)))
	wire = append(wire, tag...)
	for _, field := range fields {
		wire = binary.BigEndian.AppendUint64(wire, uint64(len(field)))
		wire = append(wire, field...)
	}
	return sha256.Sum256(wire)
}
