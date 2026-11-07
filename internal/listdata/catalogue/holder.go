package catalogue

import (
	"context"
	"sync"

	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/snapshotowner"
)

// Holder publishes the catalog in service: the catalog document (the
// embedded one or blocking.catalogue.url) merged with the optional AdGuard
// import (blocking.catalogue.adguardServicesURL). Readers load it once
// per query and never lock; the Refreshers swap in newer documents.
type Holder struct {
	current snapshotowner.Owner[installation]
	prepare sync.Mutex // serializes writers; never acquired by a publication pin
	subMu   sync.Mutex
	subs    []chan struct{}
}

// NewHolder returns a Holder serving c; nil means Embedded(). Inputs must be
// parsed immutable documents. An input beyond capture bounds retains its legacy
// pointer but grants no Cut capability; Set can replace it with a bounded value.
func NewHolder(c *Catalogue) *Holder {
	if c == nil {
		c = Embedded()
	}
	h := &Holder{}
	// Preserve legacy pointers while keeping captured data privately detached.
	frozen := copyCatalogue(c)
	_ = h.current.Publish(context.Background(), &installation{base: c, merged: c, frozenBase: frozen, frozenMerged: frozen}, sameInstallation)
	return h
}

// Current returns the catalog in service; never nil.
func (h *Holder) Current() *Catalogue {
	if h == nil {
		return Embedded()
	}
	return h.current.Capture().Value().merged
}

// Base returns the catalog document in service, without the import.
func (h *Holder) Base() *Catalogue {
	if h == nil {
		return Embedded()
	}
	return h.current.Capture().Value().base
}

// Import returns the AdGuard import in service; nil without one.
func (h *Holder) Import() *Import {
	if h == nil {
		return nil
	}
	return h.current.Capture().Value().imp
}

// Set replaces the catalog document and notifies the subscribers. It
// returns false, and changes nothing, when c has the digest of the current
// one or exceeds the capture bounds. Prepare copies and merges before taking
// the installation gate. Do not call Set from inside this holder's pin.
func (h *Holder) Set(c *Catalogue) bool {
	if c == nil {
		return false
	}
	if old := h.current.Capture().Value(); old.base.digest == c.digest {
		return false
	}
	h.prepare.Lock()
	defer h.prepare.Unlock()
	old := h.current.Capture().Value()
	if old.base.digest == c.digest {
		return false
	}
	frozen := copyCatalogue(c)
	if frozen == nil {
		return false
	}
	return h.publish(&installation{base: c, imp: old.imp, merged: merge(c, old.imp), frozenBase: frozen, frozenImport: old.frozenImport, frozenMerged: merge(frozen, old.frozenImport)})
}

// SetImport replaces the AdGuard import (nil removes it) and notifies the
// subscribers. It returns false, and changes nothing, when imp has the
// digest of the current one or exceeds the capture bounds. Do not call
// SetImport from inside this holder's pin.
func (h *Holder) SetImport(imp *Import) bool {
	if sameImport(h.current.Capture().Value().frozenImport, imp) {
		return false
	}
	h.prepare.Lock()
	defer h.prepare.Unlock()
	old := h.current.Capture().Value()
	if sameImport(old.frozenImport, imp) {
		return false
	}
	frozen := copyImport(imp)
	if (imp != nil && frozen == nil) || old.frozenBase == nil {
		return false
	}
	return h.publish(&installation{base: old.base, imp: imp, merged: merge(old.base, imp), frozenBase: old.frozenBase, frozenImport: frozen, frozenMerged: merge(old.frozenBase, frozen)})
}

// publish installs prepared memory, then wakes legacy subscribers AFTER releasing
// the installation gate. No merge, I/O or subscriber callback runs under that gate.
func (h *Holder) publish(next *installation) bool {
	if !boundedCatalogue(next.frozenMerged) {
		return false
	}
	if err := h.current.Publish(context.Background(), next, sameInstallation); err != nil {
		return false // including identity exhaustion: retain the last good state
	}
	h.subMu.Lock()
	defer h.subMu.Unlock()
	for _, ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default: // a wake-up is already pending
		}
	}
	return true
}

// Subscribe returns a channel that receives a value (coalesced) after
// every Set or SetImport that changed the catalog.
func (h *Holder) Subscribe() <-chan struct{} {
	ch := make(chan struct{}, 1)
	h.subMu.Lock()
	h.subs = append(h.subs, ch)
	h.subMu.Unlock()
	return ch
}

// HasService reports whether the current catalogue has service id.
func (h *Holder) HasService(id string) bool { return h.Current().HasService(id) }

// HasCategory reports whether the current catalogue has category id.
func (h *Holder) HasCategory(id string) bool { return h.Current().HasCategory(id) }

// CategoryServices returns the services of category id in the current
// catalogue (Catalogue.CategoryServices).
func (h *Holder) CategoryServices(id string) ([]string, bool) {
	return h.Current().CategoryServices(id)
}

// Lists returns the compile specs of the current catalogue's services and
// a fingerprint that changes with them; the list resolver compiles them
// next to the BlockLists (listfetcher.ExtraLists).
func (h *Holder) Lists() ([]blocking.Spec, string) {
	c := h.Current()
	return c.Specs(), "catalogue:" + c.fingerprint
}
