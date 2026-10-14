package catalogue

import (
	"bytes"
	"context"
	"slices"

	"github.com/robertherbaugh/kudurru/internal/snapshotowner"
)

// Retain legacy pointers for existing APIs, but captured serving only reads the
// private frozen copies. External mutation of legacy slices cannot relabel or
// change a captured dependency. Inputs must not be mutated while Set copies them;
// Catalogue and Import remain immutable by their public contract.
type installation struct {
	base, merged             *Catalogue
	imp                      *Import
	frozenBase, frozenMerged *Catalogue
	frozenImport             *Import
}

// Sources records actual installed provenance, not requested configuration or
// proof of a fresh fetch. A digest no-op retains the original source. Embedded
// fallback remains explicit when a custom URL supplies identical bytes.
type Sources struct {
	BaseOrigin, BaseDigest     string
	ImportOrigin, ImportDigest string
	HasImport                  bool
}

// Cut retains one exact holder installation without external lifetime hooks.
type Cut struct {
	value snapshotowner.Cut[installation]
}

// Capture reads one atomic installation without locks, clocks or I/O. Nil holders
// grant no capability; use NewHolder(nil) for an explicit embedded installation.
func (h *Holder) Capture() Cut {
	if h == nil {
		return Cut{}
	}
	c := h.current.Capture()
	if s := c.Value(); s == nil || s.frozenMerged == nil {
		return Cut{}
	}
	return Cut{value: c}
}

// Catalogue returns a bounded detached preparation copy of this installation.
// Call once before publication pins, then treat the caller-owned copy as immutable.
// It never borrows legacy pointers, reads newer data or acquires the holder gate.
func (c Cut) Catalogue() *Catalogue {
	if s := c.value.Value(); s != nil {
		return copyCatalogue(s.frozenMerged)
	}
	return nil
}

// Sources returns scalar provenance from the same exact captured installation.
func (c Cut) Sources() Sources {
	s := c.value.Value()
	if s == nil {
		return Sources{}
	}
	out := Sources{BaseOrigin: s.frozenBase.origin, BaseDigest: s.frozenBase.digest}
	if s.frozenImport != nil {
		out.HasImport = true
		out.ImportOrigin, out.ImportDigest = s.frozenImport.origin, s.frozenImport.digest
	}
	return out
}

// Identity names a local installation, not a durable source receipt.
func (c Cut) Identity() string { return c.value.Identity() }

// Current checks exact installation equality in memory, independent of freshness.
// Reinstalling A after B never revives the old A cut.
func (c Cut) Current() bool { return c.value.Current() }

// Changes broadcasts actual replacements without registering legacy subscribers.
// Subscribe before Capture and resubscribe after each close. No-op Sets do not wake.
func (h *Holder) Changes() <-chan struct{} {
	if h == nil {
		return nil
	}
	return h.current.Changes()
}

// WithCurrent excludes installation for the full synchronous callback. It rejects
// nil, wrong-holder, stale and zero cuts. Root owns placement in global lock order;
// no reentry, acquisition, parsing, network or upward callbacks under this pin.
func (h *Holder) WithCurrent(ctx context.Context, expected Cut, fn func() error) error {
	if h == nil {
		return snapshotowner.ErrObsolete
	}
	return h.current.WithCurrent(ctx, expected.value, fn)
}

func sameImport(a, b *Import) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && a.digest == b.digest)
}

func sameInstallation(a, b *installation) bool {
	// Frozen scalar metadata only; never recurse through legacy pointers/indexes.
	return a.frozenBase != nil && b.frozenBase != nil && a.frozenBase.digest == b.frozenBase.digest && sameImport(a.frozenImport, b.frozenImport)
}

// Parsed document caps bound retained data. Merging may double the service count
// and add the "other" category; it does not increase safe-search domains. Check
// shape and aggregate string bytes BEFORE allocating copies, including accidental
// public-slice mutation beyond Parse's bounds. Origin has a separate finite cap.
const (
	maxCaptureOriginBytes = 8192
	// Even an empty JSON string needs two quotes and a separator in a document.
	maxCaptureAddresses = MaxBytes / 3
)

func boundedServices(services []Service, limit int, remaining *int) bool {
	if len(services) > limit {
		return false
	}
	for _, s := range services {
		if len(s.Rules) > maxRules || len(s.Requires) > maxHints || len(s.Related) > maxHints {
			return false
		}
		if !takeStrings(remaining, s.ID, s.Name, s.Category, s.Source, s.Icon, s.Note, s.origin) ||
			!takeStrings(remaining, s.Rules...) || !takeStrings(remaining, s.Requires...) || !takeStrings(remaining, s.Related...) {
			return false
		}
	}
	return true
}

func takeStrings(remaining *int, values ...string) bool {
	for _, s := range values {
		if len(s) > *remaining {
			return false
		}
		*remaining -= len(s)
	}
	return true
}

func copyServices(in []Service) []Service {
	out := slices.Clone(in)
	for i := range out {
		out[i].Rules = slices.Clone(in[i].Rules)
		out[i].Requires = slices.Clone(in[i].Requires)
		out[i].Related = slices.Clone(in[i].Related)
	}
	return out
}

func copyImport(in *Import) *Import {
	if in == nil {
		return nil
	}
	if !boundedImport(in) {
		return nil
	}
	out := *in
	out.Services, out.raw = copyServices(in.Services), bytes.Clone(in.raw)
	return &out
}

func boundedImport(in *Import) bool {
	if in == nil {
		return true
	}
	remaining := MaxAdGuardBytes
	return len(in.raw) <= MaxAdGuardBytes && len(in.origin) <= maxCaptureOriginBytes && len(in.digest) <= 64 && boundedServices(in.Services, maxServices, &remaining)
}

func boundedCatalogue(in *Catalogue) bool {
	if in == nil || len(in.raw) > MaxBytes || len(in.origin) > maxCaptureOriginBytes || len(in.digest) > 64 || len(in.fingerprint) > 64 || len(in.Categories) > maxCategories+1 || len(in.SafeSearch) > maxEngines {
		return false
	}
	remaining := MaxBytes + MaxAdGuardBytes
	if !takeStrings(&remaining, in.Version, in.License, in.Homepage) || !boundedServices(in.Services, 2*maxServices, &remaining) {
		return false
	}
	for _, k := range in.Categories {
		if !takeStrings(&remaining, k.ID, k.Name, k.Description, k.Icon) {
			return false
		}
	}
	domains := 0
	for _, e := range in.SafeSearch {
		if len(e.Domains) > maxDomains-domains {
			return false
		}
		domains += len(e.Domains)
		if len(e.Addresses) > maxCaptureAddresses || len(e.addrs) > maxCaptureAddresses ||
			!takeStrings(&remaining, e.ID, e.Name, e.Target, e.ModerateTarget, e.Source, e.Icon) ||
			!takeStrings(&remaining, e.Addresses...) || !takeStrings(&remaining, e.Domains...) {
			return false
		}
	}
	return boundedImport(in.imported)
}

func copyCatalogue(in *Catalogue) *Catalogue {
	if !boundedCatalogue(in) {
		return nil
	}
	out := *in
	out.raw, out.imported = bytes.Clone(in.raw), copyImport(in.imported)
	out.Categories, out.Services, out.SafeSearch = slices.Clone(in.Categories), copyServices(in.Services), slices.Clone(in.SafeSearch)
	for i := range out.SafeSearch {
		out.SafeSearch[i].Domains = slices.Clone(in.SafeSearch[i].Domains)
		out.SafeSearch[i].Addresses = slices.Clone(in.SafeSearch[i].Addresses)
		out.SafeSearch[i].addrs = slices.Clone(in.SafeSearch[i].addrs)
	}
	// Rebuild maps, membership slices and closure-backed compile sources using
	// detached fields only. Never borrow mutable caches/specs from legacy views.
	out.index()
	return &out
}
