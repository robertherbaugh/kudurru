package catalogue

import (
	"sync"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/blocking"
)

// Holder publishes the catalogue in service. Readers load it once per
// query and never lock; the Refresher swaps in a newer document.
type Holder struct {
	cur  atomic.Pointer[Catalogue]
	mu   sync.Mutex
	subs []chan struct{}
}

// NewHolder returns a Holder serving c; nil means Embedded().
func NewHolder(c *Catalogue) *Holder {
	if c == nil {
		c = Embedded()
	}
	h := &Holder{}
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

// Set replaces the catalogue in service and notifies the subscribers. It
// returns false, and changes nothing, when c has the digest of the current
// one.
func (h *Holder) Set(c *Catalogue) bool {
	if c == nil {
		return false
	}
	if old := h.cur.Load(); old != nil && old.digest == c.digest {
		return false
	}
	h.cur.Store(c)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default: // a wake-up is already pending
		}
	}
	return true
}

// Subscribe returns a channel that receives a value (coalesced) after
// every Set that changed the catalogue.
func (h *Holder) Subscribe() <-chan struct{} {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs = append(h.subs, ch)
	h.mu.Unlock()
	return ch
}

// HasService reports whether the current catalogue has service id.
func (h *Holder) HasService(id string) bool { return h.Current().HasService(id) }

// Lists returns the compile specs of the current catalogue's services and
// a fingerprint (the document digest) that changes with them; the list
// resolver compiles them next to the BlockLists (listfetcher.ExtraLists).
func (h *Holder) Lists() ([]blocking.Spec, string) {
	c := h.Current()
	return c.Specs(), "catalogue:" + c.digest
}
