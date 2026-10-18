package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
)

// status.contentHash is the zone model's canonical hash: ZoneContentHash
// (zoneview.go) builds the zone exactly as every replica's zone cache does
// and takes zone.Zone.ContentHash, sha256 over the served RRs in canonical
// form with the SOA serial zeroed (D12). The first reconcile after the M2
// upgrade from the tuple hash below stamps one new serial per zone, which
// is harmless.
//
// The tuple hash is kept as a stable, miekg-free digest of the winning
// endpoints (zoneView.Records); it no longer feeds status.

// RecordTuple is one served RRset of a zone as the controller sees it: the
// owner name (lower-case, fully qualified), the record type, the external-
// dns set identifier, the effective TTL and the targets in presentation
// format.
type RecordTuple struct {
	Name          string
	Type          string
	SetIdentifier string
	TTL           int64
	Targets       []string
}

// ContentHash returns the hex sha256 over the sorted tuples (the M1
// contentHash, see above). Each tuple is
// one line of Go-quoted fields "name" "type" "setIdentifier" ttl "target"...
// with the targets sorted, so the encoding is unambiguous (TXT targets may
// hold any byte) and the hash does not depend on the order of record sets,
// endpoints or targets. tuples is not modified.
func ContentHash(tuples []RecordTuple) string {
	lines := make([]string, 0, len(tuples))
	for _, t := range tuples {
		targets := slices.Clone(t.Targets)
		slices.Sort(targets)
		var b strings.Builder
		b.WriteString(strconv.Quote(t.Name))
		b.WriteByte(' ')
		b.WriteString(strconv.Quote(t.Type))
		b.WriteByte(' ')
		b.WriteString(strconv.Quote(t.SetIdentifier))
		b.WriteByte(' ')
		b.WriteString(strconv.FormatInt(t.TTL, 10))
		for _, target := range targets {
			b.WriteByte(' ')
			b.WriteString(strconv.Quote(target))
		}
		b.WriteByte('\n')
		lines = append(lines, b.String())
	}
	slices.Sort(lines)
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l))
	}
	return hex.EncodeToString(h.Sum(nil))
}
