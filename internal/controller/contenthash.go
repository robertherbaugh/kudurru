package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
)

// This file is the single definition of a zone's contentHash in the
// controller. The canonical definition is "sha256 over the sorted canonical
// RRs", and internal/zone owns the canonical RR form; until the controller
// can call it, the hash is computed over the record tuples below. Swapping
// in internal/zone means replacing ContentHash (and RecordTuple's
// canonicalisation in zoneview.go) and nothing else; the first reconcile
// after the swap stamps one new serial per zone, which is harmless.

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

// ContentHash returns the hex sha256 over the sorted tuples. Each tuple is
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
