package catalogue

import (
	"sync"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/blocking"
)

// Holder publishes the catalogue in service: the catalogue document (the
// embedded one or blocking.catalogue.url) merged with the optional AdGuard
// import (blocking.catalogue.adguardServicesURL). Readers load it once
// per query and never lock; the Refreshers swap in newer documents.
type Holder struct {
	cur  atomic.Pointer[Catalogue]
	mu   sync.Mutex
	base *Catalogue
	imp  *Import
	subs []chan struct{}
}

// NewHolder returns a Holder serving c; nil means Embedded().
func NewHolder(c *Catalogue) *Holder {
	if c == nil {
		c = Embedded()
	}
	h := &Holder{base: c}
	h.cur.Store(c)
	return h
}

// Current returns the catalogue in service; never nil.
func (h *Holder) Current() *Catalogue {
	if h == nil {
		return Embedded()
	}
	return h.cur.Load()
}

// Base returns the catalogue document in service, without the import.
func (h *Holder) Base() *Catalogue {
	if h == nil {
		return Embedded()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.base
}

// Import returns the AdGuard import in service; nil without one.
func (h *Holder) Import() *Import {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.imp
}

// Set replaces the catalogue document and notifies the subscribers. It
// returns false, and changes nothing, when c has the digest of the current
// one.
func (h *Holder) Set(c *Catalogue) bool {
	if c == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.base != nil && h.base.digest == c.digest {
		return false
	}
	h.base = c
	h.publish()
	return true
}

// SetImport replaces the AdGuard import (nil removes it) and notifies the
// subscribers. It returns false, and changes nothing, when imp has the
// digest of the current one.
func (h *Holder) SetImport(imp *Import) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case imp == nil && h.imp == nil:
		return false
	case imp != nil && h.imp != nil && imp.digest == h.imp.digest:
		return false
	}
	h.imp = imp
	h.publish()
	return true
}

// publish stores the merged catalogue and wakes the subscribers; h.mu is
// held.
func (h *Holder) publish() {
	h.cur.Store(merge(h.base, h.imp))
	for _, ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default: // a wake-up is already pending
		}
	}
}

// Subscribe returns a channel that receives a value (coalesced) after
// every Set or SetImport that changed the catalogue.
func (h *Holder) Subscribe() <-chan struct{} {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs = append(h.subs, ch)
	h.mu.Unlock()
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
